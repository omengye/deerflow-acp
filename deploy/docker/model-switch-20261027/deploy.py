"""Switch Gentoo's native model and FoloToy's explicit model, preserving state.

Run through the authenticated SSH orchestrator on the existing host. Private
credentials stay in server memory/configuration and never enter the report.
"""
import copy
import datetime
import hashlib
import json
import os
from pathlib import Path
import re
import sqlite3
import subprocess
import time
import uuid

ROOT = Path('/home/gen/deerflow-deploy')
FOLO = Path('/home/gen/Documents/folotoy/deerflow-ws/src')
STAGE = ROOT / 'model-switch-20261027'
TARGET = 'gpt-6.1-terra'
DEER = 'deerflow-deploy-deerflow-1'
TOY = 'folotoy-ws-folotoy-1'


def run(command, data=None, timeout=75):
    result = subprocess.run(command, input=data, text=True, capture_output=True, timeout=timeout)
    if result.returncode:
        raise RuntimeError('model switch operation failed: ' + command[0])
    return result.stdout


def config(operation, document):
    command = ['docker', 'exec', '-i', DEER, '/var/lib/deerflow/deerflow-config-go',
               '--config', '/var/lib/deerflow/user-data/config/config.yaml',
               '--user-data', '/var/lib/deerflow/user-data',
               '--resources', '/var/lib/deerflow/resources', operation]
    result = json.loads(run(command, json.dumps(document)))
    if not result.get('ok'):
        raise RuntimeError('native config rejected: ' + operation)
    return result['data']


def atomic(path, data, mode):
    temporary = path.with_name('.' + path.name + '.model-' + uuid.uuid4().hex)
    try:
        temporary.write_bytes(data)
        temporary.chmod(mode)
        with temporary.open('rb') as stream:
            os.fsync(stream.fileno())
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


def digest(data):
    return hashlib.sha256(data).hexdigest()


def idle():
    with sqlite3.connect((FOLO / 'data/deerflow-sessions.sqlite3').as_uri() + '?mode=ro', uri=True) as db:
        if db.execute("SELECT count(*) FROM deerflow_sessions WHERE status NOT IN ('completed','idle','invalidated','failed','cancelled')").fetchone()[0]:
            raise RuntimeError('active or unconfirmed voice turn; switch deferred')
    with sqlite3.connect((ROOT / 'data/user-data/config/go-harness-state/harness.db').as_uri() + '?mode=ro', uri=True) as db:
        if db.execute("SELECT count(*) FROM harness_runs WHERE status NOT IN ('completed','failed','cancelled')").fetchone()[0]:
            raise RuntimeError('active native model run; switch deferred')


def restart():
    run(['docker', 'compose', '-p', 'deerflow-deploy', '-f', str(ROOT / 'compose.yaml'),
         '--project-directory', str(ROOT), 'up', '-d', '--no-build', '--force-recreate', 'deerflow'], timeout=120)
    run(['docker', 'compose', '--env-file', str(FOLO / '.env.ws'), '-p', 'folotoy-ws',
         '-f', str(FOLO / 'compose.ws.yaml'), '--project-directory', str(FOLO),
         'up', '-d', '--no-build', '--force-recreate', 'folotoy'], timeout=120)
    for _ in range(45):
        states = {name: json.loads(run(['docker', 'inspect', name]))[0]['State'] for name in (DEER, TOY)}
        if all(state.get('Health', {}).get('Status') == 'healthy' for state in states.values()):
            return
        if any(not state['Running'] for state in states.values()):
            raise RuntimeError('container stopped after switch')
        time.sleep(2)
    raise RuntimeError('container health timeout')


def main():
    preflight = json.loads((STAGE / 'preflight.json').read_text())
    if not preflight['target_capability_probe'].get('ok'):
        raise RuntimeError('target model probe did not pass')
    cfg, env = ROOT / 'data/user-data/config/config.yaml', FOLO / '.env.ws'
    raw, oldenv = cfg.read_bytes(), env.read_bytes()
    if digest(raw) != preflight['config_sha256'] or digest(oldenv) != preflight['env_sha256']:
        raise RuntimeError('configuration changed after preflight')
    parser = '''import copy,json,sys,yaml
p=json.load(sys.stdin);before=yaml.safe_load(p['raw']);d=copy.deepcopy(before)
old=next(x for x in d['models'] if x['name']==d['default_model'])
if any(x['name']==p['target'] for x in d['models']):raise RuntimeError('target already exists')
new=copy.deepcopy(old);new['name']=new['model']=p['target'];new.pop('display_name',None);new.pop('description',None)
d['models'].append(new);d['default_model']=p['target']
for key in ('local_acp','api'):
 if (d.get(key) or {}).get('model_name'):d[key]['model_name']=p['target']
out=yaml.safe_dump(d,allow_unicode=True,sort_keys=False);assert yaml.safe_load(out)==d
print(json.dumps({'raw':out,'new':new,'old_name':old['name']}))
'''
    proposal = json.loads(run(['docker', 'exec', '-i', TOY, 'python3', '-c', parser],
                              json.dumps({'raw': raw.decode(), 'target': TARGET})))
    draft = config('snapshot', {})
    model = copy.deepcopy(next(m for m in draft['models'] if m['name'] == draft['default_model']))
    model.update(original_name='', name=TARGET, model=TARGET, display_name='', description='',
                 api_key=proposal['new'].get('api_key', ''), clear_api_key=False)
    draft['models'].append(model)
    draft['default_model'] = TARGET
    if draft['runtime'].get('model_name'):
        draft['runtime']['model_name'] = TARGET
    config('validate', draft)
    lines = oldenv.decode().splitlines(keepends=True)
    found = [i for i, line in enumerate(lines) if re.match(r'^\s*(?:export\s+)?DEERFLOW_MODEL\s*=', line)]
    if len(found) > 1:
        raise RuntimeError('duplicate model environment variables')
    if found:
        lines[found[0]] = 'DEERFLOW_MODEL=' + TARGET + '\n'
    else:
        if lines and not lines[-1].endswith('\n'):
            lines[-1] += '\n'
        lines.append('DEERFLOW_MODEL=' + TARGET + '\n')
    newenv, newraw = ''.join(lines).encode(), proposal['raw'].encode()
    idle()
    backup = STAGE / ('backup-' + datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ'))
    backup.mkdir(mode=0o700)
    entries = []
    for name, path, old, new in [('config.yaml', cfg, raw, newraw), ('env.ws', env, oldenv, newenv)]:
        saved = backup / name
        saved.write_bytes(old)
        saved.chmod(0o600)
        entries.append({'path': str(path), 'backup': str(saved), 'before_sha256': digest(old),
                        'after_sha256': digest(new), 'mode': path.stat().st_mode & 0o777})
    report = {'status': 'prepared', 'target': TARGET, 'previous_default': proposal['old_name'],
              'files': entries, 'backup_dir': str(backup), 'management_ip': '100.80.205.120',
              'ws_url_preserved': preflight['folo_ws_url'], 'old_model_entries_retained': True,
              'images': preflight['containers'], 'device_state_preserved': True}
    def save():
        atomic(STAGE / 'deployment.json', (json.dumps(report, indent=2) + '\n').encode(), 0o600)
    save()
    try:
        if cfg.read_bytes() != raw or env.read_bytes() != oldenv:
            raise RuntimeError('concurrent configuration write')
        atomic(cfg, newraw, entries[0]['mode'])
        atomic(env, newenv, entries[1]['mode'])
        print(json.dumps({'stage': 'model_configuration_applied', 'target': TARGET}), flush=True)
        restart()
        doc = config('snapshot', {})
        state = json.loads(run(['docker', 'inspect', TOY]))[0]
        effective = dict(v.split('=', 1) for v in state['Config']['Env'] if '=' in v)
        if doc['default_model'] != TARGET or effective.get('DEERFLOW_MODEL') != TARGET:
            raise RuntimeError('runtime configuration mismatch')
        if effective.get('DEERFLOW_WS_URL') != preflight['folo_ws_url']:
            raise RuntimeError('internal WS route changed unexpectedly')
        report.update(status='deployed_healthy', default_model=doc['default_model'], folo_model=effective['DEERFLOW_MODEL'])
        save()
        print(json.dumps({k: v for k, v in report.items() if k not in ('files', 'images')}), flush=True)
    except Exception:
        for entry in entries:
            path = Path(entry['path'])
            if digest(path.read_bytes()) == entry['after_sha256']:
                atomic(path, Path(entry['backup']).read_bytes(), entry['mode'])
        report['status'] = 'rolled_back_after_error'
        save()
        restart()
        raise


if __name__ == '__main__':
    main()
