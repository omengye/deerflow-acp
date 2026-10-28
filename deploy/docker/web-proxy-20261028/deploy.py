"""Back up and minimally patch the three native network tools' proxy fields.

Credentials stay on Gentoo. Native validation is used without rewriting the
full settings document, and only DeerFlow is restarted while its daemon is idle.
"""
import datetime
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import time
import uuid

ROOT = Path('/home/gen/deerflow-deploy')
STAGE = ROOT / 'web-proxy-20261028'
FOLO = Path('/home/gen/Documents/folotoy/deerflow-ws/src')
CONFIG = ROOT / 'data/user-data/config/config.yaml'
DEER = 'deerflow-deploy-deerflow-1'
TOY = 'folotoy-ws-folotoy-1'
PROXY = 'http://172.17.0.1:11055'
TOOLS = ('web_search', 'web_fetch', 'image_search')


def run(command, data=None, timeout=60):
    result = subprocess.run(command, input=data, text=True, capture_output=True, timeout=timeout)
    if result.returncode:
        raise RuntimeError('proxy rollout operation failed: ' + command[0])
    return result.stdout


def digest(data):
    return hashlib.sha256(data).hexdigest()


def atomic(path, data, mode):
    temporary = path.with_name('.' + path.name + '.proxy-' + uuid.uuid4().hex)
    try:
        temporary.write_bytes(data)
        temporary.chmod(mode)
        with temporary.open('rb') as stream:
            os.fsync(stream.fileno())
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


def native(operation, document):
    result = json.loads(run(['docker', 'exec', '-i', DEER, '/var/lib/deerflow/deerflow-config-go',
                             '--config', '/var/lib/deerflow/user-data/config/config.yaml',
                             '--user-data', '/var/lib/deerflow/user-data',
                             '--resources', '/var/lib/deerflow/resources', operation], json.dumps(document)))
    if not result.get('ok'):
        raise RuntimeError('native config rejected: ' + operation)
    return result['data']


def control(operation):
    result = json.loads(run(['docker', 'exec', '-i', DEER, '/var/lib/deerflow/deerflow-control-go',
                             '--runtime-dir', '/var/lib/deerflow/user-data/runtime/acp-go'],
                            json.dumps({'operation': operation}), timeout=20))
    if not result.get('ok'):
        raise RuntimeError('daemon control rejected')
    return result['data']


def idle():
    state = control('daemon.status')
    if state.get('active_operations') != 0 or state.get('queued_runs', 0) != 0:
        raise RuntimeError('native daemon has active work')
    with sqlite3.connect((FOLO / 'data/deerflow-sessions.sqlite3').as_uri() + '?mode=ro', uri=True) as db:
        if db.execute("SELECT count(*) FROM deerflow_sessions WHERE status NOT IN ('completed','idle','invalidated','failed','cancelled')").fetchone()[0]:
            raise RuntimeError('voice turn active or unconfirmed')
    with sqlite3.connect((ROOT / 'data/user-data/config/go-harness-state/harness.db').as_uri() + '?mode=ro', uri=True) as db:
        if db.execute("SELECT count(*) FROM harness_runs WHERE status IN ('running','queued','starting','cancelling')").fetchone()[0]:
            raise RuntimeError('native run still active')
    # Historical needs_reconciliation entries are preserved and never reset.
    return state


def inspect(name):
    return json.loads(run(['docker', 'inspect', name]))[0]


def restart():
    run(['docker', 'restart', '--time', '30', DEER], timeout=100)
    for _ in range(45):
        state = inspect(DEER)['State']
        if state.get('Health', {}).get('Status') == 'healthy':
            return
        if not state['Running']:
            raise RuntimeError('DeerFlow stopped after restart')
        time.sleep(2)
    raise RuntimeError('DeerFlow readiness timeout')


def main():
    baseline = json.loads((STAGE / 'preflight.json').read_text())
    original = CONFIG.read_bytes()
    if digest(original) != baseline['config_sha256']:
        raise RuntimeError('configuration changed after preflight')
    parser = '''import copy,json,sys,yaml
p=json.load(sys.stdin);raw=p['raw'];before=yaml.safe_load(raw);after=copy.deepcopy(before);document=yaml.compose(raw)
tools=next(v for k,v in document.value if k.value=='tools');edits=[];seen=set();newline='\\r\\n' if '\\r\\n' in raw else '\\n'
for tool in tools.value:
 pairs={k.value:(k,v) for k,v in tool.value};name=pairs.get('name',(None,None))[1]
 if name is None or name.value not in p['names']:continue
 if name.value in seen or tool.flow_style:raise RuntimeError('unsupported or duplicate tool mapping')
 seen.add(name.value)
 if 'https_proxy' in pairs:
  value=pairs['https_proxy'][1];edits.append((value.start_mark.index,value.end_mark.index,p['proxy']))
 else:
  name_key=pairs['name'][0];line=raw.find('\\n',name.end_mark.index)
  if line<0:edits.append((len(raw),len(raw),newline+' '*name_key.start_mark.column+'https_proxy: '+p['proxy']+newline))
  else:edits.append((line+1,line+1,' '*name_key.start_mark.column+'https_proxy: '+p['proxy']+newline))
if seen!=set(p['names']):raise RuntimeError('requested tools missing')
candidate=raw
for start,end,value in sorted(edits,reverse=True):candidate=candidate[:start]+value+candidate[end:]
for tool in after['tools']:
 if tool['name'] in seen:tool['https_proxy']=p['proxy']
if yaml.safe_load(candidate)!=after:raise RuntimeError('proxy-only semantic validation failed')
key_before=next(t for t in before['tools'] if t['name']=='web_search').get('api_key');key_after=next(t for t in after['tools'] if t['name']=='web_search').get('api_key')
assert key_before==key_after
print(json.dumps({'candidate':candidate,'api_key_preserved':True,'only_requested_fields_changed':True,'changed_paths':['tools.'+n+'.https_proxy' for n in p['names']]}))
'''
    proposal = json.loads(run(['docker', 'exec', '-i', TOY, 'python3', '-c', parser],
                              json.dumps({'raw': original.decode('utf-8'), 'names': TOOLS, 'proxy': PROXY})))
    candidate = proposal.pop('candidate').encode('utf-8')
    document = native('snapshot', {})
    for tool in document['tools']:
        if tool['name'] in TOOLS:
            tool['https_proxy'] = PROXY
    native('validate', document)
    if CONFIG.read_bytes() != original:
        raise RuntimeError('config changed during validation')
    state = idle()
    for name in (DEER, TOY):
        obj = inspect(name)
        if obj['Image'] != baseline['containers'][name]['image_id']:
            raise RuntimeError('runtime image changed since preflight')
    backup = STAGE / ('backup-' + datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ'))
    backup.mkdir(mode=0o700)
    backup_config = backup / 'config.yaml'
    backup_config.write_bytes(original)
    backup_config.chmod(0o600)
    report = {'status': 'prepared', 'proxy': PROXY, 'tools': TOOLS, 'config_path': str(CONFIG),
              'backup_config': str(backup_config), 'original_mode': CONFIG.stat().st_mode & 0o777,
              'before_sha256': digest(original), 'after_sha256': digest(candidate),
              'semantic_check': proposal, 'daemon_idle_before': state['active_operations'] == 0,
              'folotoy_restart_requested': False, 'image_rebuild_requested': False,
              'historical_reconciliation_records_preserved': True}
    def save():
        atomic(STAGE / 'deployment.json', (json.dumps(report, indent=2) + '\n').encode(), 0o600)
    save()
    drained = False
    applied = False
    try:
        # Resume even if the acknowledgement is lost after drain was applied.
        drained = True
        drained_state = control('daemon.drain')
        if drained_state.get('active_operations') != 0:
            raise RuntimeError('work began before drain; configuration not applied')
        idle()
        if CONFIG.read_bytes() != original:
            raise RuntimeError('concurrent configuration write')
        atomic(CONFIG, candidate, report['original_mode'])
        applied = True
        report['status'] = 'configuration_applied'
        save()
        print(json.dumps({'stage': 'proxy_fields_applied', 'tools': TOOLS, 'key_preserved': True}), flush=True)
        restart()
        resumed = control('daemon.resume')
        drained = False
        if resumed.get('draining') is not False:
            raise RuntimeError('daemon did not resume')
        current = native('snapshot', {})
        settings = {tool['name']: tool.get('https_proxy') for tool in current['tools'] if tool['name'] in TOOLS}
        if any(settings.get(name) != PROXY for name in TOOLS):
            raise RuntimeError('saved proxy settings do not match')
        toy = inspect(TOY)
        before_toy = baseline['containers'][TOY]
        if toy['Id'] != before_toy['id'] or toy['State']['StartedAt'] != before_toy['started_at']:
            raise RuntimeError('FoloToy was unexpectedly restarted')
        report.update(status='deployed_healthy', default_model=current['default_model'],
                      effective_proxy_settings=settings, folotoy_container_preserved=True,
                      deerflow_image_preserved=inspect(DEER)['Image'] == baseline['containers'][DEER]['image_id'])
        save()
        print(json.dumps({k: report[k] for k in ('status', 'proxy', 'effective_proxy_settings', 'backup_config', 'default_model', 'folotoy_container_preserved')}), flush=True)
    except Exception:
        if applied and digest(CONFIG.read_bytes()) == report['after_sha256']:
            atomic(CONFIG, original, report['original_mode'])
            report['status'] = 'rolled_back_after_error'
            save()
            restart()
        raise
    finally:
        if drained:
            control('daemon.resume')


if __name__ == '__main__':
    main()
