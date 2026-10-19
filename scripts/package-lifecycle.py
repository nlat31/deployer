#!/usr/bin/env python3
"""Emit standalone lifecycle scripts; templates and examples stay in source."""
from pathlib import Path
import sys
import os

root = Path(__file__).resolve().parent.parent
role, arch, version, destination = sys.argv[1:]
assert role in ('server', 'agent') and arch in ('amd64', 'arm64')
destination = Path(destination)
header = f'#!/usr/bin/env bash\n# Deployer {role} {version} linux/{arch}\nset -euo pipefail\nrole={role}\n'
config = (root / 'configs' / f'{role}.env.example').read_text()
pki = os.environ.get('DEPLOYER_RELEASE_PKI')
if pki and role == 'server':
    config = config.replace('DEPLOYER_LISTEN=127.0.0.1:7710', 'DEPLOYER_LISTEN=0.0.0.0:7710')
setup = header + f'package_arch={arch}\nsource_dir="$(cd -- "$(dirname -- "$0")" && pwd)"\n'
setup += "default_environment() { cat <<'DEPLOYER_DEFAULT_ENV'\n" + config + 'DEPLOYER_DEFAULT_ENV\n}\n'
setup += (root / 'scripts/setup-runtime.sh').read_text()
if pki:
    pki = Path(pki).resolve()
    if role == 'server':
        identity = pki / 'server'
        cert_name, key_name, owner = 'server-cert.pem', 'server-key.pem', 'root'
        identities = [(cert_name, identity / 'cert.pem'), (key_name, identity / 'key.pem')]
    else:
        owner = 'root'
        identities = []  # Each host generates its own private key on first start.
    for name, source in [('ca.pem', pki / 'ca.pem')] + identities:
        pem = source.read_text()
        if not pem.endswith('\n'):
            pem += '\n'
        delimiter = 'DEPLOYER_' + name.upper().replace('-', '_').replace('.', '_')
        file_owner = 'root' if name == 'ca.pem' else owner
        mode = '0644' if name == 'ca.pem' else '0600'
        setup += f'''
if [[ ! -e "$root/config/{name}" ]]; then
  (umask 077; set -o noclobber; cat > "$root/config/{name}" <<'{delimiter}'
{pem}{delimiter}
  )
  chown {file_owner}:{file_owner} "$root/config/{name}"
  chmod {mode} "$root/config/{name}"
fi
'''
update_key = os.environ.get('DEPLOYER_UPDATE_PUBLIC_KEY')
if update_key:
    public = Path(update_key).read_text().strip()
    import base64
    assert len(base64.b64decode(public, validate=True)) == 32
    setup += f'\nif [[ ! -e "$root/config/update-public.key" ]]; then\n  (umask 077; set -o noclobber; printf "%s\\n" "{public}" > "$root/config/update-public.key")\nfi\n'
common = header + '\nroot="${DEPLOYER_ROOT:-/data/deployer}"\n'
start = common + '\nexec "$root/bin/deployer-launcher" start --role "$role" --root "$root"\n'
stop = common + '\n[[ "$(id -u)" == 0 ]] || exit 1\n[[ -e "$root" ]] || exit 0\nexec "$root/bin/deployer-launcher" stop --role "$role" --root "$root"\n'
for name, text in [('setup.sh', setup), ('start.sh', start), ('stop.sh', stop), ('remove.sh', header + (root / 'scripts/remove-runtime.sh').read_text())]:
    path = destination / name
    path.write_text(text)
    path.chmod(0o700 if name == "setup.sh" and pki and role == "server" else 0o755)
