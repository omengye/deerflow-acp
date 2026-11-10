"""Gentoo search plugin rollout, preserving the existing database and credentials."""
import argparse
from contextlib import closing
import copy
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
import tempfile
import time

ROOT = Path('/home/gen/deerflow-deploy')
TASK = ROOT / 'search-plugins-20261103-204617'
BUILD_ID = 'search-plugins-20261103-204617'
BUILDER = 'deerflow-build-search-20261103-204617'
STAGE = 'deerflow-search-stage-20261103-204617'
PROBE = 'deerflow-search-probe-20261103-204617'
IMAGE = 'deerflow-server:search-plugins-20261103-204617'
BASE_TAG = 'deerflow-server:search-base-20261103-204617'
CHANGED = ('deerflow-acpd.real', 'deerflow-config-go', 'deerflow-control-go',
           'deerflow-skill-go', 'deerflow-acp-go', 'deerflow-acp', 'waku-daemon')
storage = util = None


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def digest(path):
    with Path(path).open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def load_module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def initialize():
    global storage, util
    manifest = json.loads((TASK / 'source-manifest.json').read_text())
    for name, expected in manifest.get('deployment_scripts', {}).items():
        require(digest(TASK / name) == expected, 'deployment script changed: ' + name)
    support = ROOT / 'skill-learning-recovery-20261028/release.py'
    previous = ROOT / 'storage-binding-20261103/release.py'
    require(digest(support) == manifest['release_support_sha256'], 'backup support source changed')
    require(digest(previous) == manifest['storage_support_sha256'], 'storage support source changed')
    util = load_module('search_release_support', support)
    storage = load_module('search_storage_support', previous)
    spec = json.loads((TASK / 'deployment-spec.json').read_text())
    storage.util = util
    storage.TASK, storage.BACKUP = TASK, TASK / 'backup'
    storage.BASE, storage.BASE_TAG = spec['base_image_id'], BASE_TAG
    storage.IMAGE, storage.BUILD_ID = IMAGE, BUILD_ID
    storage.BUILDER, storage.STAGE = BUILDER, STAGE
    storage.CHANGED = CHANGED
    storage.EXISTING = tuple(dict.fromkeys((*storage.EXISTING, 'deerflow-acp-go', 'deerflow-acpd')))
    util.BUILD_ID, util.STAGE, util.BACKUP = BUILD_ID, STAGE, storage.BACKUP
    storage.verify = verify
    storage.rollback = rollback


def read(name):
    return json.loads((TASK / name).read_text())


def save(name, data):
    util.atomic(TASK / name, json.dumps(data, indent=2, sort_keys=True) + '\n')


def migrated_document(original):
    expected = copy.deepcopy(original)
    removed = []
    for tool in expected.get('tools', []):
        if tool.get('name') == 'web_search':
            for key in ('api_key', 'https_proxy', 'timeout'):
                if key in tool:
                    removed.append(key)
                    del tool[key]
    return expected, removed


def verify_config(before_raw, after_raw):
    expected, removed = migrated_document(util.load_yaml(before_raw))
    require(util.load_yaml(after_raw) == expected, 'configuration changed outside search migration')
    return removed


def verify_provider_store(before_raw, after_raw):
    after = json.loads(after_raw)['config']
    if before_raw is None:
        return
    before = json.loads(before_raw)['config']
    for key in ('default_provider', 'fetch_provider'):
        require(before.get(key) == after.get(key), 'saved search preference changed')
    for name, old in before.get('providers', {}).items():
        new = after['providers'][name]
        for key, value in old.items():
            # Unresolved automatic references may reuse the same provider's
            # documented old credential. Literal and explicitly cleared values
            # remain authoritative; values never enter a report.
            if key == 'api_key' and isinstance(value, str) and value.startswith('$'):
                continue
            require(new.get(key) == value, 'saved provider configuration changed')


def verify_legacy_search_store(config_raw, store_raw, environment):
    document = util.load_yaml(config_raw)
    configured = ((document.get('search') or {}).get('providers') or {}).get('brave')
    expected = dict(configured) if configured is not None else {'api_key': '${BRAVE_API_KEY:-}'}
    def resolve(value):
        for _ in range(8):
            if not isinstance(value, str) or not value.startswith('$'):
                return value
            if value.startswith('${') and value.endswith('}'):
                name, _, fallback = value[2:-1].partition(':-')
                value = environment.get(name) or fallback
            else:
                value = environment.get(value[1:], '')
        return value
    for tool in document.get('tools', []):
        if tool.get('name') != 'web_search':
            continue
        if not resolve(expected.get('api_key', '')) and tool.get('api_key'):
            expected['api_key'] = tool['api_key']
        for key in ('https_proxy', 'timeout'):
            if not expected.get(key) and tool.get(key):
                expected[key] = tool[key]
    actual = json.loads(store_raw)['config']['providers']['brave']
    for key, value in expected.items():
        if value:
            require(actual.get(key) == value, 'legacy search setting was not preserved')


def prepare():
    state = util.inspect()
    require(state['Image'] == read('deployment-spec.json')['base_image_id'], 'production image changed')
    util.run(['docker', 'image', 'tag', state['Image'], BASE_TAG])
    result = storage.prepare()
    return result


def build_image():
    baseline = storage.unchanged()
    state = util.inspect(BUILDER)['State']
    require(not state['Running'] and state['ExitCode'] == 0 and not state['OOMKilled'], 'Linux build/tests failed')
    logs = util.run(['docker', 'logs', BUILDER])
    util.atomic(TASK / 'linux-tests.log', logs)
    for marker in ('--- PASS: TestSearchPluginsSwitchInLiveRuntimeWithoutRestart',
                   '--- PASS: TestFirstInstallSearchPluginsInitializeSwitchAndPreserveConfig',
                   '--- PASS: TestLegacySearchMigrationPersistsBeforeRemovingFieldsAndIsIdempotent',
                   '--- PASS: TestExistingRustBridgePortableDataBinding',
                   '--- PASS: TestPortableBindingAcrossLaunchers',
                   'data_directory_binding_survives_restart_with_both_stores_intact ... ok',
                   'deerflow_ready_survives_normal_prompt_invalidation ... ok'):
        require(marker in logs, 'required Linux regression missing: ' + marker)
    require('--- FAIL:' not in logs and 'test result: FAILED' not in logs, 'Linux test failure')
    manifest = read('source-manifest.json')
    context = Path(tempfile.mkdtemp(prefix='image-', dir=TASK))
    expected = dict(baseline['binaries'])
    for name in CHANGED:
        path = TASK / 'build/bin' / name
        if name in manifest['go_artifacts']:
            require(digest(path) == manifest['go_artifacts'][name], 'Go artifact changed after test')
        shutil.copy2(path, context / name)
        expected[name] = digest(path)
    shutil.copy2(storage.candidate_wrapper(), context / 'deerflow-acpd')
    expected['deerflow-acpd'] = digest(context / 'deerflow-acpd')
    shutil.copy2(TASK / 'build/source/resources/default-config.yaml', context / 'default-config.yaml')
    shutil.copy2(TASK / 'build/source/docs/search-plugins.md', context / 'search-plugins.md')
    require(storage.image_id(BASE_TAG) == storage.BASE, 'base tag changed')
    lines = ['FROM ' + BASE_TAG, 'USER root']
    lines += ['COPY --chmod=0755 ' + name + ' /opt/deerflow/bin/' + name for name in (*CHANGED, 'deerflow-acpd')]
    lines += ['COPY --chmod=0644 default-config.yaml /opt/deerflow/resources/default-config.yaml',
              'COPY --chmod=0644 search-plugins.md /opt/deerflow/resources/search-plugins.md',
              'LABEL cc.sprwhisp.deerflow.rollout="' + BUILD_ID + '" cc.sprwhisp.deerflow.waku-protocol="12"',
              'USER deerflow:deerflow']
    (context / 'Dockerfile').write_text('\n'.join(lines) + '\n')
    with (TASK / 'image-build.log').open('w') as output:
        subprocess.run(['docker', 'build', '--network', 'none', '--pull=false', '-t', IMAGE, str(context)],
                       stdout=output, stderr=subprocess.STDOUT, check=True)
    counts = re.findall(r'test result: ok\. (\d+) passed; (\d+) failed; (\d+) ignored', logs)
    result = {'passed': True, 'image_id': storage.image_id(IMAGE), 'base_image_id': storage.BASE,
              'binaries': expected, 'protocol': 12, 'source_manifest_sha256': digest(TASK / 'source-manifest.json'),
              'default_config_sha256': digest(context / 'default-config.yaml'),
              'rust_tests': {'passed': sum(int(x[0]) for x in counts), 'failed': sum(int(x[1]) for x in counts),
                             'ignored': sum(int(x[2]) for x in counts)},
              'go_tests_passed': len(re.findall(r'^--- PASS:', logs, re.MULTILINE))}
    save('build-report.json', result)
    return result


def probe_line(process, request=None):
    if request is not None:
        process.stdin.write(json.dumps(request) + '\n')
        process.stdin.flush()
    line = process.stdout.readline()
    require(line, 'isolated search probe exited')
    return json.loads(line)


def stage_search(token):
    environment = dict(os.environ, WAKU_DAEMON_TOKEN=token)
    image = util.inspect('folotoy-ws-folotoy-1')['Image']
    before = storage.status(STAGE)['runtime']['instance_id']
    container_before = util.inspect(STAGE)['State']['StartedAt']
    code = (TASK / 'stage_search_probe.py').read_text()
    with (TASK / 'stage-search-probe.stderr').open('w') as errors:
        process = subprocess.Popen(['docker', 'run', '--rm', '-i', '--name', PROBE,
            '--network', 'container:' + STAGE, '--memory', '512m', '--pids-limit', '128',
            '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges:true',
            '-e', 'WAKU_DAEMON_TOKEN', '--entrypoint', 'python', image, '-u', '-c', code],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=errors, text=True, env=environment)
        client = None
        try:
            require(probe_line(process).get('ready'), 'fixture server not ready')
            require(set(probe_line(process, {'action': 'configure'})['configured']) == {'brave', 'deepseek', 'searxng'},
                    'three providers were not configured through Waku RPC')
            client = storage.ACP(STAGE)
            created = client.call('session/new', {'cwd': '/workspaces', 'mcpServers': []})
            session = created['result']['sessionId']
            expected = {'brave': 0, 'deepseek': 0, 'searxng': 0}
            sequence = ['brave', 'deepseek', 'searxng', 'brave']
            for name in sequence:
                selected = probe_line(process, {'action': 'select', 'provider': name})
                require(selected.get('default_provider') == name, 'search selection failed')
                result = client.call('session/prompt', {'sessionId': session,
                    'prompt': [{'type': 'text', 'text': 'Search the web using my saved provider'}]})
                require('result' in result, 'synthetic search did not finish')
                expected[name] += 1
                observed = probe_line(process, {'action': 'inspect'})
                require(not observed['errors'] and observed['counts'] == expected, 'the wrong provider executed')
                require(storage.status(STAGE)['runtime']['instance_id'] == before, 'daemon restarted during a search switch')
                require(util.inspect(STAGE)['State']['StartedAt'] == container_before, 'container restarted during a search switch')
            return {'passed': True, 'provider_sequence': sequence, 'actual_http_requests': expected,
                    'same_native_session': True, 'same_daemon_instance': True, 'same_container': True,
                    'search_settings_changed_through_waku_rpc': True, 'credentials': 'synthetic_only',
                    'production_data_mounted': False, 'paid_model_requests': 0}
        finally:
            if client is not None:
                client.close()
            if process.poll() is None:
                try:
                    process.stdin.write('{"action":"close"}\n')
                    process.stdin.flush()
                    process.wait(timeout=15)
                except Exception:
                    util.run(['docker', 'rm', '-f', PROBE], accepted_codes=(0, 1))
                    process.wait(timeout=15)


def stage():
    storage.unchanged()
    build = read('build-report.json')
    stage_root = Path(tempfile.mkdtemp(prefix='stage-', dir=TASK))
    data = stage_root / 'data'
    primary = data / 'user-data/config/go-harness-state'
    primary.mkdir(parents=True)
    (primary / 'harness.db').touch()
    alternate = data / 'user-data/data/go-harness/harness.db'
    alternate.parent.mkdir(parents=True)
    alternate.write_bytes(b'keep the alternate historical database untouched')
    util.atomic(data / 'user-data/config/runtime-data.json', storage.binding_for(storage.PRIMARY))
    configuration = '''config_version: eino-harness-v1
subagents:
  enabled: false
skills:
  enabled: false
skill_evolution:
  enabled: false
memory:
  enabled: false
local_acp:
  session_cleanup_enabled: false
  permission_mode: off
models:
  - name: offline-stage
    provider: openai
    model: offline-stage
    api_key: synthetic-no-credential
    base_url: http://127.0.0.1:39743/v1
tools:
  - name: web_search
    max_results: 5
'''
    util.atomic(data / 'user-data/config/config.yaml', configuration)
    token = secrets.token_urlsafe(32)
    environment = dict(os.environ, WAKU_DAEMON_TOKEN=token)
    started = False
    try:
        util.run(['docker', 'run', '-d', '--name', STAGE, '--network', 'none', '--user', '1000:1000',
            '--cpus', '2', '--memory', '3g', '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges:true',
            '-e', 'WAKU_DAEMON_TOKEN', '-v', str(data) + ':/var/lib/deerflow',
            '-v', str(storage.candidate_wrapper()) + ':/opt/deerflow/bin/deerflow-acpd:ro', build['image_id']], env=environment)
        started = True
        util.wait_healthy(STAGE)
        require(storage.hashes(STAGE, build['binaries']) == build['binaries'], 'staged binary mismatch')
        require(storage.status(STAGE)['runtime']['build_id'] == BUILD_ID, 'staged native build mismatch')
        client = storage.ACP(STAGE)
        try:
            created = client.call('session/new', {'cwd': '/workspaces', 'mcpServers': []})
            sid = created['result']['sessionId']
        finally:
            client.close()
        util.run(['docker', 'restart', '-t', '90', STAGE], timeout=120)
        util.wait_healthy(STAGE)
        storage.native_resume(STAGE, [(sid, '/workspaces')], 1)
        folo_image = util.inspect('folotoy-ws-folotoy-1')['Image']
        apply_probe = subprocess.run(['docker', 'run', '--rm', '-i', '--network', 'container:' + STAGE,
            '-e', 'WAKU_DAEMON_TOKEN', '--entrypoint', 'python', folo_image, '-B', '-'],
            input=(TASK / 'ws_probe.py').read_text(), text=True, capture_output=True, env=environment, timeout=150)
        util.atomic(stage_root / 'apply-probe.log', apply_probe.stdout + apply_probe.stderr)
        require(apply_probe.returncode == 0, 'isolated configuration apply probe failed')
        apply_result = json.loads(apply_probe.stdout)
        search_result = stage_search(token)
        require(alternate.read_bytes() == b'keep the alternate historical database untouched', 'alternate store changed')
        require(storage.status(STAGE)['runtime']['data_dir'] == storage.PRIMARY, 'database binding changed')
        state = util.inspect(STAGE)
        require(state['HostConfig']['NetworkMode'] == 'none' and len(state['Mounts']) == 2, 'staging isolation changed')
        result = {'passed': True, 'image_id': build['image_id'], 'binaries': build['binaries'],
                  'restart_resume': True, 'config_apply': apply_result, 'search_live_switch': search_result,
                  'alternate_database_preserved': True, 'production_data_mounted': False}
        save('stage-report.json', result)
        return result
    except Exception:
        if started:
            with (stage_root / 'failure.log').open('wb') as output:
                os.chmod(output.name, 0o600)
                subprocess.run(['docker', 'logs', '--tail', '200', STAGE], stdout=output,
                               stderr=subprocess.STDOUT, timeout=20, check=False)
        raise
    finally:
        if started:
            util.run(['docker', 'stop', '-t', '90', STAGE], timeout=120)
            util.run(['docker', 'rm', STAGE])


def production_search_rpc(token):
    probe = load_module('search_websocket_probe', TASK / 'ws_probe.py')
    ws, receive = probe.connection('ws://100.80.205.120:18787/v1')
    before = storage.status()['runtime']['instance_id']
    container_started = util.inspect()['State']['StartedAt']
    try:
        ws.send(json.dumps({'type': 'hello', 'protocolVersion': 12, 'token': token,
                            'clientId': str(uuid_value()), 'resumeFrom': []}))
        hello = json.loads(receive())
        require(hello.get('type') == 'hello' and hello.get('protocolVersion') == 12, 'production handshake failed')
        def call(operation, fields=None):
            identifier = str(uuid_value())
            nil = '00000000-0000-0000-0000-000000000000'
            ws.send(json.dumps({'type': 'request', 'requestId': identifier, 'sessionId': nil, 'runtimeId': nil,
                'command': {'type': 'deerFlow', 'operation': operation, 'input': fields or {}}}))
            deadline = time.monotonic() + 40
            while time.monotonic() < deadline:
                reply = json.loads(receive())
                if reply.get('type') == 'response' and reply.get('requestId') == identifier:
                    require(reply['outcome']['status'] == 'ok', 'production search RPC failed')
                    return reply['outcome']['payload']['data']
            raise TimeoutError('production search RPC timeout')
        state = call('search-status')
        original = state['default_provider']
        configured = [x['name'] for x in state['providers'] if x['configured']]
        selected = []
        stored_default = json.loads(util.CONFIG.with_name('search-providers.json').read_bytes())['config'].get('default_provider')
        # Preserve automatic selection and any incomplete explicit default.
        candidates = configured if stored_default == original and original in configured else []
        try:
            for provider in candidates:
                require(call('search-select', {'provider': provider})['default_provider'] == provider, 'production selection failed')
                require(storage.status()['runtime']['instance_id'] == before, 'production daemon restarted during selection')
                selected.append(provider)
        finally:
            if selected:
                require(original in configured, 'original provider is not restorable')
                require(call('search-select', {'provider': original})['default_provider'] == original, 'default provider was not restored')
        require(util.inspect()['State']['StartedAt'] == container_started, 'production container restarted during selection')
        return {'passed': True, 'providers': [p['name'] for p in state['providers']],
                'configured_providers_tested': selected, 'default_provider_restored': True,
                'same_daemon_instance': True, 'same_container': True, 'prompts_submitted': 0}
    finally:
        ws.close()


def uuid_value():
    import uuid
    return uuid.uuid4()


def verify(apply_config=False):
    build, baseline = read('build-report.json'), read('baseline.json')
    util.wait_healthy()
    state = util.inspect()
    require(state['Image'] == build['image_id'], 'production image mismatch')
    require(storage.mounts(state['Mounts']) == baseline['mounts'] and
            state['HostConfig']['PortBindings'] == baseline['ports'], 'mounts or ports changed')
    require(storage.hashes(util.CONTAINER, build['binaries']) == build['binaries'], 'installed binaries differ')
    require(digest(storage.BINDING) == baseline['binding_sha256'], 'database binding changed')
    for name in ('compose', 'wrapper'):
        require(digest(Path(baseline['files'][name]['path'])) == baseline['files'][name]['sha256'], name + ' changed')
    require(util.ENV.read_bytes() == util.image_env((storage.BACKUP / 'env').read_bytes(), build['image_id']),
            'unexpected environment change')
    old_config = (storage.BACKUP / 'config').read_bytes()
    removed = verify_config(old_config, util.CONFIG.read_bytes())
    if removed:
        require(util.CONFIG.with_name(util.CONFIG.name + '.search-migration.bak').read_bytes() == old_config,
                'search migration backup differs')
    store = util.CONFIG.with_name('search-providers.json')
    before_store = storage.BACKUP / 'search-providers.json'
    verify_provider_store(before_store.read_bytes() if before_store.exists() else None, store.read_bytes())
    if not before_store.exists():
        environment = dict(x.split('=', 1) for x in state['Config']['Env'] if '=' in x)
        verify_legacy_search_store(old_config, store.read_bytes(), environment)
    current = storage.status()
    require(current['runtime']['build_id'] == BUILD_ID, 'native build mismatch')
    token = next(x.split('=', 1)[1] for x in state['Config']['Env'] if x.startswith('WAKU_DAEMON_TOKEN='))
    rpc = production_search_rpc(token)
    # Do not send production prompts, replace credentials, or restart the daemon
    # for verification. All three real HTTP adapters were exercised in staging.
    verify_provider_store(before_store.read_bytes() if before_store.exists() else None, store.read_bytes())
    sessions = storage.historical_sessions(storage.BACKUP / 'harness.db')
    resume = storage.native_resume(util.CONTAINER, sessions, min(2, len(sessions)))
    records = storage.preserve_rows(storage.BACKUP / 'harness.db', util.DB,
        ['harness_sessions', 'harness_runs', 'harness_events', 'harness_inputs', 'harness_tool_receipts',
         'eino_session_events', 'eino_checkpoints', 'harness_session_configs', 'harness_channels'], immutable={
         'harness_events': ['session_id', 'run_id', 'event'], 'harness_inputs': ['run_id', 'content'],
         'eino_session_events': ['session_id', 'event_id', 'kind', 'payload']})
    desktop = ROOT / 'data/user-data/desktop/app.db'
    if (storage.BACKUP / 'desktop.db').exists():
        records['desktop'] = storage.preserve_rows(storage.BACKUP / 'desktop.db', desktop, ['sessions', 'messages', 'turns'])
    require(storage.tree_hashes(ROOT / 'data/user-data/data/go-harness') == read('alternate-hashes.json'), 'alternate database changed')
    require(storage.others() == read('other-containers.json'), 'unrelated containers changed')
    result = {'passed': True, 'image_id': state['Image'], 'health': state['State']['Health']['Status'],
        'data_dir': storage.PRIMARY, 'database_binding_unchanged': True, 'protocol': 12,
        'configuration_preserved': True, 'migrated_search_fields': removed, 'credentials_preserved': True,
        'legacy_search_credentials_verified': not before_store.exists(),
        'native_resume': resume, 'records': records, 'search_rpc': rpc,
        'search_live_switch': read('stage-report.json')['search_live_switch'],
        'alternate_database_preserved': True, 'other_containers_preserved': True}
    save('production-report.json', result)
    return result


def rollback():
    # The old backend reads the old Brave fields. Restore their exact original
    # bytes only when the current config is precisely our one-time migration.
    # Never restore any database, skills archive, or provider store.
    old = (storage.BACKUP / 'config').read_bytes()
    if util.CONFIG.read_bytes() != old:
        verify_config(old, util.CONFIG.read_bytes())
    build = read('build-report.json')
    state = util.inspect()
    require(state['Image'] in (storage.BASE, build['image_id']), 'unrelated production image')
    if state['State']['Running']:
        try:
            util.frontend_idle(util.control('daemon.drain'))
            util.maintenance_idle(util.database_summary(), include_queued=False)
        except Exception:
            util.management('daemon.resume')
            raise
    util.run(util.compose('stop', '-t', '90', 'deerflow'), timeout=120)
    util.atomic(util.CONFIG, old)
    util.atomic(util.ENV, (storage.BACKUP / 'env').read_bytes())
    util.atomic(util.WRAPPER, (storage.BACKUP / 'wrapper').read_bytes(), mode=0o755)
    util.run(util.compose('up', '-d', '--no-deps', '--no-build', 'deerflow'), timeout=180)
    util.wait_healthy()
    require(util.inspect()['Image'] == storage.BASE, 'rollback image mismatch')
    util.control('daemon.resume')
    save('rollback-report.json', {'passed': True, 'business_data_restored': False})


def apply():
    # Preserve the private existing store separately for exact credential checks;
    # storage.apply also takes a complete stopped-profile backup.
    store = util.CONFIG.with_name('search-providers.json')
    captured = store.read_bytes() if store.exists() else None
    original_verify = storage.verify
    def checked_verify(*args, **kwargs):
        if captured is not None and not (storage.BACKUP / 'search-providers.json').exists():
            util.atomic(storage.BACKUP / 'search-providers.json', captured)
        return original_verify(*args, **kwargs)
    storage.verify = checked_verify
    result = storage.apply()
    save('deployment-report.json', result)
    return result


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['prepare', 'build-image', 'stage', 'apply', 'verify', 'rollback'])
    action = parser.parse_args().action
    initialize()
    print(json.dumps({'prepare': prepare, 'build-image': build_image, 'stage': stage,
                      'apply': apply, 'verify': verify, 'rollback': rollback}[action](), indent=2))
