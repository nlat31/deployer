#!/usr/bin/env python3
"""End-to-end mTLS signed Agent update + rollback with real application PID retention.
Requires namespace root. All keys, releases, databases and processes are temporary.
"""
import hashlib
import http.cookiejar
import io
import json
import os
from pathlib import Path
import shutil
import socket
import ssl
import subprocess
import tarfile
import tempfile
import time
import urllib.request
import uuid

SOURCE=Path(__file__).resolve().parent.parent
if os.geteuid()!=0:raise SystemExit('Run with unshare -Ur')

def wait(fn,seconds=210):
 deadline=time.monotonic()+seconds
 while time.monotonic()<deadline:
  value=fn()
  if value:return value
  time.sleep(.25)
 raise TimeoutError('condition timed out')
def port():
 with socket.socket() as s:s.bind(('127.0.0.1',0));return s.getsockname()[1]

with tempfile.TemporaryDirectory(prefix='deployer-update-e2e-') as temporary:
 root=Path(temporary);installed=root/'installed';pki=root/'pki'
 env=dict(os.environ,GOCACHE=str(SOURCE/'.cache/go-build'))
 def command(args,**kw):return subprocess.run([str(x) for x in args],check=True,env=env,**kw)
 def build(name,destination,version='fixture'):
  command(['go','build','-buildvcs=false','-ldflags',f'-X deployer/internal/selfupdate.Version={version}','-o',destination,'./cmd/'+name],cwd=SOURCE)
 launcher=root/'launcher';ctl=root/'ctl';build('deployer-launcher',launcher);build('deployerctl',ctl)
 packages={}
 for role in ['server','agent']:
  package=root/('package-'+role);package.mkdir();packages[role]=package
  shutil.copy2(launcher,package/'deployer-launcher');build('deployer-'+role,package/('deployer-'+role),'initial')
  command(['python3',SOURCE/'scripts/package-lifecycle.py',role,'amd64','initial',package])
 # Pass installation root only to fixture scripts.
 env['DEPLOYER_ROOT']=str(installed)
 for package in packages.values():command(['bash',package/'setup.sh'],stdout=subprocess.DEVNULL)
 command(['bash',SOURCE/'scripts/pki.sh','init',pki,'127.0.0.1'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
 command(['bash',SOURCE/'scripts/pki.sh','node',pki,'fixture-node'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
 command([ctl,'update-keygen','--key',root/'signing.key','--public',installed/'config/update-public.key'])
 listen=port();base=f'https://127.0.0.1:{listen}'
 (installed/'config/server.env').write_text(f'DEPLOYER_LISTEN=127.0.0.1:{listen}\nDEPLOYER_DATA={installed}/controller\nDEPLOYER_TLS_CERT={pki}/server/cert.pem\nDEPLOYER_TLS_KEY={pki}/server/key.pem\nDEPLOYER_CLIENT_CA={pki}/ca.pem\nDEPLOYER_UPDATE_PUBLIC_KEY={installed}/config/update-public.key\n')
 (installed/'config/agent.env').write_text(f'DEPLOYER_SERVER={base}\nDEPLOYER_DATA={installed}/agent\nDEPLOYER_CA={pki}/ca.pem\nDEPLOYER_CLIENT_CERT={pki}/nodes/fixture-node/cert.pem\nDEPLOYER_CLIENT_KEY={pki}/nodes/fixture-node/key.pem\n')
 http=urllib.request.build_opener(urllib.request.HTTPSHandler(context=ssl.create_default_context(cafile=str(pki/'ca.pem'))),urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
 csrf=''
 def api(method,path,body=None,content='application/json'):
  raw=body if isinstance(body,bytes) else json.dumps(body).encode() if body is not None else None
  req=urllib.request.Request(base+'/api'+path,data=raw,method=method,headers={'Content-Type':content,'X-CSRF-Token':csrf})
  with http.open(req,timeout=30) as response:return json.load(response)
 def node():return next((n for n in api('GET','/snapshot')['nodes'] if n['id']=='fixture-node'),None)
 try:
  for role in ['server','agent']:command(['bash',packages[role]/'start.sh'],stdout=subprocess.DEVNULL)
  csrf=api('POST','/login',{'username':'admin','password':'xScan!@&'})['csrf'];wait(node)
  health={'type':'process','timeout_seconds':5,'stable_seconds':1}
  api('POST','/apps',{'id':'fixture','name':'Fixture','health':health})
  raw=io.BytesIO()
  with tarfile.open(fileobj=raw,mode='w:gz') as tar:
   for name,body in {'setup.sh':'echo preserved > "$APP_DATA_DIR/marker"','start.sh':'exec sleep 1000','stop.sh':'kill -TERM "$APP_MAIN_PID" 2>/dev/null || true','clean.sh':':'}.items():
    data=('#!/bin/bash\n'+body+'\n').encode();info=tarfile.TarInfo(name);info.size=len(data);info.mode=0o700;tar.addfile(info,io.BytesIO(data))
  api('POST','/apps/fixture/versions?version=1&arch=amd64',raw.getvalue(),'application/gzip')
  op=api('POST','/nodes/fixture-node/apps/fixture/operations',{'action':'deploy','version':'1','expected_revision':0,'idempotency_key':str(uuid.uuid4()),'confirm':True,'health':health})
  result=wait(lambda: (x if (x:=api('GET','/operations/'+op['id'])).get('result') else None),45);assert result['result']=='succeeded',result
  record=installed/'agent/apps/fixture/versions/1/run/process.json';before=json.loads(record.read_text());old_agent=json.loads((installed/'run/agent/child.json').read_text())
  def publish(binary,version):
   manifest=root/(version+'.json');command([ctl,'sign-agent','--key',root/'signing.key','--version',version,'--binary',binary,'--out',manifest])
   boundary=uuid.uuid4().hex
   body=(f'--{boundary}\r\nContent-Disposition: form-data; name="manifest"\r\n\r\n'.encode()+manifest.read_bytes()+f'\r\n--{boundary}\r\nContent-Disposition: form-data; name="binary"; filename="agent"\r\nContent-Type: application/octet-stream\r\n\r\n'.encode()+binary.read_bytes()+f'\r\n--{boundary}--\r\n'.encode())
   release=api('POST','/agent-releases',body,'multipart/form-data; boundary='+boundary)
   api('POST','/agent-rollout',{'digest':release['digest'],'nodes':['fixture-node']});return release['digest']
  good=root/'agent-new';build('deployer-agent',good,'updated');digest=publish(good,'updated')
  wait(lambda: (n:=node()) and n['agent_update']['digest']==digest and n['agent_update']['phase']=='committed')
  assert node()['agent_update']['version']=='updated';assert before==json.loads(record.read_text());assert old_agent!=json.loads((installed/'run/agent/child.json').read_text());os.kill(before['pid'],0)
  print('PASS: signed mTLS update committed; real application identity unchanged',flush=True)
  api('POST','/agent-rollout',{'paused':True})
  bad=root/'agent-broken';bad.write_text('#!/bin/sh\nexit 42\n');bad.chmod(0o700);bad_digest=publish(bad,'broken')
  wait(lambda:(n:=node()) and n['agent_update']['phase']=='failed' and n['agent_update']['target']==bad_digest,120)
  assert node()['agent_update']['digest']==digest;assert api('GET','/agent-rollout')['paused'];assert before==json.loads(record.read_text());os.kill(before['pid'],0)
  command(['bash',packages['agent']/'stop.sh']);os.kill(before['pid'],0)
  command(['bash',packages['agent']/'start.sh']);assert before==json.loads(record.read_text())
  print('PASS: bad signed version rolled back; rollout paused; stop/start preserved application',flush=True)
 except Exception:
  for role in ['agent','server']:
   for path in [installed/'updater'/(role+'.json'),installed/'logs'/(role+'.log')]:
    if path.exists():print(str(path)+':\n'+path.read_text()[-6000:],flush=True)
  raise
 finally:
  for role in ['agent','server']:
   try:command(['bash',packages[role]/'remove.sh'],timeout=45,stdout=subprocess.DEVNULL)
   except Exception:
    for name in ['child','launcher']:
     path=installed/'run'/role/(name+'.json')
     if path.exists():
      try:os.kill(json.loads(path.read_text())['pid'],9)
      except ProcessLookupError:pass
    raise
