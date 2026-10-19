#!/usr/bin/env python3
"""Exercise real binaries and direct root scripts with an isolated local controller."""
import argparse
import hashlib
import http.cookiejar
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import time
import urllib.parse
import urllib.request
import uuid

parser = argparse.ArgumentParser()
parser.add_argument('--server', default='http://127.0.0.1:17710')
parser.add_argument('--agent-server', default='https://127.0.0.1:17711')
parser.add_argument('--pki', required=True, help='Explicit test-only issuing CA directory')
parser.add_argument('--keep-agent', action='store_true')
args = parser.parse_args()
if os.geteuid() != 0:
    raise SystemExit("Run the direct-script acceptance as root")
if any(urllib.parse.urlparse(url).hostname not in ('127.0.0.1', '::1') for url in (args.server,args.agent_server)):
    raise SystemExit('Only an explicit loopback test controller is allowed')
root = Path(__file__).resolve().parent.parent
cache = root / '.cache'
cache.mkdir(exist_ok=True)
data = Path(tempfile.mkdtemp(prefix='direct-e2e-', dir=cache))
node_name = 'direct 验收节点-' + data.name[-6:]
password = os.environ['DEPLOYER_TEST_PASSWORD']
http = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
csrf = ''

def api(method, path, body=None):
    raw = body if isinstance(body, bytes) else json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(args.server + '/api' + path, data=raw, method=method,
                                 headers={'Content-Type': 'application/json', 'X-CSRF-Token': csrf})
    with http.open(req, timeout=30) as response:
        return json.load(response)

csrf = api('POST', '/login', {'username': 'admin', 'password': password})['csrf']
with socket.socket() as sock:
    sock.bind(('127.0.0.1', 0))
    port = sock.getsockname()[1]
health = {'type': 'http', 'target': f'http://127.0.0.1:{port}/health', 'timeout_seconds': 8, 'stable_seconds': 1}
api('POST', '/apps', {'id': 'sample-app', 'name': '示例应用', 'description': '真实 direct 生命周期验收', 'health': health})
for version in ('1.0.0', '1.1.0'):
    package = root / 'dist' / f'sample-app-{version}-linux-amd64.tar.gz'
    payload = package.read_bytes()
    existing = next((v for v in api('GET', '/snapshot')['versions']
                     if v['app_id'] == 'sample-app' and v['version'] == version), None)
    if existing:
        assert existing['digest'] == hashlib.sha256(payload).hexdigest(), 'Existing test package differs'
    else:
        api('POST', f'/apps/sample-app/versions?version={version}&arch=amd64', payload)
pki = Path(args.pki).resolve()
node_id = 'test-' + data.name
certificate_dir = data / 'client-certificate'
subprocess.run(['bash', str(root / 'scripts/pki.sh'), 'node', str(pki), node_id, str(certificate_dir)], check=True)
log = (data / 'agent.log').open('w')
agent = subprocess.Popen([str(root / 'dist/deployer-agent'), '--server', args.agent_server,
                          '--data', str(data), '--name', node_name,
                          '--ca', str(pki / 'ca.pem'), '--cert', str(certificate_dir / 'cert.pem'),
                          '--key', str(certificate_dir / 'key.pem')], stdout=log, stderr=log)
node = None
results = []

def wait_for(fn, seconds=45):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        if agent.poll() is not None:
            raise RuntimeError((data / 'agent.log').read_text())
        result = fn()
        if result:
            return result
        time.sleep(.3)
    raise TimeoutError('Timed out waiting for the test Agent')

def deployment():
    return next((d for d in api('GET', '/snapshot')['deployments']
                 if d['node_id'] == node['id'] and d['app_id'] == 'sample-app'), {'revision': 0})

def operation(action, version='', expected='succeeded', **options):
    op = api('POST', f"/nodes/{node['id']}/apps/sample-app/operations", {
        'action': action, 'version': version, 'expected_revision': deployment()['revision'],
        'idempotency_key': str(uuid.uuid4()), 'confirm': True, **options})
    def completed():
        value = api('GET', '/operations/' + op['id'])
        return value if value['result'] else None
    final = wait_for(completed, 90)
    if final['result'] != expected:
        events = api('GET', f"/operations/{op['id']}/events")
        raise AssertionError(json.dumps({'operation': final, 'events': events}, ensure_ascii=False, indent=2))
    results.append({'action': action, 'version': version, 'result': final['result']})
    print(f'{action} {version}: {final["result"]}', flush=True)
    return final

try:
    node = wait_for(lambda: next((n for n in api('GET', '/snapshot')['nodes']
                                 if n['name'] == node_name and n['last_seen'] > int(time.time()*1000)-5000), None))
    operation('deploy', '1.0.0', env={'PORT': str(port), 'MESSAGE': 'Version one'}, health=health)
    with urllib.request.urlopen(f'http://127.0.0.1:{port}/') as response:
        assert json.load(response)['version'] == '1.0.0'
    # Exercise crash recovery through the real direct manager.
    def main_pid():
        record = data / 'apps/sample-app/versions/1.0.0/run/process.json'
        return str(json.loads(record.read_text())['pid'])
    old_pid = main_pid()
    os.kill(int(old_pid), 9)
    wait_for(lambda: main_pid() not in ('0', old_pid) and deployment().get('run_state') == 'running')
    print('crash: automatically recovered', flush=True)
    results.append({'action': 'crash_recovery', 'result': 'succeeded'})
    # Certificate identity and running applications must survive Agent restart.
    agent.terminate()
    agent.wait(timeout=30)
    agent = subprocess.Popen(agent.args, stdout=log, stderr=log)
    wait_for(lambda: any(n['id'] == node['id'] and n['last_seen'] > int(time.time()*1000)-3000
                        for n in api('GET', '/snapshot')['nodes']))
    print('agent restart: registration and running App preserved', flush=True)
    operation('switch', '1.1.0', expected='failed_recovered', env={'PORT': str(port), 'FAIL_START': '1'}, health=health)
    assert deployment()['current'] == '1.0.0'
    operation('switch', '1.1.0', env={'PORT': str(port), 'MESSAGE': 'Version two'}, health=health)
    operation('logs')
    operation('stop')
    time.sleep(6)
    assert deployment()['run_state'] == 'stopped', 'stopped App was automatically restarted'
    print('explicit stop: remains stopped after observer cycle', flush=True)
    operation('clean_data', '1.1.0')
    operation('start')
    operation('clear_rollback')
    operation('clean_data', '1.0.0')
    operation('remove_version', '1.0.0')
    operation('stop')
    (data / 'results.json').write_text(json.dumps(results, ensure_ascii=False, indent=2))
    print(f'PASS: real controller + Agent + direct. Results: {data}/results.json', flush=True)
    if args.keep_agent:
        print('Keeping the stopped test node online for UI inspection; interrupt to clean up.', flush=True)
        while True:
            time.sleep(1)
except KeyboardInterrupt:
    pass
finally:
    agent.terminate()
    try:
        agent.wait(timeout=30)
    except subprocess.TimeoutExpired:
        agent.kill()
        agent.wait()
    subprocess.run([str(root / 'dist/deployer-agent'), '--stop-apps', '--data', str(data)], check=True)
    log.close()
