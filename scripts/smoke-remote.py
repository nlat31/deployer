#!/usr/bin/env python3
"""Explicit acceptance test of an installed server and Agent over an SSH tunnel.

Creates only sample-app. Refuses an existing sample-app to protect real data.
Requires 17721 -> remote 7710 and 17722 -> remote 18877 forwards by default.
"""
import argparse
import hashlib
import http.cookiejar
import json
import os
from pathlib import Path
import ssl
import subprocess
import time
import urllib.error
import urllib.request
import uuid

p = argparse.ArgumentParser()
p.add_argument('--ssh', required=True)
p.add_argument('--ca', required=True)
p.add_argument('--node', required=True)
p.add_argument('--server', default='https://127.0.0.1:17721')
p.add_argument('--app-url', default='http://127.0.0.1:17722')
p.add_argument('--output', required=True)
p.add_argument('--resume-upload', action='store_true', help='Resume only an undeployed acceptance sample-app after an interrupted upload')
a = p.parse_args()
root = Path(__file__).resolve().parent.parent
context = ssl.create_default_context(cafile=a.ca)
http = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPSHandler(context=context),
    urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
csrf = ''
results = []

def api(method, path, body=None):
    raw = body if isinstance(body, bytes) else json.dumps(body).encode() if body is not None else None
    request = urllib.request.Request(a.server + '/api' + path, data=raw, method=method,
        headers={'Content-Type': 'application/gzip' if isinstance(body, bytes) else 'application/json', 'X-CSRF-Token': csrf})
    with http.open(request, timeout=600 if isinstance(body, bytes) else 30) as response:
        return json.load(response)

def ssh(command):
    return subprocess.check_output(['ssh', '-o', 'BatchMode=yes', a.ssh, command], text=True).strip()

def wait(fn, seconds=100):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        try:
            value = fn()
        except (urllib.error.URLError, ConnectionError, TimeoutError):
            value = None
        if value:
            return value
        time.sleep(1)
    raise TimeoutError('Acceptance condition timed out')

def record(action):
    results.append({'action': action, 'result': 'passed'})
    Path(a.output).write_text(json.dumps(results, ensure_ascii=False, indent=2) + '\n')
    print(action + ': passed', flush=True)

def deployment():
    return next((d for d in api('GET', '/snapshot')['deployments']
        if d['node_id'] == a.node and d['app_id'] == 'sample-app'), {'revision': 0})

health = {'type': 'http', 'target': 'http://127.0.0.1:18877/health', 'timeout_seconds': 8, 'stable_seconds': 1}

def operation(action, version='', expected='succeeded', **options):
    body = {'action': action, 'version': version, 'expected_revision': deployment()['revision'],
        'idempotency_key': str(uuid.uuid4()), 'confirm': True, **options}
    op = api('POST', f'/nodes/{a.node}/apps/sample-app/operations', body)
    final = wait(lambda: (v if (v := api('GET', '/operations/' + op['id'])).get('result') else None))
    assert final['result'] == expected, json.dumps({'operation': final,
        'events': api('GET', '/operations/' + op['id'] + '/events')}, ensure_ascii=False)
    record(f'{action} {version} => {expected}')
    return body, op

def app_version():
    with urllib.request.urlopen(a.app_url, timeout=10) as response:
        return json.load(response)['version']

csrf = api('POST', '/login', {'username': 'admin', 'password': os.environ['DEPLOYER_TEST_PASSWORD']})['csrf']
snapshot = api('GET', '/snapshot')
existing = next((v for v in snapshot['apps'] if v['id'] == 'sample-app'), None)
if existing:
    assert a.resume_upload and existing['name'] == '发布验收示例' and not any(d['app_id'] == 'sample-app' for d in snapshot['deployments']), 'Existing sample-app; refusing to modify it'
wait(lambda: any(n['id'] == a.node and n['last_seen'] > time.time()*1000-15000 for n in api('GET', '/snapshot')['nodes']))
record('mTLS automatic registration')
try:
    with urllib.request.urlopen(urllib.request.Request(a.server + '/agent/poll', data=b'{}'), context=context) as response:
        raise AssertionError('Unauthenticated Agent accepted: ' + str(response.status))
except urllib.error.HTTPError as error:
    assert error.code in (401, 403), error.code
record('reject Agent without certificate')
api('POST', '/apps', {'id': 'sample-app', 'name': '发布验收示例', 'health': health})
for version in ('1.0.0', '1.1.0'):
    api('POST', f'/apps/sample-app/versions?version={version}&arch=amd64',
        (root / 'dist' / f'sample-app-{version}-linux-amd64.tar.gz').read_bytes())
record('upload and validate two versions')
operation('deploy', '1.0.0', env={'PORT': '18877'}, health=health)
assert app_version() == '1.0.0'
def app_pid():
    return str(json.loads(ssh('sudo -n cat /data/deployer/agent/apps/sample-app/versions/1.0.0/run/process.json'))['pid'])
old_pid = app_pid()
ssh('sudo -n kill -KILL ' + old_pid)
wait(lambda: app_pid() != old_pid and deployment()['run_state'] == 'running')
assert app_version() == '1.0.0'
record('application crash recovery')
ssh('sudo -n /data/deployer/lifecycle/agent/stop.sh && sudo -n /data/deployer/lifecycle/agent/start.sh')
wait(lambda: any(n['id'] == a.node and n['last_seen'] > time.time()*1000-3000 for n in api('GET', '/snapshot')['nodes']))
assert app_version() == '1.0.0'
record('Agent restart preserves registration and application')
operation('switch', '1.1.0', expected='failed_recovered', env={'PORT': '18877', 'FAIL_START': '1'}, health=health)
assert deployment()['current'] == app_version() == '1.0.0'
record('failed switch restores previous healthy version')
operation('switch', '1.1.0', env={'PORT': '18877'}, health=health)
assert app_version() == '1.1.0'
operation('switch', '1.0.0', env={'PORT': '18877'}, health=health)
assert app_version() == '1.0.0'
operation('switch', '1.1.0', env={'PORT': '18877'}, health=health)
assert app_version() == '1.1.0'
record('switch versions in both directions')
body, op = operation('logs')
same = api('POST', f'/nodes/{a.node}/apps/sample-app/operations', body)
assert same['id'] == op['id']
record('idempotent request')
operation('stop')
time.sleep(7)
assert deployment()['run_state'] == 'stopped'
record('explicitly stopped app stays stopped')
operation('clean_data', '1.1.0')
assert ssh('sudo -n find /data/deployer/agent/apps/sample-app/versions/1.1.0/data -mindepth 1 -print -quit') == ''
record('data directory cleared')
operation('start')
operation('clean_data', '1.0.0')
operation('remove_version', '1.0.0')
assert ssh('sudo -n test ! -e /data/deployer/agent/apps/sample-app/versions/1.0.0 && echo removed') == 'removed'
record('old version physically removed')
try:
    api('DELETE', '/apps/sample-app/versions/1.1.0?confirm=true')
    raise AssertionError('Deletion of active repository version accepted')
except urllib.error.HTTPError as error:
    assert error.code == 409, error.code
record('active repository version deletion rejected')
api('DELETE', '/apps/sample-app/versions/1.0.0?confirm=true')
record('unused repository version deleted')
ssh('sudo -n /data/deployer/lifecycle/server/stop.sh && sudo -n /data/deployer/lifecycle/server/start.sh')
time.sleep(3)
csrf = api('POST', '/login', {'username': 'admin', 'password': os.environ['DEPLOYER_TEST_PASSWORD']})['csrf']
assert deployment()['current'] == app_version() == '1.1.0'
record('controller restart preserves state and running application')
ssh('sudo -n /data/deployer/lifecycle/agent/stop.sh')
try:
    wait(lambda: next(n for n in api('GET', '/snapshot')['nodes'] if n['id'] == a.node)['last_seen'] < time.time()*1000-32000, 45)
    record('offline node retained')
finally:
    ssh('sudo -n /data/deployer/lifecycle/agent/start.sh')
wait(lambda: any(n['id'] == a.node and n['last_seen'] > time.time()*1000-3000 for n in api('GET', '/snapshot')['nodes']))
operation('stop')
operation('start')
assert app_version() == '1.1.0'
record('Agent reconnects with same identity; final application healthy')
print('PASS: ' + a.output, flush=True)
