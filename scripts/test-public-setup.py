#!/usr/bin/env python3
"""Validate packaged Agent bootstrap without touching host installation paths."""
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile

archive = Path(sys.argv[1]).resolve()
with tempfile.TemporaryDirectory() as temporary:
    root = Path(temporary)
    source = root / 'package'
    source.mkdir()
    with tarfile.open(archive) as tar:
        members = [m for m in tar.getmembers() if m.isfile()]
        assert {Path(m.name).name for m in members} == {'deployer-agent', 'setup.sh', 'start.sh', 'stop.sh', 'remove.sh'}
        for member in members:
            path = source / Path(member.name).name
            path.write_bytes(tar.extractfile(member).read())
            path.chmod(member.mode)
            if path.suffix == '.sh':
                subprocess.run(['bash', '-n', str(path)], check=True)
    # Keep actual binary preflight, install, flock and filesystem writes.
    # Only privileged account/service management is mocked.
    mock = root / 'mock'
    mock.mkdir()
    for command, script in {
        'id': 'echo 0',
        'chown': 'exit 0',
        'systemctl': 'if [[ "$1" == --version ]]; then echo "systemd 255"; fi',
    }.items():
        path = mock / command
        path.write_text('#!/bin/bash\n' + script + '\n')
        path.chmod(0o755)
    (root / 'etc/systemd/system').mkdir(parents=True)
    script = (source / 'setup.sh').read_text()
    for prefix in ('/data/deployer', '/etc/systemd'):
        script = script.replace(prefix, str(root) + prefix)
    (source / 'setup.sh').write_text(script)
    env = dict(os.environ, PATH=str(mock) + ':' + os.environ['PATH'])
    subprocess.run(['bash', str(source / 'setup.sh')], env=env, check=True, stdout=subprocess.DEVNULL)
    config = root / 'data/deployer/config'
    assert 'DEPLOYER_SERVER=https://18.138.124.233:7710' in (config / 'agent.env').read_text()
    for name in ('client-key.pem', 'client-cert.pem'):
        assert not (config / name).exists(), 'Universal package must not contain host identity'
    assert (config / 'ca.pem').stat().st_mode & 0o777 == 0o644
    assert 'PRIVATE KEY' not in script, 'Universal Agent cannot contain shared keys'
    before = {p.name: p.read_bytes() for p in config.iterdir()}
    subprocess.run(['bash', str(source / 'setup.sh')], env=env, check=True, stdout=subprocess.DEVNULL)
    assert before == {p.name: p.read_bytes() for p in config.iterdir()}
    print('PASS: fresh setup installs public defaults and CA without shared identity; repeated setup preserves config and identity')
