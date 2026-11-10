"""Deploy the committed server while retaining bound data and rollback evidence."""
import argparse
from contextlib import closing
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import sqlite3
import subprocess
import tarfile
import tempfile

ROOT = Path('/home/gen/deerflow-deploy')
TASK = ROOT / 'code-update-20261110-a7a030f95550'
BACKUP = TASK / 'backup'
COMMIT = 'a7a030f95550814842f97848170cbc3472ce51db'
BUILD_ID = 'git-' + COMMIT[:12]
BUILDER = 'deerflow-build-' + COMMIT[:12]
STAGE = 'deerflow-stage-' + COMMIT[:12]
IMAGE = 'deerflow-server:git-' + COMMIT[:12]
BASE_TAG = 'deerflow-server:before-' + COMMIT[:12]
PRIMARY = '/var/lib/deerflow/user-data/config/go-harness-state'
ALTERNATE = ROOT / 'data/user-data/data/go-harness'
ARCHIVE = BACKUP / 'inactive-go-harness'
CHANGED = ('deerflow-acpd.real', 'deerflow-config-go', 'deerflow-control-go',
           'deerflow-skill-go', 'deerflow-acp-go', 'deerflow-acp', 'waku-daemon', 'deerflow-channel-wechat')
RETIRED = ('skill_evolution', 'skill_evolution_migration_ack', 'skill_evolution_migration')
TABLES = ['harness_sessions', 'harness_runs', 'harness_events', 'harness_inputs',
          'harness_tool_receipts', 'eino_session_events', 'eino_checkpoints',
          'harness_session_configs', 'harness_channels', 'df_skill_proposals',
          'df_skill_maintenance_jobs', 'df_skill_maintenance_calls']
IMMUTABLE = {'harness_events': ['session_id', 'run_id', 'event'],
             'harness_inputs': ['run_id', 'content'],
             'eino_session_events': ['session_id', 'event_id', 'kind', 'payload']}
util = storage = None


def require(value, message):
    if not value:
        raise RuntimeError(message)


def digest(path):
    with Path(path).open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def read(name):
    return json.loads((TASK / name).read_text())


def save(name, value):
    util.atomic(TASK / name, json.dumps(value, indent=2, sort_keys=True) + '\n')


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def initialize():
    global util, storage
    spec, manifest = read('deployment-spec.json'), read('source-manifest.json')
    require(manifest['commit'] == spec['commit'] == COMMIT, 'source commit differs')
    support = ROOT / 'skill-learning-recovery-20261028/release.py'
    previous = ROOT / 'storage-binding-20261103/release.py'
    require(digest(support) == spec['release_support_sha256'], 'backup support changed')
    require(digest(previous) == spec['storage_support_sha256'], 'storage support changed')
    util = load('committed_release_support', support)
    storage = load('committed_storage_support', previous)
    storage.util = util
    storage.TASK, storage.BACKUP = TASK, BACKUP
    storage.BASE, storage.BASE_TAG = spec['base_image_id'], BASE_TAG
    storage.IMAGE, storage.BUILD_ID = IMAGE, BUILD_ID
    storage.BUILDER, storage.STAGE, storage.CHANGED = BUILDER, STAGE, CHANGED
    storage.EXISTING = tuple(dict.fromkeys((*storage.EXISTING, 'deerflow-acp-go', 'deerflow-acpd', 'deerflow-server-entrypoint')))
    util.BUILD_ID, util.STAGE, util.BACKUP = BUILD_ID, STAGE, BACKUP


def prepare():
    require(util.inspect()['Image'] == storage.BASE, 'production image changed')
    util.run(['docker', 'image', 'tag', storage.BASE, BASE_TAG])
    util.atomic(TASK / 'candidate-wrapper.sh', util.WRAPPER.read_bytes(), mode=0o755)
    manifest = read('source-manifest.json')
    manifest['candidate_wrapper_sha256'] = digest(TASK / 'candidate-wrapper.sh')
    save('source-manifest.json', manifest)
    result = storage.prepare()
    save('provider-baseline.json', {'sha256': digest(util.CONFIG.with_name('search-providers.json'))})
    util.run(['docker', 'cp', util.CONTAINER + ':/opt/deerflow/bin/deerflow-server-entrypoint', str(TASK / 'entrypoint-baseline.sh')])
    require(digest(TASK / 'entrypoint-baseline.sh') == read('baseline.json')['binaries']['deerflow-server-entrypoint'], 'entrypoint changed')
    result['commit'] = COMMIT
    save('prepare-report.json', result)
    return result


def build_image():
    baseline = storage.unchanged()
    state = util.inspect(BUILDER)['State']
    require(not state['Running'] and state['ExitCode'] == 0 and not state['OOMKilled'], 'Linux build or tests failed')
    # Docker sends stderr separately; preserve both Rust and Go test output.
    captured = subprocess.run(['docker', 'logs', BUILDER], text=True, capture_output=True, check=True)
    logs = captured.stdout + captured.stderr
    util.atomic(TASK / 'linux-tests.log', logs)
    require('COMMITTED_LINUX_VALIDATION_COMPLETE' in logs, 'Linux validation incomplete')
    require('--- FAIL:' not in logs and 'test result: FAILED' not in logs, 'Linux regression failed')
    manifest = read('source-manifest.json')
    extra = read('extra-artifacts.json')
    require(extra['commit'] == COMMIT, 'additional artifact commit differs')
    expected_go = {**manifest['binaries'], **extra['binaries']}
    linux_tests = {}
    for name in manifest['linux_test_plan']:
        require('LINUX_PACKAGE_END ' + name in logs, 'missing Linux package: ' + name)
        inventory = (TASK / 'build/tests' / (name + '.inventory')).read_text().splitlines()
        names = {line for line in inventory if re.fullmatch(r'(Test|Example|Fuzz)\w+', line)}
        chunk = logs.split('LINUX_PACKAGE_BEGIN ' + name + '\n', 1)[1].split('LINUX_PACKAGE_END ' + name + '\n', 1)[0]
        completed = set(re.findall(r'^--- (?:PASS|SKIP): (\S+)', chunk, re.MULTILINE))
        require(names and names <= completed, 'Linux inventory incomplete: ' + name)
        linux_tests[name] = {'passed': len(re.findall(r'^--- PASS:', chunk, re.MULTILINE)),
                             'skipped': len(re.findall(r'^--- SKIP:', chunk, re.MULTILINE)), 'inventory': len(names)}
    context = Path(tempfile.mkdtemp(prefix='image-', dir=TASK))
    expected = dict(baseline['binaries'])
    for name in CHANGED:
        source = TASK / 'build/bin' / name
        if name in expected_go:
            require(digest(source) == expected_go[name], 'Go executable changed')
        shutil.copy2(source, context / name)
        expected[name] = digest(source)
    shutil.copy2(storage.candidate_wrapper(), context / 'deerflow-acpd')
    expected['deerflow-acpd'] = digest(context / 'deerflow-acpd')
    for name, relative in [('default-config.yaml', 'resources/default-config.yaml'),
                           ('search-plugins.md', 'docs/search-plugins.md'),
                           ('model-output-limits.md', 'docs/model-output-limits.md')]:
        shutil.copy2(TASK / 'build/source' / relative, context / name)
    require(storage.image_id(BASE_TAG) == storage.BASE, 'base image alias changed')
    lines = ['FROM ' + BASE_TAG, 'USER root']
    lines += ['COPY --chmod=0755 ' + name + ' /opt/deerflow/bin/' + name for name in (*CHANGED, 'deerflow-acpd')]
    lines += ['COPY --chmod=0644 ' + name + ' /opt/deerflow/resources/' + name
              for name in ('default-config.yaml', 'search-plugins.md', 'model-output-limits.md')]
    labels = {'cc.sprwhisp.deerflow.rollout': BUILD_ID, 'cc.sprwhisp.deerflow.waku-protocol': '12',
              'cc.sprwhisp.deerflow.source-state': 'clean-committed-tree',
              'cc.sprwhisp.deerflow.source-revision': COMMIT,
              'cc.sprwhisp.deerflow.source-manifest-sha256': digest(TASK / 'source-manifest.json'),
              'org.opencontainers.image.revision': COMMIT}
    lines += ['LABEL ' + key + '=' + json.dumps(value) for key, value in labels.items()]
    lines += ['USER deerflow:deerflow']
    (context / 'Dockerfile').write_text('\n'.join(lines) + '\n')
    with (TASK / 'image-build.log').open('w') as output:
        subprocess.run(['docker', 'build', '--network', 'none', '--pull=false', '-t', IMAGE, str(context)],
                       stdout=output, stderr=subprocess.STDOUT, check=True)
    counts = re.findall(r'test result: ok\. (\d+) passed; (\d+) failed; (\d+) ignored', logs)
    result = {'passed': True, 'commit': COMMIT, 'image_tag': IMAGE, 'image_id': storage.image_id(IMAGE),
              'base_image_id': storage.BASE, 'binaries': expected, 'labels': labels,
              'linux_tests': linux_tests, 'source_archive_sha256': manifest['source_archive_sha256'],
              'rust_tests': {'passed': sum(int(x[0]) for x in counts), 'failed': sum(int(x[1]) for x in counts),
                             'ignored': sum(int(x[2]) for x in counts)}}
    save('build-report.json', result)
    return result


def sqlite_backup(source, destination):
    destination.parent.mkdir(parents=True, exist_ok=True)
    with closing(sqlite3.connect(source.as_uri() + '?mode=ro', uri=True)) as src, closing(sqlite3.connect(destination)) as dst:
        src.backup(dst)
        require(dst.execute('PRAGMA quick_check').fetchone()[0] == 'ok', 'backup integrity failed')
    destination.chmod(0o600)


def config_check(before, after):
    original = util.load_yaml(before)
    expected = {key: value for key, value in original.items() if key not in RETIRED}
    require(util.load_yaml(after) == expected, 'configuration changed beyond retired evolution settings')
    return [key for key in RETIRED if key in original]


def migration_check(config, database):
    journal = json.loads(config.with_name('.skill-evolution-migration.json').read_text())
    require(journal['state'] == 'complete' and journal['version'] == 'skill-evolution-v2', 'migration incomplete')
    with closing(sqlite3.connect(database.as_uri() + '?mode=ro', uri=True)) as db:
        row = db.execute('SELECT policy_version,pipeline_epoch,migration_id,input_watermark FROM df_evolution_epoch WHERE id=1').fetchone()
        require(row == (journal['version'], journal['pipeline_epoch'], journal['migration_id'], journal['input_watermark']), 'migration identity differs')
    return {'complete': True, 'policy_version': journal['version'], 'input_watermark': journal['input_watermark']}


def resume(container, database):
    sessions = storage.historical_sessions(database)
    result = storage.native_resume(container, sessions, min(2, len(sessions)))
    ids = result.pop('resumed_original_ids')
    result['resumed_original_count'] = len(ids)
    result['resumed_ids_sha256'] = hashlib.sha256(json.dumps(ids).encode()).hexdigest()
    return result


def stage():
    storage.unchanged()
    build = read('build-report.json')
    area = TASK / 'stage'
    require(not area.exists(), 'stage already exists')
    config_source = util.CONFIG.parent
    data = area / 'data'
    config = data / 'user-data/config/config.yaml'
    config.parent.mkdir(parents=True, mode=0o700)
    shutil.copytree(config_source, config.parent, dirs_exist_ok=True, symlinks=True,
                    ignore=shutil.ignore_patterns('go-harness-state', '.config-tool.lock'))
    primary = config.parent / 'go-harness-state'
    shutil.copytree(util.DB.parent, primary, symlinks=True, ignore=shutil.ignore_patterns('harness.db*'))
    snapshot = area / 'before.db'
    sqlite_backup(util.DB, snapshot)
    shutil.copy2(snapshot, primary / 'harness.db')
    shutil.copytree(ROOT / 'skills', data / 'user-data/skills', symlinks=True)
    desktop_source = ROOT / 'data/user-data/desktop/app.db'
    if desktop_source.is_file():
        sqlite_backup(desktop_source, data / 'user-data/desktop/app.db')
    token = secrets.token_urlsafe(32)
    environment = dict(os.environ, WAKU_DAEMON_TOKEN=token)
    before_config = config.read_bytes()
    before_providers = config.with_name('search-providers.json').read_bytes()
    started = False
    try:
        util.run(['docker', 'run', '-d', '--pull=never', '--name', STAGE,
                  '--network', 'none', '--user', '1000:1000', '--cpus', '2', '--memory', '3g',
                  '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges:true',
                  '-e', 'WAKU_DAEMON_TOKEN', '-v', str(data) + ':/var/lib/deerflow',
                  '-v', str(storage.candidate_wrapper()) + ':/opt/deerflow/bin/deerflow-acpd:ro',
                  build['image_id']], env=environment)
        started = True
        util.wait_healthy(STAGE)
        require(storage.status(STAGE)['runtime']['build_id'] == BUILD_ID, 'stage runtime version differs')
        require(storage.hashes(STAGE, build['binaries']) == build['binaries'], 'stage binaries differ')
        plugin = json.loads(util.run(['docker', 'exec', '-i', STAGE, '/opt/deerflow/bin/deerflow-channel-wechat'],
                    payload=json.dumps({'jsonrpc': '2.0', 'id': 1, 'method': 'initialize', 'params': {'protocol': 1}}) + '\n'))
        require(plugin['result']['id'] == 'wechat' and plugin['result']['independent_poll'], 'bundled channel handshake failed')
        removed = config_check(before_config, config.read_bytes())
        require(config.with_name('search-providers.json').read_bytes() == before_providers, 'stage provider credentials changed')
        migration = migration_check(config, primary / 'harness.db')
        workspaces = sorted({cwd for _, cwd in storage.historical_sessions(snapshot)})
        require(all(Path(cwd).is_relative_to('/workspaces') for cwd in workspaces), 'unexpected historical workspace')
        util.run(['docker', 'exec', STAGE, 'mkdir', '-p', '--', *workspaces])
        restored = resume(STAGE, snapshot)
        util.run(['docker', 'restart', '-t', '90', STAGE], timeout=120)
        util.wait_healthy(STAGE)
        require(migration_check(config, primary / 'harness.db') == migration, 'restart changed migration boundary')
        resume(STAGE, snapshot)
        folo = util.inspect('folotoy-ws-folotoy-1')['Image']
        probe = subprocess.run(['docker', 'run', '--rm', '-i', '--network', 'container:' + STAGE,
                               '-e', 'WAKU_DAEMON_TOKEN', '--entrypoint', 'python', folo, '-B', '-'],
                              input=(TASK / 'ws_probe.py').read_text(), text=True, capture_output=True,
                              env=environment, timeout=150)
        util.atomic(area / 'websocket-probe.log', probe.stdout + probe.stderr)
        require(probe.returncode == 0, 'stage websocket/apply probe failed')
        rpc = json.loads(probe.stdout)
        resume(STAGE, snapshot)
        config_check(before_config, config.read_bytes())
        require(config.with_name('search-providers.json').read_bytes() == before_providers, 'configuration apply changed provider credentials')
        require(migration_check(config, primary / 'harness.db') == migration, 'apply changed migration boundary')
        records = storage.preserve_rows(snapshot, primary / 'harness.db', TABLES, immutable=IMMUTABLE)
        mounts = util.inspect(STAGE)['Mounts']
        require(len(mounts) == 2 and all(Path(m['Source']).is_relative_to(TASK) for m in mounts), 'production path mounted in stage')
        result = {'passed': True, 'commit': COMMIT, 'image_id': build['image_id'], 'binaries': build['binaries'],
                  'production_data_mounted': False, 'network': 'none', 'restart_resume': restored,
                  'configuration_apply': rpc, 'migration': migration, 'retired_config_sections': removed,
                  'provider_credentials_preserved': True, 'records': records}
        save('stage-report.json', result)
        return result
    finally:
        if started:
            logs = subprocess.run(['docker', 'logs', STAGE], text=True, capture_output=True)
            util.atomic(area / 'container.log', logs.stdout + logs.stderr)
            util.run(['docker', 'stop', '-t', '90', STAGE], timeout=120)
            util.run(['docker', 'rm', STAGE])


def verify():
    from ws_probe import probe
    baseline, build = read('baseline.json'), read('build-report.json')
    util.wait_healthy()
    state = util.inspect()
    require(state['Image'] == build['image_id'], 'production image differs')
    require(storage.mounts(state['Mounts']) == baseline['mounts'] and state['HostConfig']['PortBindings'] == baseline['ports'], 'mounts or ports changed')
    require(storage.hashes(util.CONTAINER, build['binaries']) == build['binaries'], 'installed binary checksum differs')
    labels = json.loads(util.run(['docker', 'image', 'inspect', state['Image']]))[0]['Config']['Labels']
    require(labels['org.opencontainers.image.revision'] == COMMIT, 'image commit differs')
    require(digest(storage.BINDING) == baseline['binding_sha256'], 'database binding changed')
    for name in ('compose', 'wrapper'):
        require(digest(Path(baseline['files'][name]['path'])) == baseline['files'][name]['sha256'], name + ' changed')
    require(util.ENV.read_bytes() == util.image_env((BACKUP / 'env').read_bytes(), build['image_id']), 'unexpected environment change')
    removed = config_check((BACKUP / 'config').read_bytes(), util.CONFIG.read_bytes())
    require(util.CONFIG.with_name('search-providers.json').read_bytes() == (BACKUP / 'search-providers.json').read_bytes(), 'provider credentials changed')
    require(storage.status()['runtime']['build_id'] == BUILD_ID, 'native build differs')
    migration = migration_check(util.CONFIG, util.DB)
    token = next(value.split('=', 1)[1] for value in state['Config']['Env'] if value.startswith('WAKU_DAEMON_TOKEN='))
    websocket = probe(token, 'ws://100.80.205.120:18787/v1', apply=False)
    restored = resume(util.CONTAINER, BACKUP / 'harness.db')
    records = storage.preserve_rows(BACKUP / 'harness.db', util.DB, TABLES, immutable=IMMUTABLE)
    if (BACKUP / 'desktop.db').is_file():
        records['desktop'] = storage.preserve_rows(BACKUP / 'desktop.db', ROOT / 'data/user-data/desktop/app.db', ['sessions', 'messages', 'turns'])
    require(not ALTERNATE.exists(), 'inactive database reappeared')
    require(storage.tree_hashes(ARCHIVE) == read('alternate-hashes.json'), 'inactive archive changed')
    require(storage.others() == read('other-containers.json'), 'unrelated containers changed')
    result = {'passed': True, 'commit': COMMIT, 'build_id': BUILD_ID, 'image_tag': IMAGE, 'image_id': state['Image'],
              'health': state['State']['Health']['Status'], 'container_id': state['Id'],
              'data_dir': PRIMARY, 'database_binding_unchanged': True, 'configuration_preserved': True,
              'retired_config_sections': removed, 'migration': migration,
              'credentials_preserved': True, 'websocket': websocket, 'native_resume': restored,
              'records': records, 'inactive_database_archive': str(ARCHIVE), 'inactive_archive_preserved': True,
              'backup': str(BACKUP), 'other_containers_preserved': True, 'production_prompts_submitted': 0}
    save('production-report.json', result)
    return result


def rollback():
    state = util.inspect()
    require(state['Image'] in (storage.BASE, read('build-report.json')['image_id']), 'unrelated production image')
    if state['State']['Running']:
        current = util.management('daemon.status')
        if current.get('ok'):
            util.frontend_idle(util.control('daemon.drain'))
            util.maintenance_idle(util.database_summary(), include_queued=False)
    util.run(util.compose('stop', '-t', '90', 'deerflow'), timeout=120)
    original = (BACKUP / 'config').read_bytes()
    if util.CONFIG.read_bytes() != original:
        config_check(original, util.CONFIG.read_bytes())
        util.atomic(util.CONFIG, original)
    util.atomic(util.ENV, (BACKUP / 'env').read_bytes())
    # Keep every live and archived database; rollback never copies old DB bytes.
    util.run(util.compose('up', '-d', '--no-deps', '--no-build', 'deerflow'), timeout=180)
    util.wait_healthy()
    require(util.inspect()['Image'] == storage.BASE, 'rollback image differs')
    util.control('daemon.resume')
    save('rollback-report.json', {'passed': True, 'business_data_restored': False})


def apply():
    storage.unchanged()
    build, staged = read('build-report.json'), read('stage-report.json')
    require(staged['passed'] and staged['image_id'] == build['image_id'] and staged['binaries'] == build['binaries'], 'candidate not staged')
    require(not BACKUP.exists(), 'backup already exists')
    require(digest(util.CONFIG.with_name('search-providers.json')) == read('provider-baseline.json')['sha256'], 'provider store changed')
    require(ALTERNATE.is_dir() and not ALTERNATE.is_symlink(), 'inactive database path unexpected')
    util.run(util.compose('config', '--quiet'))
    util.frontend_idle(storage.status()['data'])
    util.maintenance_idle(util.database_summary(), include_queued=False)
    stopped = False
    try:
        util.frontend_idle(util.control('daemon.drain'))
        util.maintenance_idle(util.database_summary(), include_queued=False)
        storage.unchanged()
        save('other-containers.json', storage.others())
        BACKUP.mkdir(mode=0o700)
        for name, path in [('env', util.ENV), ('compose', util.COMPOSE), ('config', util.CONFIG),
                           ('wrapper', util.WRAPPER), ('runtime-data.json', storage.BINDING),
                           ('search-providers.json', util.CONFIG.with_name('search-providers.json'))]:
            util.atomic(BACKUP / name, path.read_bytes())
        stopped = True
        util.run(util.compose('stop', '-t', '90', 'deerflow'), timeout=120)
        require(not util.inspect()['State']['Running'], 'production did not stop')
        for name, path in [('user-data', ROOT / 'data/user-data'), ('skills', ROOT / 'skills')]:
            with tarfile.open(BACKUP / (name + '.tar.gz'), 'x:gz', dereference=False) as archive:
                archive.add(path, arcname=name)
            (BACKUP / (name + '.tar.gz')).chmod(0o600)
        for name, path in [('harness.db', util.DB), ('desktop.db', ROOT / 'data/user-data/desktop/app.db')]:
            if path.is_file():
                sqlite_backup(path, BACKUP / name)
        before_alternate = storage.tree_hashes(ALTERNATE)
        save('alternate-hashes.json', before_alternate)
        storage.unchanged()
        # The new resolver requires the inactive database outside every runtime path.
        # Rename the complete stopped directory on the same filesystem and verify it.
        require(ALTERNATE.resolve().is_relative_to(ROOT / 'data/user-data'), 'inactive path escaped')
        require(ARCHIVE.parent.resolve() == BACKUP.resolve() and not ARCHIVE.exists(), 'archive target unexpected')
        ALTERNATE.rename(ARCHIVE)
        require(storage.tree_hashes(ARCHIVE) == before_alternate, 'inactive archive checksum differs')
        checked = json.loads(util.run(['docker', 'run', '--rm', '--network', 'none', '--user', '1000:1000',
            '-v', str(ROOT / 'data') + ':/var/lib/deerflow:ro', '--entrypoint', '/opt/deerflow/bin/deerflow-acpd.real',
            build['image_id'], '--check-paths', '--config', '/var/lib/deerflow/user-data/config/config.yaml']))
        require(checked['data_dir'] == PRIMARY, 'candidate selected a different database')
        util.atomic(util.ENV, util.image_env((BACKUP / 'env').read_bytes(), build['image_id']))
        util.run(util.compose('up', '-d', '--no-deps', '--no-build', 'deerflow'), timeout=180)
        result = verify()
        save('deployment-report.json', result)
        return result
    except Exception:
        if stopped:
            try:
                rollback()
            except Exception as error:
                save('recovery-attention.json', {'rollback_deferred': True, 'error_type': type(error).__name__, 'business_data_restored': False})
        else:
            util.management('daemon.resume')
        raise


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['prepare', 'build-image', 'stage', 'apply', 'verify', 'rollback'])
    action = parser.parse_args().action
    initialize()
    print(json.dumps(globals()[action.replace('-', '_')](), indent=2))
