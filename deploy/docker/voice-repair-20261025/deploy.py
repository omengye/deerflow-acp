"""Gentoo-only, backed-up rollout of restart-safe FoloToy device login state.

The SSH orchestrator supplies SOURCE_FILES. No credentials enter the image.
Existing ASR2, WS, role settings and model configuration are preserved.
"""
import datetime
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sqlite3
import time
import uuid

ROOT = Path('/home/gen/deerflow-deploy/voice-repair-20261025')
FOLO = Path('/home/gen/Documents/folotoy/deerflow-ws/src')
CONTAINER = 'folotoy-ws-folotoy-1'
IMAGE = 'folotoy-deerflow-ws:device-sessions-v2-20261025'
FILES = ('core/device_session_store.py', 'core/folotoy_app.py', 'core/folotoy_msg_processor.py')


def command(argv, payload=None, timeout=60):
    result = subprocess.run(argv, input=payload, text=True, capture_output=True, timeout=timeout)
    if result.returncode:
        raise RuntimeError('deployment command failed: ' + argv[0])
    return result.stdout


def sha(data):
    return hashlib.sha256(data).hexdigest()


def atomic(path, data, mode=0o600):
    temporary = path.with_name('.' + path.name + '.' + uuid.uuid4().hex)
    try:
        temporary.write_bytes(data)
        temporary.chmod(mode)
        with temporary.open('rb') as stream:
            os.fsync(stream.fileno())
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


def compose():
    return ['docker', 'compose', '--env-file', str(FOLO / '.env.ws'), '-p', 'folotoy-ws',
            '-f', str(FOLO / 'compose.ws.yaml'), '--project-directory', str(FOLO)]


def healthy():
    for _ in range(45):
        state = json.loads(command(['docker', 'inspect', CONTAINER]))[0]['State']
        if state.get('Health', {}).get('Status') == 'healthy':
            return
        if not state['Running']:
            raise RuntimeError('FoloToy container stopped')
        time.sleep(2)
    raise RuntimeError('FoloToy health timeout')


def main():
    baseline = json.loads((ROOT / 'preflight.json').read_text())
    current = json.loads(command(['docker', 'inspect', CONTAINER]))[0]
    if current['Image'] != baseline['image_id']:
        raise RuntimeError('runtime image changed since preflight')
    before = {}
    for name in FILES:
        path = FOLO / name
        before[name] = path.read_bytes() if path.is_file() else None
        if name in baseline['source_files'] and sha(before[name]) != baseline['source_files'][name]:
            raise RuntimeError('runtime source changed since preflight')
        if name not in baseline['source_files'] and before[name] is not None:
            raise RuntimeError('new module unexpectedly exists')
    raw_compose = (FOLO / 'compose.ws.yaml').read_bytes()
    if sha(raw_compose) != baseline['compose_sha256']:
        raise RuntimeError('Compose changed since preflight')
    private = FOLO / 'data/private'
    if private.exists() and (private.stat().st_mode & 0o777) != 0o700:
        raise RuntimeError('existing private data directory permissions need inspection')

    payload = ROOT / ('payload-' + uuid.uuid4().hex[:12])
    payload.mkdir(mode=0o700)
    tests = payload / 'tests'
    tests.mkdir(mode=0o700)
    for name in FILES:
        path = payload / 'folotoy-source' / name
        path.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
        path.write_text(SOURCE_FILES[Path(name).name])
        path.chmod(0o600)
    (tests / 'test_device_sessions.py').write_text(SOURCE_FILES['test_device_sessions.py'])
    test_payload = {str(p.relative_to(payload)): p.read_text() for p in payload.rglob('*.py')}
    test_code = '''import json,os,pathlib,subprocess,sys,tempfile
p=json.load(sys.stdin)
with tempfile.TemporaryDirectory(prefix='device-session-tests-') as temporary:
 root=pathlib.Path(temporary)
 for name,source in p.items():
  dest=root/name;dest.parent.mkdir(parents=True,exist_ok=True);dest.write_text(source)
 env=dict(os.environ,FOLOTOY_BASE_SOURCE='/opt/folotoy')
 result=subprocess.run([sys.executable,str(root/'tests/test_device_sessions.py')],env=env,capture_output=True,text=True)
 print(json.dumps({'passed':result.returncode==0,'diagnostics':(result.stdout+'\\n'+result.stderr)[-6000:]}))
'''
    test_result = json.loads(command(['docker', 'exec', '-i', '-w', '/opt/folotoy', CONTAINER,
                                      'python3', '-c', test_code], json.dumps(test_payload)))
    if not test_result['passed']:
        print(json.dumps({'stage': 'offline_tests_failed', 'result': test_result}), flush=True)
        raise RuntimeError('device session regression tests failed')
    print(json.dumps({'stage': 'offline_tests_passed', 'result': test_result}), flush=True)
    dockerfile = ('ARG BASE_IMAGE\nFROM ${BASE_IMAGE}\nUSER root\n' +
                  ''.join('COPY --chown=1000:1000 folotoy-source/' + name + ' /opt/folotoy/' + name + '\n' for name in FILES) +
                  'RUN rm -f /opt/folotoy/core/folotoy_app.cpython-*.so /opt/folotoy/core/folotoy_msg_processor.cpython-*.so\n' +
                  'USER ' + baseline['runtime_user'] + '\n')
    (payload / 'Dockerfile').write_text(dockerfile)
    command(['docker', 'build', '--network', 'none', '--build-arg', 'BASE_IMAGE=' + baseline['image'],
             '--tag', IMAGE, str(payload)], timeout=240)
    image = json.loads(command(['docker', 'image', 'inspect', IMAGE]))[0]['Id']
    parser = '''import copy,json,sys,yaml
p=json.load(sys.stdin);old=yaml.safe_load(p['raw']);new=copy.deepcopy(old);new['services']['folotoy']['image']=p['image']
print(json.dumps({'raw':yaml.safe_dump(new,sort_keys=False),'only_image_changed':old['services']['folotoy']['image']!=p['image']}))
'''
    parsed = json.loads(command(['docker', 'exec', '-i', CONTAINER, 'python3', '-c', parser],
                                json.dumps({'raw': raw_compose.decode(), 'image': IMAGE})))
    new_compose = parsed['raw'].encode()
    status_probe = subprocess.run(['docker', 'exec', '-i', 'deerflow-deploy-deerflow-1',
                                 '/var/lib/deerflow/deerflow-control-go', '--runtime-dir',
                                 '/var/lib/deerflow/user-data/runtime/acp-go'], input=json.dumps({'operation': 'daemon.status'}),
                                 text=True, capture_output=True, timeout=20)
    if status_probe.returncode == 0:
        status = json.loads(status_probe.stdout)
        if not status.get('ok') or status.get('data', {}).get('active_operations') not in (0, [], {}):
            raise RuntimeError('native runtime busy')
    elif status_probe.stderr.strip() != 'cannot connect to local daemon':
        raise RuntimeError('native status probe failed')
    with sqlite3.connect('file:/home/gen/deerflow-deploy/data/user-data/config/go-harness-state/harness.db?mode=ro', uri=True) as db:
        if db.execute("SELECT count(*) FROM harness_runs WHERE status NOT IN ('completed','failed','cancelled')").fetchone()[0]:
            raise RuntimeError('native database has an unfinished run')
    with sqlite3.connect((FOLO / 'data/deerflow-sessions.sqlite3').as_uri() + '?mode=ro', uri=True) as db:
        if db.execute("SELECT count(*) FROM deerflow_sessions WHERE status NOT IN ('completed','idle','invalidated','failed','cancelled')").fetchone()[0]:
            raise RuntimeError('FoloToy has an unfinished turn')

    backup = ROOT / ('backup-' + datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ'))
    backup.mkdir(mode=0o700)
    entries = []
    proposals = {name: SOURCE_FILES[Path(name).name].encode() for name in FILES}
    proposals['compose.ws.yaml'] = new_compose
    before['compose.ws.yaml'] = raw_compose
    for index, (name, new) in enumerate(proposals.items()):
        path = FOLO / name
        original = before[name]
        saved = backup / (str(index) + '.before') if original is not None else None
        if saved:
            saved.write_bytes(original)
            saved.chmod(0o600)
        entries.append({'path': str(path), 'backup': str(saved) if saved else None,
                        'before_sha256': sha(original) if original is not None else None,
                        'after_sha256': sha(new), 'mode': path.stat().st_mode & 0o777 if path.exists() else 0o644})
    report = {'status': 'prepared', 'old_image': baseline['image'], 'old_image_id': baseline['image_id'],
              'image': IMAGE, 'image_id': image, 'backup': str(backup), 'payload': str(payload),
              'files': entries, 'offline_tests': test_result, 'model': 'gemini-3.8-flash-high'}
    report_path = ROOT / 'deployment.json'
    def save():
        atomic(report_path, (json.dumps(report, indent=2) + '\n').encode())
    save()
    try:
        for entry in entries:
            path = Path(entry['path'])
            current_data = path.read_bytes() if path.exists() else None
            if current_data != before[str(path.relative_to(FOLO))]:
                raise RuntimeError('concurrent source change')
        for entry, (name, data) in zip(entries, proposals.items()):
            atomic(Path(entry['path']), data, entry['mode'])
        command(compose() + ['config', '--quiet'])
        command(compose() + ['up', '-d', '--no-build', '--force-recreate', 'folotoy'], timeout=120)
        healthy()
        report['status'] = 'deployed_healthy'
        save()
        print(json.dumps({k: v for k, v in report.items() if k not in ('files', 'offline_tests')}), flush=True)
    except Exception:
        for entry in entries:
            path = Path(entry['path'])
            if path.exists() and sha(path.read_bytes()) == entry['after_sha256']:
                if entry['backup']:
                    atomic(path, Path(entry['backup']).read_bytes(), entry['mode'])
                else:
                    path.unlink()
        report['status'] = 'rolled_back_after_error'
        save()
        command(compose() + ['up', '-d', '--no-build', '--force-recreate', 'folotoy'], timeout=120)
        healthy()
        raise


if __name__ == '__main__':
    main()
