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
        cert_name, key_name, owner = 'server-cert.pem', 'server-key.pem', 'deployer-control'
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
if [[ ! -e /data/deployer/config/{name} ]]; then
  (umask 077; set -o noclobber; cat > /data/deployer/config/{name} <<'{delimiter}'
{pem}{delimiter}
  )
  chown {file_owner}:{file_owner} /data/deployer/config/{name}
  chmod {mode} /data/deployer/config/{name}
fi
'''
common = header + '''[[ "$(id -u)" == 0 ]] || { echo 'Run this script as root (sudo).' >&2; exit 1; }
unit="deployer-$role.service"
'''
start = common + '''[[ -f "/data/deployer/config/$role.env" && -x "/data/deployer/bin/deployer-$role" ]] || { echo 'Run setup.sh and configure certificates first.' >&2; exit 1; }
systemctl enable "$unit"
pid="$(systemctl show "$unit" --property=MainPID --value)"
if systemctl is-active --quiet "$unit"; then
  if [[ ! "/proc/$pid/exe" -ef "/data/deployer/bin/deployer-$role" ]]; then systemctl restart "$unit"; fi
else
  systemctl start "$unit"
fi
systemctl is-active "$unit"
'''
stop = common + '''if [[ "$(systemctl show "$unit" --property=LoadState --value)" != not-found ]]; then
  systemctl stop "$unit"
fi
'''
for name, text in [('setup.sh', setup), ('start.sh', start), ('stop.sh', stop), ('remove.sh', header + (root / 'scripts/remove-runtime.sh').read_text())]:
    path = destination / name
    path.write_text(text)
    path.chmod(0o700 if name == "setup.sh" and pki and role == "server" else 0o755)
