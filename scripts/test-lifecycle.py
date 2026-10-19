#!/usr/bin/env python3
"""Real packaged scripts + launcher, isolated installation; run inside unshare -Ur.
No host installation, service manager or privileged account commands are used.
"""
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import tempfile
import time
import unittest

SOURCE = Path(__file__).resolve().parent.parent
HELPER = '''#!/usr/bin/python3
import json, os, pathlib, signal, subprocess, sys, time
if '--version' in sys.argv:
 print('{"version":"fixture"}'); sys.exit(0)
root=pathlib.Path(os.environ['DEPLOYER_DATA'])
root.mkdir(parents=True,exist_ok=True)
if '--stop-apps' in sys.argv:
 if (root/'refuse-stop').exists(): sys.exit(7)
 if (root/'app.pid').exists():
  try: os.kill(int((root/'app.pid').read_text()),signal.SIGTERM)
  except ProcessLookupError: pass
  (root/'app.pid').unlink()
 sys.exit(0)
if not (root/'app.pid').exists():
 app=subprocess.Popen(['/bin/sleep','1000'],start_new_session=True,stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
 (root/'app.pid').write_text(str(app.pid))
dir=pathlib.Path(os.environ['DEPLOYER_RUNTIME_DIR'])
while True:
 beat={'token':os.environ['DEPLOYER_RUN_TOKEN'],'phase':'drained' if (dir/'drain').exists() else 'ready','time':int(time.time()*1000)}
 temp=dir/'helper-ready.tmp'; temp.write_text(json.dumps(beat));temp.replace(dir/'ready.json')
 time.sleep(.1)
'''
class Lifecycle(unittest.TestCase):
 def setUp(self):
  self.tmp=tempfile.TemporaryDirectory(prefix='deployer-lifecycle-');self.addCleanup(self.tmp.cleanup)
  self.path=Path(self.tmp.name);self.root=self.path/'installed';self.package=self.path/'package';self.package.mkdir()
  self.env=dict(os.environ,DEPLOYER_ROOT=str(self.root))
  shutil.copy2(SOURCE/'dist/deployer-launcher',self.package/'deployer-launcher')
  (self.package/'deployer-agent').write_text(HELPER);(self.package/'deployer-agent').chmod(0o755)
  subprocess.run(['python3',str(SOURCE/'scripts/package-lifecycle.py'),'agent','amd64','fixture',str(self.package)],check=True)
  self.call('setup')
  (self.root/'config/agent.env').write_text(f'DEPLOYER_DATA={self.root}/agent\n')
  self.addCleanup(self.cleanup)
 def call(self,action,ok=True):
  p=subprocess.run(['bash',str(self.package/(action+'.sh'))],env=self.env,capture_output=True,text=True,timeout=45)
  self.assertEqual(p.returncode==0,ok,p.stdout+p.stderr)
  return p
 def cleanup(self):
  try:
   self.call('stop')
   (self.root/'agent/refuse-stop').unlink(missing_ok=True)
   self.call('remove')
  except Exception:
   for path in [self.root/'run/agent/child.json',self.root/'run/agent/launcher.json']:
    if path.exists():
     try:os.kill(json.loads(path.read_text())['pid'],signal.SIGKILL)
     except ProcessLookupError:pass
 def test_idempotence_and_business_survival(self):
  before=(self.root/'config/agent.env').read_bytes();self.call('setup');self.assertEqual(before,(self.root/'config/agent.env').read_bytes())
  self.call('start');child=json.loads((self.root/'run/agent/child.json').read_text())
  app=int((self.root/'agent/app.pid').read_text());self.call('start');self.assertEqual(child,json.loads((self.root/'run/agent/child.json').read_text()))
  self.call('stop');self.call('stop');os.kill(app,0)
  self.call('start');self.assertEqual(app,int((self.root/'agent/app.pid').read_text()))
  (self.root/'agent/keep-data').write_text('preserve')
  self.call('remove');self.call('remove');self.assertEqual('preserve',(self.root/'agent/keep-data').read_text());self.assertTrue((self.root/'config/agent.env').exists());self.assertFalse((self.root/'bin/deployer-agent').exists());self.assertFalse((self.root/'agent/app.pid').exists())
  self.call('setup');self.call('start')
 def test_concurrent_start(self):
  commands=[subprocess.Popen(['bash',str(self.package/'start.sh')],env=self.env,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True) for _ in range(3)]
  for p in commands:
   out,err=p.communicate(timeout=30);self.assertEqual(p.returncode,0,out+err)
  child=json.loads((self.root/'run/agent/child.json').read_text())
  self.call('start');self.assertEqual(child,json.loads((self.root/'run/agent/child.json').read_text()))
 def test_failed_app_stop_keeps_deployment(self):
  self.call('start');(self.root/'agent/refuse-stop').touch();self.call('remove',ok=False)
  self.assertTrue((self.root/'bin/deployer-agent').exists());self.assertTrue((self.root/'agent/app.pid').exists())
 def test_trial_crash_rolls_back(self):
  self.call('start');old=json.loads((self.root/'updater/agent.json').read_text())['current'];app=int((self.root/'agent/app.pid').read_text())
  (self.package/'deployer-agent').write_text('#!/bin/bash\nif [[ "${1:-}" == --version ]]; then echo fixture; exit 0; fi\nexit 9\n')
  self.call('setup',ok=False)
  state=json.loads((self.root/'updater/agent.json').read_text());self.assertEqual(state['phase'],'failed');self.assertEqual(old,state['current']);os.kill(app,0)
  deadline=time.time()+10
  while time.time()<deadline:
   if (self.root/'run/agent/ready.json').exists():break
   time.sleep(.1)
  self.call('start')
 def test_symlink_refused(self):
  config=self.root/'config';config.rename(self.root/'original');config.symlink_to(self.root/'original',target_is_directory=True)
  self.call('setup',ok=False);self.call('remove',ok=False)
  config.unlink();(self.root/'original').rename(config)

if __name__=='__main__':
 if os.geteuid()!=0:raise SystemExit('Run in a root user namespace: unshare -Ur python3 scripts/test-lifecycle.py')
 unittest.main()
