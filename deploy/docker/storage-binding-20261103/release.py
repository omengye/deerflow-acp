"""Scoped Gentoo storage rollout. Backups are never restored over business data."""
import argparse
from contextlib import closing
import hashlib
import importlib.util
import json
import os
from pathlib import Path, PurePosixPath
import queue
import re
import secrets
import shutil
import sqlite3
import subprocess
import tarfile
import tempfile
import threading
import time

ROOT = Path('/home/gen/deerflow-deploy')
TASK = ROOT / 'storage-binding-20261103'
BACKUP = TASK / 'backup'
BASE = 'sha256:90e46bb01bd79c2d79d330190e419b798417e630f21f606079d9a5defa210cf9'
BASE_TAG = 'deerflow-server:artifact-ready-20261031-fe4c659c'
IMAGE = 'deerflow-server:storage-binding-20261103-6586c1f0'
BUILD_ID = 'storage-binding-20261103-6586c1f0'
BUILDER = 'deerflow-build-storage-20261103'
STAGE = 'deerflow-storage-stage-20261103'
PRIMARY = '/var/lib/deerflow/user-data/config/go-harness-state'
BINDING = ROOT / 'data/user-data/config/runtime-data.json'
CHANGED = ('deerflow-acpd.real', 'deerflow-acp-go', 'deerflow-acp', 'waku-daemon')
EXISTING = ('deerflow-acpd.real', 'deerflow-config-go', 'deerflow-control-go',
            'deerflow-skill-go', 'deerflow-acp', 'waku-daemon', 'deerflow-server-healthcheck')
util = None


def require(value, message):
    if not value:
        raise RuntimeError(message)


def digest(path):
    with path.open('rb') as handle:
        return hashlib.file_digest(handle, 'sha256').hexdigest()


def binding_for(path):
    require(isinstance(path, str) and path.startswith('/'), 'database path must be absolute')
    relative = PurePosixPath(path).relative_to('/var/lib/deerflow/user-data')
    require(relative.parts and '..' not in relative.parts, 'invalid portable database path')
    return (json.dumps({'data_dir': str(relative)}, separators=(',', ':')) + '\n').encode()


def initialize():
    global util
    # Reuse the repository's tested backup, draining and control primitives.
    support = Path(__file__).resolve().parent.parent / 'skill-learning-recovery-20261028/release.py'
    require(digest(support) == read('source-manifest.json')['release_support_sha256'], 'release support changed')
    spec = importlib.util.spec_from_file_location('storage_release_support', support)
    util = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(util)
    util.BUILD_ID, util.STAGE, util.BACKUP = BUILD_ID, STAGE, BACKUP


def read(name):
    return json.loads((TASK / name).read_text())


def save(name, value):
    util.atomic(TASK / name, json.dumps(value, indent=2, sort_keys=True) + '\n')


def image_id(image):
    value = util.run(['docker', 'image', 'inspect', '--format', '{{.Id}}', image]).strip()
    require(re.fullmatch('sha256:[a-f0-9]{64}', value), 'invalid image identity')
    return value


def others():
    return [entry for entry in util.other_containers() if entry['name'] != BUILDER]


def hashes(container, names):
    output = util.run(['docker', 'exec', container, 'sha256sum', *['/opt/deerflow/bin/' + name for name in names]])
    return {Path(line.split(None, 1)[1].strip()).name: line.split(None, 1)[0] for line in output.splitlines()}


def mounts(records):
    fields = ('Type', 'Source', 'Destination', 'RW', 'Mode', 'Propagation')
    return sorted([{field: item.get(field) for field in fields} for item in records],
                  key=lambda item: item['Destination'])


def tree_hashes(root):
    require(not root.is_symlink(), 'alternate database directory is a symlink')
    return {'exists': root.exists(), 'files': {
        path.relative_to(root).as_posix(): digest(path)
        for path in sorted(root.rglob('*')) if path.is_file()}}


def candidate_wrapper():
    path = TASK / 'candidate-wrapper.sh'
    require(digest(path) == read('source-manifest.json')['candidate_wrapper_sha256'], 'candidate wrapper changed')
    return path


def status(container=None):
    result = util.management('daemon.status', container or util.CONTAINER)
    require(result.get('ok') and result.get('runtime', {}).get('data_dir') == PRIMARY, 'running database mismatch')
    return result


def historical_sessions(path):
    with closing(sqlite3.connect(path.as_uri() + '?mode=ro', uri=True)) as db:
        # Native retention uses empty TEXT timestamps, not SQL NULL. Background
        # child sessions must not be attached as foreground conversations.
        return list(db.execute("SELECT s.id,s.cwd FROM harness_sessions s "
            "WHERE s.closed_at='' AND s.archived_at='' AND NOT EXISTS "
            "(SELECT 1 FROM harness_background_specs b WHERE b.child_session_id=s.id) "
            "ORDER BY s.updated_at DESC LIMIT 30"))


def prepare():
    require(not (TASK / 'baseline.json').exists(), 'already prepared')
    state = util.inspect()
    require(state['Image'] == BASE and image_id(BASE_TAG) == BASE, 'base image changed')
    util.check_mounts(state)
    running = status()
    candidate_wrapper()
    sessions = historical_sessions(util.DB)
    require(sessions, 'no historical sessions available to verify')
    candidate = binding_for(running['runtime']['data_dir'])
    require(not BINDING.exists() or BINDING.read_bytes() == candidate, 'existing binding differs')
    files = {name: {'path': str(path), 'sha256': digest(path)} for name, path in
             [('env', util.ENV), ('compose', util.COMPOSE), ('config', util.CONFIG), ('wrapper', util.WRAPPER)]}
    baseline = {'base_image_id': BASE, 'files': files, 'binding_sha256': digest(BINDING) if BINDING.exists() else None,
                'binaries': hashes(util.CONTAINER, EXISTING), 'mounts': mounts(state['Mounts']),
                'ports': state['HostConfig']['PortBindings'], 'database': util.database_summary(),
                'historical_candidates': len(sessions)}
    util.atomic(TASK / 'candidate-binding.json', candidate)
    save('baseline.json', baseline)
    return {'passed': True, 'data_dir': PRIMARY, 'database': baseline['database']}


def unchanged(allow_established_binding=False):
    baseline = read('baseline.json')
    state = util.inspect()
    require(state['Image'] == BASE, 'production image changed')
    require(mounts(state['Mounts']) == baseline['mounts'] and
            state['HostConfig']['PortBindings'] == baseline['ports'], 'production mounts/ports changed')
    for record in baseline['files'].values():
        require(digest(Path(record['path'])) == record['sha256'], 'production configuration changed')
    current_binding = digest(BINDING) if BINDING.exists() else None
    if current_binding != baseline['binding_sha256']:
        require(allow_established_binding and baseline['binding_sha256'] is None and
                BINDING.read_bytes() == binding_for(PRIMARY), 'binding changed')
    return baseline


def retry_prepare():
    require(read('rollback-report.json').get('passed'), 'successful rollback required')
    unchanged(allow_established_binding=True)
    status()
    require(historical_sessions(util.DB), 'no historical sessions available to verify')
    require(tree_hashes(ROOT / 'data/user-data/data/go-harness') == read('alternate-hashes.json'),
            'alternate database changed after rollback')
    preserved = preserve_rows(BACKUP / 'harness.db', util.DB,
                             ['harness_sessions', 'harness_events', 'harness_tool_receipts'])
    archive = Path(tempfile.mkdtemp(prefix='previous-attempt-', dir=TASK))
    # Archive only this rollout's backup and metadata. Never move live data.
    for name in ('backup', 'baseline.json', 'alternate-hashes.json',
                 'other-containers.json', 'rollback-report.json'):
        (TASK / name).rename(archive / name)
    result = prepare()
    result.update(previous_attempt=str(archive), rollback_records_preserved=preserved)
    save('retry-report.json', result)
    return result


def build_image():
    baseline = unchanged()
    builder = util.inspect(BUILDER)['State']
    require(not builder['Running'] and builder['ExitCode'] == 0 and not builder['OOMKilled'], 'build/tests not successful')
    logs = util.run(['docker', 'logs', BUILDER])
    util.atomic(TASK / 'linux-tests.log', logs)
    for required in ('--- PASS: TestExistingRustBridgePortableDataBinding',
                     '--- PASS: TestPortableBindingAcrossLaunchers',
                     'data_directory_binding_survives_restart_with_both_stores_intact ... ok',
                     'deerflow_ready_survives_normal_prompt_invalidation ... ok'):
        require(required in logs, 'missing regression test: ' + required)
    require('--- FAIL:' not in logs and 'test result: FAILED' not in logs, 'test failures')
    context = Path(tempfile.mkdtemp(prefix='image-', dir=TASK))
    expected = dict(baseline['binaries'])
    for name in CHANGED:
        source = TASK / 'build/bin' / name
        shutil.copy2(source, context / name)
        expected[name] = digest(source)
    shutil.copy2(candidate_wrapper(), context / 'deerflow-acpd')
    expected['deerflow-acpd'] = digest(context / 'deerflow-acpd')
    require(image_id(BASE_TAG) == BASE, 'base tag changed')
    lines = ['FROM ' + BASE_TAG, 'USER root']
    lines += ['COPY --chmod=0755 ' + name + ' /opt/deerflow/bin/' + name for name in (*CHANGED, 'deerflow-acpd')]
    lines += ['LABEL cc.sprwhisp.deerflow.rollout="storage-binding-20261103" cc.sprwhisp.deerflow.waku-protocol="12"',
              'LABEL org.opencontainers.image.revision="6586c1f074141908913344411d20ad516901ae86"', 'USER deerflow:deerflow']
    (context / 'Dockerfile').write_text('\n'.join(lines) + '\n')
    with (TASK / 'image-build.log').open('w') as output:
        subprocess.run(['docker', 'build', '--network', 'none', '--pull=false', '-t', IMAGE, str(context)],
                       stdout=output, stderr=subprocess.STDOUT, check=True)
    result = {'passed': True, 'image_id': image_id(IMAGE), 'base_image_id': BASE, 'binaries': expected,
              'source_manifest_sha256': digest(TASK / 'source-manifest.json'), 'protocol': 12}
    counts = re.findall(r'test result: ok\. (\d+) passed; (\d+) failed; (\d+) ignored', logs)
    result['rust_tests'] = {'passed': sum(int(row[0]) for row in counts),
                            'failed': sum(int(row[1]) for row in counts),
                            'ignored': sum(int(row[2]) for row in counts)}
    result['go_tests_passed'] = len(re.findall(r'^--- PASS:', logs, re.MULTILINE))
    save('build-report.json', result)
    return result


class ACP:
    def __init__(self, container):
        self.proc = subprocess.Popen(['docker', 'exec', '-i', container, '/var/lib/deerflow/deerflow-acp',
            '--no-auto-start', '--config', '/var/lib/deerflow/user-data/config/config.yaml',
            '--runtime-dir', util.RUNTIME], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL, text=True, encoding='utf-8')
        self.queue, self.serial = queue.Queue(), 0
        def consume():
            for line in self.proc.stdout:
                self.queue.put(json.loads(line))
            self.queue.put(None)
        threading.Thread(target=consume, daemon=True).start()
        require('result' in self.call('initialize', {'protocolVersion': 1, 'clientCapabilities': {}}), 'ACP initialization failed')

    def call(self, method, params):
        self.serial += 1
        self.proc.stdin.write(json.dumps({'jsonrpc': '2.0', 'id': self.serial, 'method': method, 'params': params}) + '\n')
        self.proc.stdin.flush()
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            value = self.queue.get(timeout=max(0.01, deadline - time.monotonic()))
            require(value is not None, 'ACP exited')
            if value.get('id') == self.serial and 'method' not in value:
                return value
        raise TimeoutError('ACP request timeout')

    def close(self):
        self.proc.stdin.close()
        try:
            self.proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            self.proc.terminate()
            self.proc.wait(timeout=10)


def native_resume(container, records, needed):
    require(needed > 0 and len(records) >= needed, 'no historical sessions available to verify')
    client, restored, occupied = ACP(container), [], 0
    try:
        for sid, cwd in records:
            result = client.call('session/resume', {'sessionId': sid, 'cwd': cwd, 'mcpServers': []})
            if 'result' in result:
                restored.append(sid)
            elif result.get('error', {}).get('message') == 'Session is attached to another connection':
                occupied += 1
            else:
                raise RuntimeError('Original native session could not be resumed')
            if len(restored) >= needed:
                break
    finally:
        client.close()
    require(len(restored) >= needed, 'insufficient detached historical sessions verified')
    return {'passed': True, 'resumed_original_ids': restored, 'occupied_skipped': occupied, 'prompts_submitted': 0}


def stage():
    unchanged()
    build = read('build-report.json')
    stage_root = Path(tempfile.mkdtemp(prefix='stage-', dir=TASK))
    data = stage_root / 'data'
    primary = data / 'user-data/config/go-harness-state'
    alternate = data / 'user-data/data/go-harness/harness.db'
    primary.mkdir(parents=True)
    (primary / 'harness.db').touch()
    alternate.parent.mkdir(parents=True)
    alternate.write_bytes(b'alternate historical database must remain untouched')
    util.atomic(data / 'user-data/config/runtime-data.json', binding_for(PRIMARY))
    util.atomic(data / 'user-data/config/config.yaml', 'config_version: eino-harness-v1\nsubagents:\n  enabled: false\nlocal_acp:\n  session_cleanup_enabled: false\nmodels:\n  - name: offline-stage\n    provider: openai\n    model: offline-stage\n    api_key: synthetic-no-credential\n    base_url: http://127.0.0.1:9/v1\n')
    env = dict(os.environ, WAKU_DAEMON_TOKEN=secrets.token_urlsafe(32))
    started = False
    try:
        util.run(['docker', 'run', '-d', '--name', STAGE, '--network', 'none', '--user', '1000:1000',
            '--cpus', '2', '--memory', '3g', '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges:true',
            '-e', 'WAKU_DAEMON_TOKEN', '-v', str(data) + ':/var/lib/deerflow',
            '-v', str(candidate_wrapper()) + ':/opt/deerflow/bin/deerflow-acpd:ro', build['image_id']], env=env)
        started = True
        util.wait_healthy(STAGE)
        require(hashes(STAGE, build['binaries']) == build['binaries'], 'staged binary mismatch')
        require(status(STAGE)['runtime']['build_id'] == BUILD_ID, 'staged native build mismatch')
        client = ACP(STAGE)
        try:
            created = client.call('session/new', {'cwd': '/workspaces', 'mcpServers': []})
            sid = created['result']['sessionId']
        finally:
            client.close()
        util.run(['docker', 'restart', '-t', '90', STAGE], timeout=120)
        util.wait_healthy(STAGE)
        native_resume(STAGE, [(sid, '/workspaces')], 1)
        folo = util.inspect('folotoy-ws-folotoy-1')['Image']
        probe_result = subprocess.run(['docker', 'run', '--rm', '-i', '--network', 'container:' + STAGE,
            '-e', 'WAKU_DAEMON_TOKEN', '--entrypoint', 'python', folo, '-B', '-'],
            input=(TASK / 'ws_probe.py').read_text(), text=True, capture_output=True, env=env, timeout=120)
        util.atomic(stage_root / 'probe.log', probe_result.stdout + probe_result.stderr)
        require(probe_result.returncode == 0, 'isolated websocket probe failed; see stage probe.log')
        probe = json.loads(probe_result.stdout)
        restored = native_resume(STAGE, [(sid, '/workspaces')], 1)
        require(alternate.read_bytes() == b'alternate historical database must remain untouched', 'alternate store changed')
        require(status(STAGE)['runtime']['data_dir'] == PRIMARY, 'apply changed database')
        state = util.inspect(STAGE)
        require(state['HostConfig']['NetworkMode'] == 'none' and len(state['Mounts']) == 2, 'staging isolation changed')
        result = {'passed': True, 'image_id': build['image_id'], 'binaries': build['binaries'],
                  'restart_resume': True, 'config_apply': probe, 'resume_after_apply': restored,
                  'alternate_database_preserved': True, 'production_data_mounted': False}
        save('stage-report.json', result)
        return result
    except Exception:
        if started:
            with (stage_root / 'failure.log').open('wb') as output:
                os.chmod(output.name, 0o600)
                subprocess.run(['docker', 'logs', '--tail', '200', STAGE],
                               stdout=output, stderr=subprocess.STDOUT, timeout=20, check=False)
        raise
    finally:
        if started:
            util.run(['docker', 'stop', '-t', '90', STAGE], timeout=120)
            util.run(['docker', 'rm', STAGE])


def preserve_rows(before, after, tables, immutable=None):
    results = {}
    with closing(sqlite3.connect(after.as_uri() + '?mode=ro', uri=True)) as db:
        db.execute('ATTACH DATABASE ? AS previous', (before.as_uri() + '?mode=ro',))
        require(db.execute('PRAGMA quick_check').fetchone()[0] == 'ok', 'database integrity failed')
        existing = {row[0] for row in db.execute("SELECT name FROM previous.sqlite_master WHERE type='table'")}
        for table in tables:
            if table not in existing:
                continue
            require(re.fullmatch('[a-z_]+', table), 'invalid table name')
            keys = [row[1] for row in db.execute('PRAGMA previous.table_info(' + table + ')') if row[5]]
            require(keys, 'table has no identity: ' + table)
            match = ' AND '.join('a."' + key + '"=b."' + key + '"' for key in keys)
            missing = db.execute('SELECT count(*) FROM previous.' + table + ' b WHERE NOT EXISTS (SELECT 1 FROM main.' + table + ' a WHERE ' + match + ')').fetchone()[0]
            require(missing == 0, 'historical records missing: ' + table)
            results[table] = {'before': db.execute('SELECT count(*) FROM previous.' + table).fetchone()[0],
                             'after': db.execute('SELECT count(*) FROM main.' + table).fetchone()[0], 'missing': missing}
            columns = (immutable or {}).get(table, [])
            if columns:
                require(all(re.fullmatch('[a-z_]+', column) for column in columns), 'invalid column name')
                differs = ' OR '.join('a."' + column + '" IS NOT b."' + column + '"' for column in columns)
                changed = db.execute('SELECT count(*) FROM previous.' + table + ' b JOIN main.' + table +
                                     ' a ON ' + match + ' WHERE ' + differs).fetchone()[0]
                require(changed == 0, 'historical payload changed: ' + table)
                results[table]['payloads_changed'] = changed
    return results


def verify(apply_config=False):
    from ws_probe import probe
    build, baseline = read('build-report.json'), read('baseline.json')
    util.wait_healthy()
    state = util.inspect()
    require(state['Image'] == build['image_id'], 'production image mismatch')
    require(mounts(state['Mounts']) == baseline['mounts'] and state['HostConfig']['PortBindings'] == baseline['ports'], 'mounts/ports changed')
    require(hashes(util.CONTAINER, build['binaries']) == build['binaries'], 'installed binaries differ')
    require(BINDING.read_bytes() == binding_for(PRIMARY), 'binding changed')
    for name in ('config', 'compose'):
        item = baseline['files'][name]
        require(digest(Path(item['path'])) == item['sha256'], name + ' changed')
    require(util.ENV.read_bytes() == util.image_env((BACKUP / 'env').read_bytes(), build['image_id']), 'unexpected environment change')
    token = next(x.split('=', 1)[1] for x in state['Config']['Env'] if x.startswith('WAKU_DAEMON_TOKEN='))
    current = status()
    require(current['runtime']['build_id'] == BUILD_ID, 'wrong native build')
    if apply_config:
        util.frontend_idle(current['data'])
        util.maintenance_idle(util.database_summary(), include_queued=False)
    websocket = probe(token, 'ws://100.80.205.120:18787/v1', apply=apply_config)
    require(status()['runtime']['data_dir'] == PRIMARY, 'apply switched database')
    sessions = historical_sessions(BACKUP / 'harness.db')
    resume = native_resume(util.CONTAINER, sessions, min(2, len(sessions)))
    records = preserve_rows(BACKUP / 'harness.db', util.DB, ['harness_sessions', 'harness_runs', 'harness_events',
        'harness_inputs', 'harness_tool_receipts', 'eino_session_events', 'eino_checkpoints',
        'harness_session_configs', 'harness_channels'], immutable={
            'harness_events': ['session_id', 'run_id', 'event'],
            'harness_inputs': ['run_id', 'content'],
            'eino_session_events': ['session_id', 'event_id', 'kind', 'payload']})
    desktop = ROOT / 'data/user-data/desktop/app.db'
    if (BACKUP / 'desktop.db').exists():
        records['desktop'] = preserve_rows(BACKUP / 'desktop.db', desktop, ['sessions', 'messages', 'turns'])
    require(tree_hashes(ROOT / 'data/user-data/data/go-harness') == read('alternate-hashes.json'),
            'alternate database directory changed')
    require(others() == read('other-containers.json'), 'unrelated containers changed')
    result = {'passed': True, 'image_id': state['Image'], 'health': state['State']['Health']['Status'],
              'data_dir': PRIMARY, 'protocol': 12, 'websocket': websocket, 'native_resume': resume,
              'records': records, 'alternate_database_preserved': True, 'other_containers_preserved': True}
    save('production-report.json', result)
    return result


def rollback():
    build = read('build-report.json')
    state = util.inspect()
    require(state['Image'] in (BASE, build['image_id']), 'unrelated production image')
    if state['State']['Running']:
        try:
            util.frontend_idle(util.control('daemon.drain'))
            util.maintenance_idle(util.database_summary(), include_queued=False)
        except Exception:
            util.management('daemon.resume')
            raise
    util.run(util.compose('stop', '-t', '90', 'deerflow'), timeout=120)
    util.atomic(util.ENV, (BACKUP / 'env').read_bytes())
    util.atomic(util.WRAPPER, (BACKUP / 'wrapper').read_bytes(), mode=0o755)
    # Retain the validated binding; never restore an old business DB or skills.
    util.run(util.compose('up', '-d', '--no-deps', '--no-build', 'deerflow'), timeout=180)
    util.wait_healthy()
    require(util.inspect()['Image'] == BASE, 'rollback image mismatch')
    util.control('daemon.resume')
    save('rollback-report.json', {'passed': True, 'business_data_restored': False})


def restart():
    verify()
    util.frontend_idle(status()['data'])
    try:
        util.frontend_idle(util.control('daemon.drain'))
        util.maintenance_idle(util.database_summary(), include_queued=False)
        util.run(util.compose('restart', '-t', '90', 'deerflow'), timeout=120)
        result = verify()
        save('restart-report.json', result)
        return result
    finally:
        util.management('daemon.resume')


def apply():
    baseline, build, staged = unchanged(), read('build-report.json'), read('stage-report.json')
    require(staged.get('passed') and staged['image_id'] == build['image_id'] and staged['binaries'] == build['binaries'], 'unverified candidate')
    require(digest(candidate_wrapper()) == build['binaries']['deerflow-acpd'], 'staged wrapper changed')
    require(not BACKUP.exists(), 'backup already exists')
    util.run(util.compose('config', '--quiet'))
    util.frontend_idle(status()['data'])
    util.maintenance_idle(util.database_summary(), include_queued=False)
    stopped = False
    try:
        util.frontend_idle(util.control('daemon.drain'))
        util.maintenance_idle(util.database_summary(), include_queued=False)
        unchanged()
        save('other-containers.json', others())
        BACKUP.mkdir(mode=0o700)
        for name, path in [('env', util.ENV), ('compose', util.COMPOSE), ('config', util.CONFIG), ('wrapper', util.WRAPPER)]:
            util.atomic(BACKUP / name, path.read_bytes())
        if BINDING.exists():
            util.atomic(BACKUP / 'runtime-data.json', BINDING.read_bytes())
        stopped = True
        util.run(util.compose('stop', '-t', '90', 'deerflow'), timeout=120)
        require(not util.inspect()['State']['Running'], 'service did not stop')
        for name, path in [('user-data', ROOT / 'data/user-data'), ('skills', ROOT / 'skills')]:
            with tarfile.open(BACKUP / (name + '.tar.gz'), 'x:gz', dereference=False) as archive:
                archive.add(path, arcname=name)
            (BACKUP / (name + '.tar.gz')).chmod(0o600)
        for name, path in [('harness.db', util.DB), ('desktop.db', ROOT / 'data/user-data/desktop/app.db')]:
            if path.is_file():
                with closing(sqlite3.connect(path.as_uri() + '?mode=ro', uri=True)) as source, closing(sqlite3.connect(BACKUP / name)) as target:
                    source.backup(target)
                    require(target.execute('PRAGMA quick_check').fetchone()[0] == 'ok', 'backup integrity failed')
                (BACKUP / name).chmod(0o600)
        alternate = ROOT / 'data/user-data/data/go-harness'
        save('alternate-hashes.json', tree_hashes(alternate))
        unchanged()
        util.atomic(BINDING, binding_for(PRIMARY))
        checked = json.loads(util.run(['docker', 'run', '--rm', '--network', 'none', '--user', '1000:1000',
            '-v', str(ROOT / 'data') + ':/var/lib/deerflow:ro', '--entrypoint', '/opt/deerflow/bin/deerflow-acpd.real',
            build['image_id'], '--check-paths', '--config', '/var/lib/deerflow/user-data/config/config.yaml']))
        require(checked['data_dir'] == PRIMARY, 'candidate preflight selected another store')
        util.atomic(util.WRAPPER, candidate_wrapper().read_bytes(), mode=0o755)
        util.atomic(util.ENV, util.image_env((BACKUP / 'env').read_bytes(), build['image_id']))
        util.run(util.compose('up', '-d', '--no-deps', '--no-build', 'deerflow'), timeout=180)
        result = verify(apply_config=True)
        save('deployment-report.json', result)
        return result
    except Exception:
        if stopped:
            try:
                rollback()
            except Exception:
                save('recovery-attention.json', {'rollback_deferred': True, 'business_data_restored': False})
        else:
            util.management('daemon.resume')
        raise


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['prepare', 'retry-prepare', 'build-image', 'stage', 'apply', 'verify', 'restart', 'rollback'])
    action = parser.parse_args().action
    initialize()
    print(json.dumps({'prepare': prepare, 'retry-prepare': retry_prepare, 'build-image': build_image, 'stage': stage, 'apply': apply,
                      'verify': verify, 'restart': restart, 'rollback': rollback}[action](), indent=2))
