"""Two-stage FoloToy ASR 2.0 rollout. Credentials enter stage1 through stdin.

All Docker/YAML output stays captured because existing deployment files can
contain credentials. No DeerFlow configuration or database is changed.
"""
import argparse
from contextlib import closing
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import time
import uuid

CONTAINER = 'folotoy-ws-folotoy-1'
IMAGE = 'folotoy-deerflow-ws:asr2-20261025'
RESOURCE = 'volc.seedasr.sauc.duration'
DEFAULT_ROOT = Path('/home/gen/deerflow-deploy/asr2-rollout-20261025')
DEFAULT_FOLO = Path('/home/gen/Documents/folotoy/deerflow-ws/src')
SOURCE_FILES = ('src/config.py', 'src/util/config_util.py', 'src/sdk/stt.py',
                'src/sdk/stt_sdk/volcengine_asr2.py')


class DeploymentError(Exception):
    """Safe diagnostics only, never the contents of provider exceptions."""


def sha(data):
    return hashlib.sha256(data).hexdigest() if data is not None else None


def bytes_at(path):
    if path.is_symlink():
        raise DeploymentError('Refusing a linked deployment file')
    return path.read_bytes() if path.is_file() else None


def atomic(path, data, mode=0o600):
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, name = tempfile.mkstemp(prefix='.asr2-', dir=path.parent)
    try:
        with os.fdopen(fd, 'wb') as output:
            output.write(data)
            output.flush()
            os.fsync(output.fileno())
        os.chmod(name, mode)
        os.replace(name, path)
    finally:
        Path(name).unlink(missing_ok=True)


def json_write(path, value):
    atomic(path, (json.dumps(value, ensure_ascii=False, indent=2) + '\n').encode())


def command(argv, *, payload=None, cwd=None, timeout=60, check=True):
    env = dict(os.environ)
    env.pop('FOLOTOY_WS_IMAGE', None)
    result = subprocess.run(argv, input=payload, capture_output=True, text=True,
                            cwd=cwd, env=env, timeout=timeout)
    if check and result.returncode:
        raise DeploymentError('Command failed: ' + argv[0] + ' exit ' + str(result.returncode))
    return result


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
        state = inspect()
        if state['status'] == 'running' and state['health'] == 'healthy':
            return state
        time.sleep(2)
    raise DeploymentError('FoloToy health check deadline exceeded')


def wait_idle(folo, timeout=120):
    path = folo / 'data/deerflow-sessions.sqlite3'
    if not path.is_file():
        raise DeploymentError('Cannot establish current voice session activity')
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        with closing(sqlite3.connect(path.resolve().as_uri() + '?mode=ro', uri=True, timeout=2)) as db:
            active = db.execute("SELECT count(*) FROM deerflow_sessions WHERE status IN ('running','pending','cancelling','in_progress')").fetchone()[0]
        if active == 0:
            return
        time.sleep(2)
    raise DeploymentError('Active voice requests did not become idle')


def compose_argv(folo):
    return ['docker', 'compose', '--env-file', str(folo / '.env.ws'), '-p', 'folotoy-ws',
            '--project-directory', str(folo), '-f', str(folo / 'compose.ws.yaml')]


def compose_validate(folo):
    command(compose_argv(folo) + ['config', '--quiet'], cwd=folo, timeout=30)


def compose_up(folo, *, pinned_image=None):
    temporary = None
    argv = compose_argv(folo)
    try:
        if pinned_image:
            temporary = folo / ('.asr2-rollback-' + uuid.uuid4().hex + '.yaml')
            atomic(temporary, ('services:\n  folotoy:\n    image: ' + pinned_image + '\n').encode())
            argv += ['-f', str(temporary)]
        command(argv + ['up', '-d', '--no-build', '--force-recreate', 'folotoy'],
                cwd=folo, timeout=120)
    finally:
        if temporary:
            temporary.unlink(missing_ok=True)


YAML_CANDIDATE = r'''
import copy,json,sys,yaml
request=json.load(sys.stdin);before=yaml.safe_load(request['raw']);after=copy.deepcopy(before)
service=after['services']['folotoy'];service['image']=request['image']
environment=service.get('environment') or {}
if not isinstance(environment,dict):raise ValueError('Expected environment mapping')
if any(key in environment for key in ('VOLCENGINE_ASR2_API_KEY','VOLCENGINE_ASR2_RESOURCE_ID','VOLCENGINE_ASR2_TIMEOUT')):raise ValueError('Existing ASR2 environment needs review')
files=service.get('env_file') or []
if isinstance(files,str):files=[files]
if not isinstance(files,list):raise ValueError('Expected env_file sequence')
files=copy.deepcopy(files)
if request['private_env'] in files:raise ValueError('Private ASR2 environment already bound')
files.append(request['private_env']);service['env_file']=files
text=yaml.safe_dump(after,allow_unicode=True,sort_keys=False)
if yaml.safe_load(text)!=after:raise ValueError('YAML roundtrip mismatch')
print(json.dumps({'raw':text}))
'''


def compose_candidate(raw, private_env):
    result = command(['docker', 'exec', '-i', CONTAINER, 'python3', '-c', YAML_CANDIDATE],
                     payload=json.dumps({'raw': raw.decode(), 'private_env': str(private_env), 'image': IMAGE}), timeout=30)
    return json.loads(result.stdout)['raw'].encode()


def roles_candidate(raw, selected):
    roles = json.loads(raw)
    if not isinstance(roles, dict) or not all(isinstance(roles.get(str(i)), dict) for i in range(1, 8)):
        raise DeploymentError('Expected role objects 1 through 7')
    for number in selected:
        roles[str(number)]['stt_type'] = 'volcengine-asr2'
    return (json.dumps(roles, ensure_ascii=False, indent=2) + '\n').encode()


def secret_input():
    try:
        value = json.load(sys.stdin)
    except (ValueError, OSError):
        raise DeploymentError('Expected private credential JSON on stdin') from None
    key = value.get('api_key') if isinstance(value, dict) else None
    if not isinstance(key, str) or not re.fullmatch(r'[A-Za-z0-9._-]{16,4096}', key):
        raise DeploymentError('Invalid private API Key')
    label = value.get('key_label', 'provided_api_key')
    if not isinstance(label, str) or not re.fullmatch(r'[A-Za-z0-9_-]{1,100}', label):
        raise DeploymentError('Invalid public credential label')
    return key, label


def prepare_change(path, after, backup, number, mode=0o644):
    before = bytes_at(path)
    original_mode = path.stat().st_mode & 0o777 if before is not None else None
    filename = str(number) + '.before'
    if before is not None:
        atomic(backup / filename, before)
    return {'path': str(path), 'before_sha256': sha(before), 'after_sha256': sha(after),
            'before_mode': original_mode, 'after_mode': mode,
            'backup_file': filename if before is not None else None}, after


def verify_changes(changes):
    for record, _ in changes:
        if sha(bytes_at(Path(record['path']))) != record['before_sha256']:
            raise DeploymentError('Deployment target changed concurrently')


def restore_index(index, *, allow_original=True):
    backup = Path(index['backup'])
    changes = index['changes']
    # Validate every file before restoring any; never half-restore an external edit.
    for record in changes:
        current = sha(bytes_at(Path(record['path'])))
        allowed = (record['after_sha256'], record['before_sha256']) if allow_original else (record['after_sha256'],)
        if current not in allowed:
            raise DeploymentError('Rollback stopped because a deployment file changed externally')
        if record['backup_file'] is not None:
            saved = backup / record['backup_file']
            if sha(saved.read_bytes()) != record['before_sha256']:
                raise DeploymentError('Rollback backup digest mismatch')
    for record in reversed(changes):
        path = Path(record['path'])
        if record['backup_file'] is None:
            path.unlink(missing_ok=True)
        else:
            atomic(path, (backup / record['backup_file']).read_bytes(), record['before_mode'])


RUNTIME_CHECK = r'''
import importlib,json,logging,os
logging.disable(logging.CRITICAL)
from config import Config
from util.config_util import get_role_stt_config
config=Config();roles={};checked={}
for number in range(1,8):
 role=config.ROLES[str(number)];kind=role.get('stt_type',config.STT_TYPE);roles[str(number)]=kind
 if kind=='volcengine-asr2':
  options=get_role_stt_config(kind,role,config);checked[str(number)]={'resource_id':options.get('resource_id'),'api_key_present':bool(options.get('api_key')),'timeout':options.get('timeout')}
modules={name:importlib.import_module(name).__file__ for name in ('config','util.config_util','sdk.stt','sdk.stt_sdk.volcengine_asr2')}
print(json.dumps({'roles':roles,'new_provider_config':checked,'module_files':modules,'global_stt_type':config.STT_TYPE,'new_api_key_present':bool(os.environ.get('VOLCENGINE_ASR2_API_KEY')),'hotwords_enabled':False}))
'''


def runtime_check(expected_roles):
    process = command(['docker', 'exec', '-i', '-w', '/opt/folotoy', CONTAINER,
                       'python3', '-c', RUNTIME_CHECK], timeout=25)
    value = json.loads(process.stdout)
    for number in range(1, 8):
        current = value['roles'][str(number)]
        if number in expected_roles:
            config = value['new_provider_config'].get(str(number), {})
            if current != 'volcengine-asr2' or config.get('resource_id') != RESOURCE or not config.get('api_key_present'):
                raise DeploymentError('The selected role did not load ASR2.0 configuration')
        elif current == 'volcengine-asr2':
            raise DeploymentError('An unselected role changed to ASR2.0')
    if not all(path.endswith('.py') for path in value['module_files'].values()):
        raise DeploymentError('The new Python ASR dispatch was shadowed by native code')
    return value


def load_report(path):
    report = json.loads(path.read_text())
    if report.get('format_version') != 1 or not report.get('backup') or not report.get('folotoy_root'):
        raise DeploymentError('Invalid deployment report')
    index_path = Path(report['backup']) / 'index.json'
    index = json.loads(index_path.read_text())
    if index.get('deployment_id') != report.get('deployment_id'):
        raise DeploymentError('Deployment identity mismatch')
    folo = Path(report['folotoy_root']).resolve()
    backup = Path(report['backup']).resolve()
    if backup.parent != path.resolve().parent or not backup.name.startswith('backup-'):
        raise DeploymentError('Rollback backup escaped deployment directory')
    if index.get('backup') != str(backup) or index.get('folotoy_root') != str(folo):
        raise DeploymentError('Rollback target identity mismatch')
    allowed = {str((folo / relative).resolve()) for relative in
               ('compose.ws.yaml', 'roles-ws.json', 'private/asr2.env',
                *(str(Path(v).relative_to('src')) for v in SOURCE_FILES))}
    if {str(Path(v['path']).resolve()) for v in index['changes']} != allowed:
        raise DeploymentError('Rollback target set differs from reviewed deployment')
    if not re.fullmatch(r'sha256:[a-f0-9]{64}', index.get('old_image_id', '')):
        raise DeploymentError('Rollback image identity invalid')
    for record in index['changes']:
        if record.get('backup_file') is not None and not re.fullmatch(r'[0-9]+\.before', record['backup_file']):
            raise DeploymentError('Rollback backup filename invalid')
    return report, index, index_path


def stage1(args):
    key, key_label = secret_input()
    payload = args.payload.resolve(strict=True)
    folo = args.folotoy_root.resolve(strict=True)
    root = args.root.resolve()
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    root.chmod(0o700)
    report_path = root / 'deployment.json'
    if report_path.exists() and not args.allow_reapply:
        raise DeploymentError('Existing deployment report; review before reapply')
    manifest = json.loads((payload / 'folotoy-source/manifest.json').read_text())
    records = {v['path']: v for v in manifest['files']}
    if set(records) != set(SOURCE_FILES):
        raise DeploymentError('Unexpected production source manifest')
    for relative, record in records.items():
        source = payload / 'folotoy-source' / relative
        if source.is_symlink() or sha(source.read_bytes()) != record['after_sha256']:
            raise DeploymentError('Staged source digest mismatch')
        target = folo.joinpath(*Path(relative).parts[1:])
        if sha(bytes_at(target)) not in (record.get('before_sha256'), record['after_sha256']):
            raise DeploymentError('FoloToy source differs from reviewed versions')
    wait_idle(folo)
    previous = inspect()
    if previous['status'] != 'running' or previous['health'] != 'healthy':
        raise DeploymentError('Original FoloToy container is not healthy')
    deployment_id = uuid.uuid4().hex
    backup = root / ('backup-' + datetime.now(timezone.utc).strftime('%Y%m%dT%H%M%SZ') + '-' + deployment_id[:8])
    backup.mkdir(mode=0o700)
    private = folo / 'private'
    private.mkdir(mode=0o700, exist_ok=True)
    if private.is_symlink():
        raise DeploymentError('Private directory must not be linked')
    private.chmod(0o700)
    secret_path = private / 'asr2.env'
    if secret_path.exists():
        raise DeploymentError('Existing private ASR2 configuration needs review')
    compose_path, roles_path = folo / 'compose.ws.yaml', folo / 'roles-ws.json'
    compose = compose_candidate(bytes_at(compose_path), secret_path)
    roles = roles_candidate(bytes_at(roles_path), (1,))
    secret = ('VOLCENGINE_ASR2_API_KEY=' + key + '\nVOLCENGINE_ASR2_RESOURCE_ID=' + RESOURCE + '\nVOLCENGINE_ASR2_TIMEOUT=45\n').encode()
    changes = [prepare_change(compose_path, compose, backup, 0, 0o600),
               prepare_change(roles_path, roles, backup, 1, 0o600),
               prepare_change(secret_path, secret, backup, 2, 0o600)]
    for relative in SOURCE_FILES:
        changes.append(prepare_change(folo.joinpath(*Path(relative).parts[1:]),
                                      (payload / 'folotoy-source' / relative).read_bytes(), backup, len(changes)))
    index = {'format_version': 1, 'deployment_id': deployment_id, 'backup': str(backup),
             'changes': [v for v, _ in changes], 'old_image_id': previous['image_id'],
             'folotoy_root': str(folo)}
    index_path = backup / 'index.json'
    json_write(index_path, index)
    report = {'format_version': 1, 'deployment_id': deployment_id, 'status': 'preparing',
              'backup': str(backup), 'folotoy_root': str(folo), 'previous_container': previous,
              'payload_root': str(payload),
              'new_image': IMAGE, 'key_label': key_label, 'credentials_in_image': False,
              'hotwords_enabled': False, 'old_provider_retained': True,
              'manual_rollback': 'python3 ' + str(payload / 'rollback.py') + ' --deployment-report ' + str(report_path)}
    json_write(report_path, report)
    applied = False
    try:
        base_tag = 'folotoy-deerflow-ws:asr2-base-' + deployment_id[:12]
        command(['docker', 'image', 'tag', previous['image_id'], base_tag])
        report['pinned_base_tag'] = base_tag
        with tempfile.TemporaryDirectory(prefix='build-', dir=root) as directory:
            context = Path(directory)
            shutil.copyfile(payload / 'Dockerfile.folotoy', context / 'Dockerfile')
            for relative in SOURCE_FILES:
                target = context / 'folotoy-source' / relative
                target.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(payload / 'folotoy-source' / relative, target)
            command(['docker', 'build', '--build-arg', 'BASE_IMAGE=' + base_tag,
                     '--build-arg', 'RUNTIME_USER=' + previous['runtime_user'], '-t', IMAGE, str(context)], timeout=300)
        wait_idle(folo)
        verify_changes(changes)
        for record, after in changes:
            if sha(bytes_at(Path(record['path']))) != record['before_sha256']:
                raise DeploymentError('Deployment target changed concurrently')
            atomic(Path(record['path']), after, record['after_mode'])
            applied = True
        compose_validate(folo)
        compose_up(folo)
        report['container'] = healthy()
        report['runtime'] = runtime_check({1})
        report.update(status='role1_ready', role1_roles_sha256=sha(bytes_at(roles_path)),
                      active_asr2_roles=[1], stage2_requires_role1_acceptance=True)
        json_write(report_path, report)
        return report
    except Exception as error:
        failures = []
        if applied:
            try:
                restore_index(index)
                compose_up(folo, pinned_image=previous['image_id'])
                restored = healthy()
                if restored['image_id'] != previous['image_id']:
                    raise DeploymentError('Restored container image mismatch')
            except Exception as rollback_error:
                failures.append(type(rollback_error).__name__)
        report.update(status='rolled_back' if not failures else 'rollback_needs_attention',
                      failed_phase='stage1', error_type=type(error).__name__, rollback_errors=failures)
        json_write(report_path, report)
        raise DeploymentError('Stage1 failed; ' + report['status']) from None


def stage2(args):
    report, index, index_path = load_report(args.deployment_report)
    if report['status'] not in ('role1_ready', 'role1_ready_stage2_rolled_back'):
        raise DeploymentError('Stage2 requires a healthy role1 deployment')
    acceptance = json.loads(args.acceptance.read_text())
    if not (acceptance.get('passed') is True and str(acceptance.get('role')) == '1'
            and acceptance.get('provider') == 'volcengine-asr2'
            and acceptance.get('deployment_id') == report['deployment_id']):
        raise DeploymentError('Stage2 requires matching successful role1 acceptance')
    folo = Path(report['folotoy_root'])
    roles_path = folo / 'roles-ws.json'
    before = bytes_at(roles_path)
    if sha(before) != report['role1_roles_sha256']:
        raise DeploymentError('Roles changed after canary deployment')
    after = roles_candidate(before, range(2, 8))
    wait_idle(folo)
    healthy()
    # Save acceptance separately; it contains evidence identifiers, no Key.
    json_write(Path(report['backup']) / 'role1-acceptance.json', acceptance)
    try:
        if bytes_at(roles_path) != before:
            raise DeploymentError('Roles changed concurrently')
        atomic(roles_path, after, 0o600)
        for record in index['changes']:
            if record['path'] == str(roles_path):
                record['after_sha256'] = sha(after)
        json_write(index_path, index)
        compose_up(folo)
        report['container'] = healthy()
        report['runtime'] = runtime_check(set(range(1, 8)))
        report.update(status='all_roles_ready', active_asr2_roles=list(range(1, 8)),
                      role1_acceptance_passed=True, final_roles_sha256=sha(after))
        json_write(args.deployment_report, report)
        return report
    except Exception as error:
        if bytes_at(roles_path) != after:
            report.update(status='rollback_needs_attention', failed_phase='stage2', error_type=type(error).__name__)
            json_write(args.deployment_report, report)
            raise DeploymentError('Stage2 failure with concurrent role changes') from None
        atomic(roles_path, before, 0o600)
        for record in index['changes']:
            if record['path'] == str(roles_path):
                record['after_sha256'] = sha(before)
        json_write(index_path, index)
        compose_up(folo)
        report['container'] = healthy()
        report['runtime'] = runtime_check({1})
        report.update(status='role1_ready_stage2_rolled_back', active_asr2_roles=[1],
                      failed_phase='stage2', error_type=type(error).__name__)
        json_write(args.deployment_report, report)
        raise DeploymentError('Stage2 failed; restored the verified role1 rollout') from None


def rollback(report_path):
    report, index, _ = load_report(report_path)
    folo = Path(report['folotoy_root'])
    wait_idle(folo)
    restore_index(index)
    compose_validate(folo)
    compose_up(folo, pinned_image=index['old_image_id'])
    state = healthy()
    if state['image_id'] != index['old_image_id']:
        raise DeploymentError('Rollback did not start the original image')
    report.update(status='rolled_back', container=state, active_asr2_roles=[],
                  rollback_image_verified=True)
    json_write(report_path, report)
    return report


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='action', required=True)
    first = sub.add_parser('stage1')
    first.add_argument('--payload', type=Path, required=True)
    first.add_argument('--root', type=Path, default=DEFAULT_ROOT)
    first.add_argument('--folotoy-root', type=Path, default=DEFAULT_FOLO)
    first.add_argument('--allow-reapply', action='store_true')
    second = sub.add_parser('stage2')
    second.add_argument('--deployment-report', type=Path, required=True)
    second.add_argument('--acceptance', type=Path, required=True)
    args = parser.parse_args(argv)
    os.umask(0o077)
    try:
        report = stage1(args) if args.action == 'stage1' else stage2(args)
        print(json.dumps(report, ensure_ascii=False), flush=True)
        return 0
    except DeploymentError as error:
        print(json.dumps({'status': 'failed', 'error': str(error)}), flush=True)
        return 1


if __name__ == '__main__':
    raise SystemExit(main())
