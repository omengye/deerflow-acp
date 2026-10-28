"""Deploy the ASR2 input-rate fix while preserving the original v2 rollback.

Only the ASR2 module and Compose image change. Existing keys, roles, voices and
DeerFlow settings are never rewritten. All subprocess output stays captured.
"""
import argparse
from contextlib import closing
from datetime import datetime, timezone
import copy
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import sqlite3
import subprocess
import tempfile
import time
import uuid

CONTAINER = 'folotoy-ws-folotoy-1'
IMAGE = 'folotoy-deerflow-ws:asr2-audiofix-20261025'
SOURCE = 'src/sdk/stt_sdk/volcengine_asr2.py'
EXPECTED_BEFORE = 'b77453075b9198c94220c5c7899ad01e80dc80a1e16e73ff56ad61ae4a4dc8e0'
DEFAULT_ROOT = Path('/home/gen/deerflow-deploy/asr2-audiofix-20261025')
DEFAULT_ROLLOUT = Path('/home/gen/deerflow-deploy/asr2-rollout-20261025/deployment.json')


class DeploymentError(Exception):
    """Fixed diagnostics, without credential-bearing Docker/YAML output."""


def sha(data):
    return hashlib.sha256(data).hexdigest() if data is not None else None


def raw(path):
    if path.is_symlink():
        raise DeploymentError('Refusing a linked deployment target')
    return path.read_bytes() if path.is_file() else None


def encoded(value):
    return (json.dumps(value, ensure_ascii=False, indent=2) + '\n').encode()


def atomic(path, data, mode=0o600):
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, name = tempfile.mkstemp(prefix='.asr2-audiofix-', dir=path.parent)
    try:
        with os.fdopen(fd, 'wb') as output:
            output.write(data)
            output.flush()
            os.fsync(output.fileno())
        os.chmod(name, mode)
        os.replace(name, path)
    finally:
        Path(name).unlink(missing_ok=True)


def command(argv, *, payload=None, cwd=None, timeout=60):
    env = dict(os.environ)
    env.pop('FOLOTOY_WS_IMAGE', None)
    value = subprocess.run(argv, input=payload, capture_output=True, text=True,
                           cwd=cwd, env=env, timeout=timeout)
    if value.returncode:
        raise DeploymentError('Command failed: ' + argv[0] + ' exit ' + str(value.returncode))
    return value


def inspect():
    value = json.loads(command(['docker', 'inspect', CONTAINER], timeout=15).stdout)[0]
    return {'image': value['Config']['Image'], 'image_id': value['Image'],
            'runtime_user': value['Config'].get('User') or '0',
            'container_id': value['Id'], 'started_at': value['State']['StartedAt'],
            'status': value['State']['Status'],
            'health': value['State'].get('Health', {}).get('Status')}


def healthy(timeout=90):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = inspect()
        if value['status'] == 'running' and value['health'] == 'healthy':
            return value
        time.sleep(2)
    raise DeploymentError('FoloToy health deadline exceeded')


def wait_idle(folo, timeout=120):
    path = folo / 'data/deerflow-sessions.sqlite3'
    if not path.is_file():
        raise DeploymentError('Cannot establish voice request activity')
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        with closing(sqlite3.connect(path.resolve().as_uri() + '?mode=ro', uri=True, timeout=2)) as db:
            count = db.execute("SELECT count(*) FROM deerflow_sessions WHERE status IN ('running','pending','cancelling','in_progress')").fetchone()[0]
        if count == 0:
            return
        time.sleep(2)
    raise DeploymentError('Voice requests did not become idle')


def compose_args(folo):
    return ['docker', 'compose', '--env-file', str(folo / '.env.ws'), '-p', 'folotoy-ws',
            '--project-directory', str(folo), '-f', str(folo / 'compose.ws.yaml')]


def compose_validate(folo):
    command(compose_args(folo) + ['config', '--quiet'], cwd=folo, timeout=30)


def compose_up(folo, pinned_image=None):
    argv = compose_args(folo)
    override = None
    try:
        if pinned_image:
            if not re.fullmatch(r'sha256:[a-f0-9]{64}', pinned_image):
                raise DeploymentError('Invalid pinned rollback image')
            override = folo / ('.asr2-audiofix-rollback-' + uuid.uuid4().hex + '.yaml')
            atomic(override, ('services:\n  folotoy:\n    image: ' + pinned_image + '\n').encode())
            argv += ['-f', str(override)]
        command(argv + ['up', '-d', '--no-build', '--force-recreate', 'folotoy'], cwd=folo, timeout=120)
    finally:
        if override:
            override.unlink(missing_ok=True)


YAML_UPDATE = r'''
import copy,json,sys,yaml
r=json.load(sys.stdin);before=yaml.safe_load(r['raw']);after=copy.deepcopy(before)
after['services']['folotoy']['image']=r['image']
text=yaml.safe_dump(after,allow_unicode=True,sort_keys=False)
if yaml.safe_load(text)!=after:raise ValueError('YAML serialization changed values')
print(json.dumps({'raw':text}))
'''


def compose_candidate(data):
    result = command(['docker', 'exec', '-i', CONTAINER, 'python3', '-c', YAML_UPDATE],
                     payload=json.dumps({'raw': data.decode(), 'image': IMAGE}), timeout=30)
    return json.loads(result.stdout)['raw'].encode()


def validate_rollout(report_path):
    report_path = report_path.resolve(strict=True)
    before_report = raw(report_path)
    report = json.loads(before_report)
    if report.get('format_version') != 1 or report.get('status') != 'all_roles_ready':
        raise DeploymentError('Expected the completed ASR2 role rollout')
    folo = Path(report['folotoy_root']).resolve(strict=True)
    backup = Path(report['backup']).resolve(strict=True)
    if backup.parent != report_path.parent or not backup.name.startswith('backup-'):
        raise DeploymentError('Original rollback backup identity mismatch')
    index_path = backup / 'index.json'
    before_index = raw(index_path)
    index = json.loads(before_index)
    if index.get('deployment_id') != report.get('deployment_id') or index.get('backup') != str(backup) or index.get('folotoy_root') != str(folo):
        raise DeploymentError('Original rollback index identity mismatch')
    if not re.fullmatch(r'sha256:[a-f0-9]{64}', index.get('old_image_id', '')):
        raise DeploymentError('Original v2 rollback image is invalid')
    expected = {str(folo / p) for p in ('compose.ws.yaml', 'roles-ws.json', 'private/asr2.env',
                'config.py', 'util/config_util.py', 'sdk/stt.py', 'sdk/stt_sdk/volcengine_asr2.py')}
    if {r['path'] for r in index['changes']} != expected:
        raise DeploymentError('Original rollback target set differs from the reviewed rollout')
    for record in index['changes']:
        if sha(raw(Path(record['path']))) != record['after_sha256']:
            raise DeploymentError('Original ASR2 deployment was changed externally')
        name = record.get('backup_file')
        if name is not None:
            if not re.fullmatch(r'[0-9]+\.before', name) or sha(raw(backup / name)) != record['before_sha256']:
                raise DeploymentError('Original rollback backup failed validation')
    return report, index, index_path, before_report, before_index, folo


def prepare(path, after, backup, number):
    before = raw(path)
    if before is None:
        raise DeploymentError('Expected existing audiofix target')
    mode = path.stat().st_mode & 0o777
    filename = str(number) + '.before'
    atomic(backup / filename, before)
    return {'path': str(path), 'before_sha256': sha(before), 'after_sha256': sha(after),
            'before_mode': mode, 'after_mode': mode, 'backup_file': filename}, after


def verify_changes(changes):
    for record, _ in changes:
        if sha(raw(Path(record['path']))) != record['before_sha256']:
            raise DeploymentError('Audiofix target changed concurrently')


def apply_change(record, after):
    if sha(raw(Path(record['path']))) != record['before_sha256']:
        raise DeploymentError('Audiofix target changed concurrently')
    atomic(Path(record['path']), after, record['after_mode'])


def restore(index):
    backup = Path(index['backup'])
    for record in index['changes']:
        if sha(raw(Path(record['path']))) not in (record['before_sha256'], record['after_sha256']):
            raise DeploymentError('Rollback stopped for an externally changed audiofix target')
        if sha(raw(backup / record['backup_file'])) != record['before_sha256']:
            raise DeploymentError('Audiofix backup digest mismatch')
    for record in reversed(index['changes']):
        atomic(Path(record['path']), (backup / record['backup_file']).read_bytes(), record['before_mode'])


RUNTIME = r'''
import hashlib,importlib,json,logging,pathlib,shutil
logging.disable(logging.CRITICAL)
from config import Config
from util.config_util import get_role_stt_config
c=Config();m=importlib.import_module('sdk.stt_sdk.volcengine_asr2');roles={}
for n in range(1,8):
 r=c.ROLES[str(n)];kind=r.get('stt_type',c.STT_TYPE);p=get_role_stt_config(kind,r,c)
 roles[str(n)]={'provider':kind,'resource_id':p.get('resource_id'),'api_key_present':bool(p.get('api_key'))}
print(json.dumps({'module_file':m.__file__,'module_sha256':hashlib.sha256(pathlib.Path(m.__file__).read_bytes()).hexdigest(),'ffmpeg_available':bool(shutil.which('ffmpeg')),'roles':roles,'global_stt_type':c.STT_TYPE,'hotwords_enabled':False}))
'''


def runtime_check(expected_sha):
    value = json.loads(command(['docker', 'exec', '-w', '/opt/folotoy', CONTAINER,
                                'python3', '-c', RUNTIME], timeout=25).stdout)
    if (not value['module_file'].endswith('.py') or value['module_sha256'] != expected_sha
            or not value['ffmpeg_available']):
        raise DeploymentError('The fixed ASR2 Python module did not load')
    if not all(v == {'provider': 'volcengine-asr2', 'resource_id': 'volc.seedasr.sauc.duration', 'api_key_present': True}
               for v in value['roles'].values()) or len(value['roles']) != 7:
        raise DeploymentError('ASR2 role or resource configuration changed')
    return value


def update_original(original_report, original_index, source_path, compose_path,
                    source_after, compose_after, state, runtime, fix_id, report_path):
    report = copy.deepcopy(original_report)
    index = copy.deepcopy(original_index)
    selected = {str(source_path): sha(source_after), str(compose_path): sha(compose_after)}
    for record in index['changes']:
        if record['path'] in selected:
            record['after_sha256'] = selected.pop(record['path'])
    if selected:
        raise DeploymentError('Original rollback lacks the audiofix targets')
    report.update(new_image=IMAGE, container=state,
                  audio_input_fix={'id': fix_id, 'report': str(report_path),
                                   'live_input_validation': 'pending'})
    report.setdefault('runtime', {})['audio_input_fix'] = runtime
    return report, index


def deploy(args):
    payload = args.payload.resolve(strict=True)
    original_path = args.rollout_report.resolve(strict=True)
    original, original_index, original_index_path, original_raw, index_raw, folo = validate_rollout(original_path)
    manifest = json.loads((payload / 'folotoy-source/manifest.json').read_text())
    if len(manifest['files']) != 1 or manifest['files'][0]['path'] != SOURCE:
        raise DeploymentError('Expected one ASR2 module in the patch manifest')
    record = manifest['files'][0]
    source_after = raw(payload / 'folotoy-source' / SOURCE)
    source_path = folo / 'sdk/stt_sdk/volcengine_asr2.py'
    if record.get('before_sha256') != EXPECTED_BEFORE or sha(raw(source_path)) != EXPECTED_BEFORE:
        raise DeploymentError('Existing ASR2 module differs from the reviewed version')
    if source_after is None or sha(source_after) != record['after_sha256']:
        raise DeploymentError('Fixed source digest mismatch')
    wait_idle(folo)
    previous = healthy()
    if previous['image_id'] != original['container']['image_id']:
        raise DeploymentError('Running container differs from the ASR2 rollout')
    root = args.root.resolve()
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    root.chmod(0o700)
    report_path = root / 'deployment.json'
    if report_path.exists():
        raise DeploymentError('An audiofix deployment report already exists')
    fix_id = uuid.uuid4().hex
    backup = root / ('backup-' + datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ') + '-' + fix_id[:8])
    backup.mkdir(mode=0o700)
    compose_path = folo / 'compose.ws.yaml'
    compose_after = compose_candidate(raw(compose_path))
    changes = [prepare(source_path, source_after, backup, 0),
               prepare(compose_path, compose_after, backup, 1),
               prepare(original_path, original_raw, backup, 2),
               prepare(original_index_path, index_raw, backup, 3)]
    if raw(original_path) != original_raw or raw(original_index_path) != index_raw:
        raise DeploymentError('Original rollout metadata changed concurrently')
    index = {'format_version': 1, 'fix_id': fix_id, 'backup': str(backup),
             'folotoy_root': str(folo), 'rollout_report': str(original_path),
             'rollout_index': str(original_index_path), 'old_image_id': previous['image_id'],
             'changes': [r for r, _ in changes]}
    index_path = backup / 'index.json'
    atomic(index_path, encoded(index))
    report = {'format_version': 1, 'fix_id': fix_id, 'status': 'preparing',
              'backup': str(backup), 'folotoy_root': str(folo), 'rollout_report': str(original_path),
              'payload_root': str(payload), 'previous_container': previous, 'new_image': IMAGE,
              'credentials_changed': False, 'roles_changed': False, 'hotwords_enabled': False,
              'full_v2_rollback': original['manual_rollback'],
              'manual_rollback': 'python3 ' + str(payload / 'rollback.py') + ' --deployment-report ' + str(report_path)}
    atomic(report_path, encoded(report))
    applied = False
    try:
        base_tag = 'folotoy-deerflow-ws:asr2-audiofix-base-' + fix_id[:12]
        command(['docker', 'image', 'tag', previous['image_id'], base_tag])
        report['pinned_base_tag'] = base_tag
        with tempfile.TemporaryDirectory(prefix='build-', dir=root) as temporary:
            context = Path(temporary)
            shutil.copyfile(payload / 'Dockerfile.folotoy', context / 'Dockerfile')
            target = context / 'folotoy-source' / SOURCE
            target.parent.mkdir(parents=True)
            target.write_bytes(source_after)
            command(['docker', 'build', '--build-arg', 'BASE_IMAGE=' + base_tag,
                     '--build-arg', 'RUNTIME_USER=' + previous['runtime_user'],
                     '-t', IMAGE, str(context)], timeout=300)
        built = json.loads(command(['docker', 'image', 'inspect', IMAGE]).stdout)[0]['Id']
        wait_idle(folo)
        verify_changes(changes)
        for item, after in changes[:2]:
            apply_change(item, after)
            applied = True
        compose_validate(folo)
        compose_up(folo)
        state = healthy()
        if state['image_id'] != built:
            raise DeploymentError('The fixed container image did not start')
        runtime = runtime_check(record['after_sha256'])
        next_original, next_index = update_original(original, original_index, source_path, compose_path,
                                                    source_after, compose_after, state, runtime, fix_id, report_path)
        # Record the expected new metadata before writing it so partial-write
        # recovery can recognize both states without accepting arbitrary edits.
        changes[2] = (changes[2][0], encoded(next_original))
        changes[3] = (changes[3][0], encoded(next_index))
        for item, after in changes[2:]:
            item['after_sha256'] = sha(after)
        index['changes'] = [r for r, _ in changes]
        atomic(index_path, encoded(index))
        for item, after in changes[2:]:
            apply_change(item, after)
        report.update(status='deployed_pending_live_validation', container=state, runtime=runtime,
                      full_v2_rollback_preserved=True)
        atomic(report_path, encoded(report))
        return report
    except Exception as error:
        failures = []
        if applied:
            try:
                restore(index)
                compose_up(folo, pinned_image=previous['image_id'])
                if healthy()['image_id'] != previous['image_id']:
                    raise DeploymentError('Previous image was not restored')
            except Exception as restoration:
                failures.append(type(restoration).__name__)
        report.update(status=('rolled_back' if applied else 'not_applied') if not failures else 'rollback_needs_attention',
                      error_type=type(error).__name__, rollback_errors=failures)
        atomic(report_path, encoded(report))
        raise DeploymentError('Audiofix deployment failed; ' + report['status']) from None


def load_hotfix(path):
    path = path.resolve(strict=True)
    report = json.loads(raw(path))
    backup = Path(report['backup']).resolve(strict=True)
    if report.get('format_version') != 1 or backup.parent != path.parent or not backup.name.startswith('backup-'):
        raise DeploymentError('Invalid audiofix rollback report')
    index = json.loads(raw(backup / 'index.json'))
    folo = Path(report['folotoy_root']).resolve(strict=True)
    original_path = Path(report['rollout_report']).resolve(strict=True)
    if not all(re.fullmatch(r'[0-9]+\.before', r.get('backup_file', '')) for r in index['changes']):
        raise DeploymentError('Invalid audiofix rollback backup names')
    original_before = next((r for r in index['changes'] if r['path'] == str(original_path)), None)
    if original_before is None:
        raise DeploymentError('Missing original deployment backup')
    before = json.loads(raw(backup / original_before['backup_file']))
    original_index = Path(before['backup']).resolve(strict=True) / 'index.json'
    expected = {str(folo / 'sdk/stt_sdk/volcengine_asr2.py'), str(folo / 'compose.ws.yaml'),
                str(original_path), str(original_index)}
    if (index.get('fix_id') != report.get('fix_id') or index.get('backup') != str(backup)
            or index.get('folotoy_root') != str(folo)
            or {r['path'] for r in index['changes']} != expected
            or not re.fullmatch(r'sha256:[a-f0-9]{64}', index.get('old_image_id', ''))):
        raise DeploymentError('Invalid audiofix rollback targets')
    if not all(re.fullmatch(r'[0-9]+\.before', r['backup_file']) for r in index['changes']):
        raise DeploymentError('Invalid audiofix rollback backup names')
    return report, index, folo


def rollback(path):
    report, index, folo = load_hotfix(path)
    wait_idle(folo)
    restore(index)
    compose_validate(folo)
    compose_up(folo, pinned_image=index['old_image_id'])
    state = healthy()
    if state['image_id'] != index['old_image_id']:
        raise DeploymentError('Audiofix rollback image verification failed')
    report.update(status='rolled_back', container=state, original_asr2_metadata_restored=True)
    atomic(path, encoded(report))
    return report


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--payload', required=True, type=Path)
    parser.add_argument('--rollout-report', type=Path, default=DEFAULT_ROLLOUT)
    parser.add_argument('--root', type=Path, default=DEFAULT_ROOT)
    args = parser.parse_args(argv)
    os.umask(0o077)
    try:
        print(json.dumps(deploy(args), ensure_ascii=False), flush=True)
        return 0
    except DeploymentError as error:
        print(json.dumps({'status': 'failed', 'error': str(error)}), flush=True)
        return 1


if __name__ == '__main__':
    raise SystemExit(main())
