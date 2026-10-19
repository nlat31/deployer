#!/usr/bin/env python3
"""Exercise actual removal logic in a temporary tree with mocked system services."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

BODY = Path(__file__).with_name('remove-runtime.sh').read_text()

class RemoveTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        for directory in ('data/deployer/bin', 'data/deployer/config', 'data/deployer/controller', 'data/deployer/agent/apps', 'etc/systemd/system', 'var/log/journal', 'run/log/journal', 'mock'):
            (self.root / directory).mkdir(parents=True, exist_ok=True)
        self.put('etc/machine-id', '1' * 32)
        for role in ('server', 'agent'):
            self.put(f'data/deployer/bin/deployer-{role}', 'binary')
            self.put(f'etc/systemd/system/deployer-{role}.service', 'unit')
            self.put(f'data/deployer/config/{role}.env', 'config')
        self.put('data/deployer/bin/deployer-agent', '#!/bin/bash\necho stop-apps >> "$REMOVE_TEST_ROOT/commands"\n[[ "${FAIL_APPS:-}" != 1 ]]\n').chmod(0o755)
        self.put('data/deployer/config/ca.pem', 'shared CA')
        self.put('etc/systemd/journald@deployer.conf', 'logs')
        self.put('var/log/journal/' + '1' * 32 + '.deployer/log', 'log')
        self.mock('id', '#!/bin/bash\necho 0\n')
        self.mock('getent', '#!/bin/bash\nexit 2\n')
        self.mock('systemctl', '''#!/bin/bash
printf '%s\\n' "$*" >> "$REMOVE_TEST_ROOT/commands"
if [[ "$1" == show ]]; then
 if [[ "$*" == *WorkingDirectory* ]]; then echo /data/deployer/agent/apps/demo/versions/1; else echo loaded; fi
elif [[ "$1" == list-units ]]; then
 echo 'deployer-0123456789abcdef01234567.service loaded active running'
 echo 'deployer-0123456789abcdef01234567-setup-0123456789ab.service loaded active running'
 echo 'unrelated.service loaded active running'
elif [[ "$1" == stop && "${FAIL_STOP:-}" == 1 ]]; then exit 1
fi
''')
        # Rewrite only filesystem paths, leaving the managed unit directory filter.
        script = BODY
        for prefix in ('/data/deployer', '/etc/systemd', '/etc/machine-id', '/var/log/journal', '/run/log/journal'):
            script = script.replace(prefix, str(self.root) + prefix)
        script = script.replace('case "$directory" in ' + str(self.root), 'case "$directory" in ')
        self.put('remove.sh', script)
        self.env = dict(os.environ, PATH=str(self.root / 'mock') + ':' + os.environ['PATH'], REMOVE_TEST_ROOT=str(self.root))

    def put(self, name, content):
        p = self.root / name
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(content)
        return p

    def mock(self, name, text):
        self.put('mock/' + name, text).chmod(0o755)

    def run_remove(self, role, ok=True):
        p = subprocess.run(['bash', str(self.root / 'remove.sh'), role], env=self.env, capture_output=True, text=True)
        self.assertEqual(p.returncode == 0, ok, p.stdout + p.stderr)

    def test_roles_shared_files_and_repeat(self):
        self.run_remove('server')
        self.assertFalse((self.root / 'data/deployer/controller').exists())
        self.assertTrue((self.root / 'data/deployer/bin/deployer-agent').exists())
        self.assertTrue((self.root / 'data/deployer/config/ca.pem').exists())
        self.run_remove('agent')
        self.assertFalse((self.root / 'data/deployer').exists())
        self.assertFalse((self.root / 'etc/systemd/journald@deployer.conf').exists())
        self.assertIn('stop-apps', (self.root / 'commands').read_text())
        self.assertNotIn('stop unrelated.service', (self.root / 'commands').read_text())
        self.run_remove('all')

    def test_failed_app_stop_preserves_data(self):
        self.env['FAIL_APPS'] = '1'
        self.run_remove('agent', ok=False)
        self.assertTrue((self.root / 'data/deployer/agent/apps').exists())

    def test_failed_stop_preserves_data(self):
        self.env['FAIL_STOP'] = '1'
        self.run_remove('all', ok=False)
        self.assertTrue((self.root / 'data/deployer/bin/deployer-server').exists())
        self.assertTrue((self.root / 'data/deployer/controller').exists())

    def test_refuse_symlink(self):
        config = self.root / 'data/deployer/config'
        config.rename(config.with_name('original'))
        config.symlink_to(config.with_name('original'), target_is_directory=True)
        self.run_remove('all', ok=False)
        self.assertTrue((self.root / 'data/deployer/bin/deployer-server').exists())

if __name__ == '__main__':
    unittest.main()
