#!/usr/bin/env python3
"""Validate a server archive and exercise its real scripts in an isolated root.
Run as root; never uses /data/deployer. Requires a public package with TLS embedded.
"""
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tarfile
import tempfile
import urllib.request

archive = Path(sys.argv[1]).resolve()
assert os.geteuid() == 0, 'Run as root'
expected = {'deployer-server', 'deployer-launcher', 'deployerctl', 'setup.sh', 'start.sh', 'stop.sh', 'remove.sh'}
with tempfile.TemporaryDirectory(prefix='deployer-server-package-') as temporary:
    root = Path(temporary) / 'installed'
    root.mkdir(mode=0o700)
    with tarfile.open(archive) as package:
        members = package.getmembers()
        assert {m.name for m in members} == expected
        assert all(m.isfile() and m.mode & 0o100 for m in members)
        package.extractall(root, filter='data')
    for script in root.glob('*.sh'):
        subprocess.run(['bash', '-n', script], check=True)
    version = json.loads(subprocess.check_output([root / 'deployer-server', '--version']))
    environment = dict(os.environ, DEPLOYER_ROOT=str(root))
    def call(action):
        subprocess.run([str(root / (action + '.sh'))], env=environment, check=True, timeout=45)
    try:
        call('setup')
        configuration = root / 'config/server.env'
        config = configuration.read_text().replace('/data/deployer', str(root))
        # Keep both sockets reserved until selecting distinct isolated ports.
        with socket.socket() as tls, socket.socket() as ui:
            tls.bind(('127.0.0.1', 0)); ui.bind(('127.0.0.1', 0))
            tls_port, ui_port = tls.getsockname()[1], ui.getsockname()[1]
        config = config.replace('DEPLOYER_LISTEN=0.0.0.0:7710', f'DEPLOYER_LISTEN=127.0.0.1:{tls_port}')
        config = config.replace('DEPLOYER_UI_LISTEN=\n', f'DEPLOYER_UI_LISTEN=127.0.0.1:{ui_port}\n')
        configuration.write_text(config)
        call('setup')
        assert configuration.read_text() == config
        call('start')
        identity = (root / 'run/server/child.json').read_bytes()
        call('start')
        assert (root / 'run/server/child.json').read_bytes() == identity
        with urllib.request.urlopen(f'http://127.0.0.1:{ui_port}/healthz', timeout=5) as response:
            assert response.status == 200
        call('stop'); call('stop')
        call('remove'); call('remove')
        assert not (root / 'bin/deployer-server').exists()
        assert (root / 'controller').is_dir(), 'remove must preserve data'
        print(json.dumps({'passed': True, 'version': version['version'], 'top_level_files': sorted(expected), 'idempotent': ['setup', 'start', 'stop', 'remove']}))
    finally:
        call('stop')
