"""Scoped Gentoo rollout. No credentials or business text enter JSON reports.

Run locally on Gentoo: prepare, build the image, stage, apply, verify.
Rollback restores executable/configuration selection, NEVER an old business DB.
"""
import argparse
import copy
from contextlib import closing
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import sqlite3
import subprocess
import tarfile
import time
import uuid

import yaml

ROOT = Path('/home/gen/deerflow-deploy')
TASK = ROOT / 'skill-learning-recovery-20261028'
DATA = ROOT / 'data'
CONFIG = DATA / 'user-data/config/config.yaml'
DB = DATA / 'user-data/config/go-harness-state/harness.db'
ENV = ROOT / '.env'
COMPOSE = ROOT / 'compose.yaml'
WRAPPER = ROOT / 'runtime/deerflow-acpd-wrapper.sh'
BASE_ID = 'sha256:0a39622576b914ba64d73794288315b824b48bbdcfcb22d7687c8799872a6514'
IMAGE = 'deerflow-server:skill-learning-recovery-20261028'
BUILD_ID = 'skill-learning-recovery-20261028'
CONTAINER = 'deerflow-deploy-deerflow-1'
STAGE = 'deerflow-skill-learning-stage-20261028'
BACKUP = TASK / 'backup'
RUNTIME = '/var/lib/deerflow/user-data/runtime/acp-go'
BINARIES = ('deerflow-acpd.real', 'deerflow-config-go', 'deerflow-control-go', 'deerflow-skill-go')


def require(value, code):
    if not value:
        raise RuntimeError(code)


def run(args, *, payload=None, env=None, timeout=120, accepted_codes=(0,)):
    result = subprocess.run(args, input=payload, text=True, capture_output=True,
                            env=env, timeout=timeout)
    # Never expose stderr: Docker/configuration errors can contain credentials.
    require(result.returncode in accepted_codes, 'command_failed:' + Path(args[0]).name)
    return result.stdout


def sha(data):
    return hashlib.sha256(data).hexdigest()


def regular(path):
    require(path.is_file() and not path.is_symlink(), 'not_regular_file:' + path.name)
    return path.read_bytes()


def atomic(path, data, mode=0o600):
    if isinstance(data, str):
        data = data.encode()
    require(not path.is_symlink(), 'refuse_symlink:' + path.name)
    tmp = path.with_name(path.name + '.learning-' + uuid.uuid4().hex)
    with tmp.open('xb') as handle:
        tmp.chmod(mode)
        handle.write(data)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(tmp, path)


def save(name, value):
    atomic(TASK / name, json.dumps(value, indent=2, sort_keys=True) + '\n')


class UniqueLoader(yaml.SafeLoader):
    pass


def unique_mapping(loader, node, deep=False):
    result = {}
    for key_node, value_node in node.value:
        key = loader.construct_object(key_node, deep=deep)
        require(key not in result, 'duplicate_yaml_key')
        result[key] = loader.construct_object(value_node, deep=deep)
    return result


UniqueLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, unique_mapping)


def load_yaml(raw):
    value = yaml.load(raw, Loader=UniqueLoader)
    require(isinstance(value, dict), 'configuration_not_mapping')
    return value


def changed_paths(before, after, prefix=''):
    if isinstance(before, dict) and isinstance(after, dict):
        found = []
        for key in sorted(set(before) | set(after)):
            path = prefix + '/' + str(key)
            if key not in before or key not in after:
                found.append(path)
            else:
                found.extend(changed_paths(before[key], after[key], path))
        return found
    return [] if before == after else [prefix]


def candidate_config(raw):
    """Replace only two top-level sections; all other source bytes survive."""
    original = load_yaml(raw)
    document = copy.deepcopy(original)
    require(isinstance(document.get('skills'), dict), 'skills_missing')
    require(isinstance(document.get('skill_evolution'), dict), 'evolution_missing')
    skills, evolution = document['skills'], document['skill_evolution']
    require(skills.get('enabled') is True, 'skills_disabled')
    require(skills.get('path') == '../skills', 'unexpected_skills_path')
    wanted = {'id': 'managed-evolution', 'root': '../skills/evolved', 'scope': 'global'}
    sources = skills.setdefault('managed_sources', [])
    require(isinstance(sources, list), 'managed_sources_invalid')
    matching = [x for x in sources if isinstance(x, dict) and x.get('id') == wanted['id']]
    require(len(matching) <= 1, 'duplicate_managed_source')
    if matching:
        require(matching[0] == wanted, 'managed_source_conflict')
    else:
        sources.append(wanted)
    require(skills.get('export_root', '../skills-exports') == '../skills-exports', 'export_root_conflict')
    skills['export_root'] = '../skills-exports'
    allowed = {'/skills/managed_sources', '/skills/export_root'}

    def set_value(group, key, value):
        target = evolution if not group else evolution.setdefault(group, {})
        require(isinstance(target, dict), 'evolution_group_invalid')
        target[key] = value
        allowed.add('/skill_evolution/' + (group + '/' if group else '') + key)

    for key, value in {'enabled': True, 'mode': 'auto_patch', 'security_fail_closed': True}.items():
        set_value('', key, value)
    require(evolution.get('allow_builtin_overlays', False) is False, 'unexpected_builtin_overlay_grant')
    for key in ('enabled', 'consolidation_enabled', 'matching_enabled'):
        set_value('learning', key, True)
    require(evolution['learning'].get('cross_session_enabled', False) is False, 'unexpected_cross_session_grant')
    set_value('learning', 'cross_session_enabled', False)
    set_value('generation', 'enabled', True)
    for key in ('operator_corrections_enabled', 'missing_reports_enabled',
                'successful_workflows_enabled', 'experience_patterns_enabled',
                'experience_new_skills_enabled', 'experience_review_drafts_enabled'):
        set_value('discovery', key, True)
    set_value('discovery', 'new_skill_source_id', 'managed-evolution')
    set_value('behavior', 'enabled', True)
    require(evolution.get('behavior', {}).get('suite_path', '') in
            ('', './skill-behavior/searxng-web-search.json'), 'custom_behavior_suite_requires_review')
    set_value('behavior', 'suite_path', './skill-behavior/searxng-web-search.json')
    set_value('auto_patch', 'experience_patterns_enabled', True)
    for group in ('validation', 'generation'):
        for key, value in {'timeout_seconds': 60, 'max_input_bytes': 65536,
                           'max_output_tokens': 1024 if group == 'validation' else 4096}.items():
            if evolution.get(group, {}).get(key, 0) == 0:
                set_value(group, key, value)
    # Budget authority remains byte-for-byte semantically unchanged.
    require(evolution.get('budget') == original['skill_evolution'].get('budget'), 'budget_changed')
    for key in ('max_daily_model_calls', 'max_daily_tokens'):
        value = evolution['learning'].get(key)
        require(isinstance(value, int) and 0 < value <= evolution['budget'][key], 'learning_budget_invalid')
    changed = changed_paths(original, document)
    require(set(changed) <= allowed, 'unauthorized_configuration_change')
    text = raw.decode('utf-8')
    root = yaml.compose(text, Loader=UniqueLoader)
    spans = []
    for index, (key, _value) in enumerate(root.value):
        if key.value in ('skills', 'skill_evolution'):
            end = root.value[index + 1][0].start_mark.index if index + 1 < len(root.value) else len(text)
            # Preserve trailing top-level comments and whitespace belonging to the next section.
            lines = text[key.start_mark.index:end].splitlines(keepends=True)
            while lines and (not lines[-1].strip() or lines[-1].startswith('#')):
                lines.pop()
            end = key.start_mark.index + len(''.join(lines))
            spans.append((key.start_mark.index, end, key.value))
    require(len(spans) == 2, 'configuration_sections_ambiguous')
    for start, end, key in reversed(spans):
        replacement = yaml.safe_dump({key: document[key]}, sort_keys=False, allow_unicode=True)
        text = text[:start] + replacement + text[end:]
    output = text.encode('utf-8')
    require(load_yaml(output) == document, 'configuration_roundtrip_changed')
    return output, changed


def compose(*args):
    return ['docker', 'compose', '--env-file', str(ENV), '--project-directory', str(ROOT),
            '-p', 'deerflow-deploy', '-f', str(COMPOSE), *args]


def inspect(name=CONTAINER):
    return json.loads(run(['docker', 'inspect', name]))[0]


def image_id(image=IMAGE):
    value = run(['docker', 'image', 'inspect', '--format', '{{.Id}}', image]).strip()
    require(re.fullmatch(r'sha256:[a-f0-9]{64}', value), 'invalid_image_identity')
    return value


def management(operation, container=CONTAINER, **fields):
    raw = run(['docker', 'exec', '-i', container, '/var/lib/deerflow/deerflow-control-go',
               '--runtime-dir', RUNTIME], payload=json.dumps({'operation': operation, **fields}),
              accepted_codes=(0, 1))
    value = json.loads(raw)
    require(isinstance(value, dict), 'invalid_management_response')
    return value


def control(operation, container=CONTAINER, **fields):
    value = management(operation, container, **fields)
    require(value.get('ok') is True, 'management_failed:' + operation)
    return value['data']


def other_containers():
    ids = run(['docker', 'ps', '-aq']).split()
    if not ids:
        return []
    states = json.loads(run(['docker', 'inspect', *ids]))
    return sorted([{'id': x['Id'], 'name': x['Name'].lstrip('/'),
                    'status': x['State']['Status'], 'running': x['State']['Running'],
                    'started_at': x['State']['StartedAt']}
                   for x in states if x['Name'].lstrip('/') not in (CONTAINER, STAGE)],
                  key=lambda x: x['name'])


def database_summary(path=DB):
    require(path.is_file() and not path.is_symlink(), 'database_not_regular')
    with closing(sqlite3.connect(path.as_uri() + '?mode=ro', uri=True, timeout=10)) as db:
        tables = {row[0] for row in db.execute("SELECT name FROM sqlite_master WHERE type='table'")}
        required = {'harness_sessions', 'harness_runs', 'df_skill_maintenance_jobs',
                    'df_skill_maintenance_calls', 'harness_learning_outbox'}
        require(required <= tables, 'unexpected_database_schema')
        result = {'sessions': db.execute('SELECT count(*) FROM harness_sessions').fetchone()[0]}
        for table, column, key in [('harness_runs', 'status', 'runs'),
                                   ('df_skill_maintenance_jobs', 'state', 'maintenance_jobs'),
                                   ('df_skill_maintenance_calls', 'state', 'maintenance_calls'),
                                   ('harness_learning_outbox', 'state', 'learning_outbox')]:
            result[key] = dict(db.execute('SELECT ' + column + ',count(*) FROM ' + table + ' GROUP BY ' + column))
        return result


def maintenance_idle(summary, *, include_queued=True):
    jobs, calls = summary['maintenance_jobs'], summary['maintenance_calls']
    require(not (set(jobs) - {'queued', 'retry', 'running', 'completed', 'failed', 'cancelled'}),
            'unknown_maintenance_job_state')
    require(not (set(calls) - {'reserved', 'settled'}), 'unknown_maintenance_call_state')
    states = ('queued', 'retry', 'running') if include_queued else ('running',)
    require(all(jobs.get(state, 0) == 0 for state in states) and
            calls.get('reserved', 0) == 0, 'maintenance_work_wait_required')


def frontend_idle(state):
    require(all(state.get(key) == 0 for key in ('active_operations', 'active_runs', 'queued_runs')),
            'frontend_work_wait_required')


def check_mounts(state):
    mounts = {x['Destination']: x for x in state.get('Mounts', [])}
    for destination, source in [('/var/lib/deerflow', DATA),
                                ('/var/lib/deerflow/user-data/skills', ROOT / 'skills'),
                                ('/opt/deerflow/bin/deerflow-acpd', WRAPPER)]:
        require(mounts.get(destination, {}).get('Source') == str(source), 'deployment_mount_changed')


def baseline(manifest):
    current = inspect()
    require(current['Image'] == BASE_ID, 'base_image_changed')
    check_mounts(current)
    for name, path in [('config', CONFIG), ('env', ENV), ('compose', COMPOSE), ('wrapper', WRAPPER)]:
        require(sha(regular(path)) == manifest[name + '_sha256'], name + '_changed_since_prepare')
    return current


def prepare():
    TASK.mkdir(mode=0o700, parents=True, exist_ok=True)
    require(not (TASK / 'manifest.json').exists(), 'prepare_already_exists')
    state = inspect()
    require(state['Image'] == BASE_ID, 'base_image_changed')
    check_mounts(state)
    raw = regular(CONFIG)
    candidate, changed = candidate_config(raw)
    summary = database_summary()
    require(summary['sessions'] >= 66, 'unexpected_database_identity')
    status = management('daemon.status')
    require(status.get('ok') is True, 'daemon_unavailable')
    require(status.get('runtime', {}).get('data_dir') == '/var/lib/deerflow/user-data/config/go-harness-state',
            'runtime_database_path_changed')
    manifest = {'version': 1, 'base_image_id': BASE_ID, 'image_tag': IMAGE, 'build_id': BUILD_ID,
                'database_path': str(DB), 'candidate_sha256': sha(candidate), 'changed_paths': changed,
                'baseline_database': summary, 'other_containers': other_containers(),
                'binaries': {name: sha(regular(TASK / 'bin' / name)) for name in BINARIES}}
    for name, path in [('config', CONFIG), ('env', ENV), ('compose', COMPOSE), ('wrapper', WRAPPER)]:
        manifest[name + '_sha256'] = sha(regular(path))
    atomic(TASK / 'candidate-config.yaml', candidate)
    save('manifest.json', manifest)
    report = {'passed': True, 'phase': 'prepared', 'changed_paths': changed,
              'database_path': str(DB), 'database': summary,
              'budget_preserved': True, 'cross_session_enabled': False}
    save('preflight.json', report)
    return report


def read_manifest():
    result = json.loads(regular(TASK / 'manifest.json'))
    require(result.get('base_image_id') == BASE_ID and result.get('build_id') == BUILD_ID,
            'manifest_identity_changed')
    require(sha(regular(TASK / 'candidate-config.yaml')) == result['candidate_sha256'], 'candidate_changed')
    for name in BINARIES:
        require(sha(regular(TASK / 'bin' / name)) == result['binaries'][name], 'binary_changed')
    return result


def synthetic_config(candidate):
    document = load_yaml(candidate)
    # Construct from an allowlist. Never copy provider keys, channels, proxies, memory or paths.
    original = document['skill_evolution']
    native_groups = {
        'validation': 'timeout_seconds max_input_bytes max_output_tokens response_format',
        'generation': 'enabled timeout_seconds max_input_bytes max_output_tokens',
        'learning': ('enabled consolidation_enabled matching_enabled cross_session_enabled idle_seconds '
                     'timeout_seconds max_input_bytes max_output_tokens max_daily_model_calls '
                     'max_daily_tokens external_context_policy'),
        'discovery': ('operator_corrections_enabled missing_reports_enabled successful_workflows_enabled '
                      'experience_patterns_enabled experience_new_skills_enabled experience_review_drafts_enabled '
                      'new_skill_source_id repeat_threshold repeat_window_days cooldown_hours '
                      'max_daily_proposals max_pending_proposals'),
        'behavior': 'enabled suite_path timeout_seconds max_steps max_input_bytes max_output_tokens',
        'auto_patch': ('experience_patterns_enabled max_changed_lines max_daily_publications '
                       'observation_hours min_observations rollback_failure_threshold'),
        'budget': 'max_daily_model_calls max_daily_tokens max_job_seconds workers'}
    stage_evolution = {key: original[key] for key in
                       ('enabled', 'mode', 'allow_builtin_overlays', 'security_fail_closed') if key in original}
    for group, fields in native_groups.items():
        stage_evolution[group] = {key: copy.deepcopy(original[group][key])
                                  for key in fields.split() if key in original.get(group, {})}
    value = {'models': [{'name': 'offline-stage', 'provider': 'openai', 'model': 'offline-stage',
                         'base_url': 'http://127.0.0.1:9/v1', 'api_key': 'synthetic-no-credential',
                         'context_window': 65536}], 'default_model': 'offline-stage',
             'skills': copy.deepcopy(document['skills']),
             'skill_evolution': stage_evolution,
             'local_acp': {'model_name': 'offline-stage', 'enable_bash': False},
             'memory': {'enabled': False}, 'tools': []}
    value['skills'] = {'enabled': True, 'path': '../skills', 'extensions_file': './extensions_config.json',
                       'export_root': '../skills-exports', 'managed_sources': [
                           {'id': 'managed-evolution', 'root': '../skills/evolved', 'scope': 'global'}]}
    evolution = value['skill_evolution']
    for key in ('moderation_model_name', 'evaluation_model_name', 'generation_model_name'):
        evolution[key] = None
    for key in ('extraction_model_name', 'consolidation_model_name', 'matching_model_name'):
        evolution['learning'][key] = None
    return yaml.safe_dump(value, sort_keys=False).encode()


def wait_healthy(container=CONTAINER):
    for _ in range(60):
        state = inspect(container)['State']
        if state.get('Health', {}).get('Status') == 'healthy':
            return
        require(state.get('Status') not in ('dead', 'exited'), 'container_exited')
        time.sleep(2)
    raise RuntimeError('container_not_healthy')


def worker_report(container=CONTAINER):
    native = management('daemon.status', container)
    require(native.get('ok') is True, 'daemon_status_failed')
    require(native.get('runtime', {}).get('build_id') == BUILD_ID, 'runtime_build_mismatch')
    reports = {}
    keys = {
        'learning': ('enabled', 'running', 'mode', 'idle_seconds', 'consolidation_enabled',
                     'matching_enabled', 'cross_session_enabled', 'proposal_discovery_enabled',
                     'auto_patch_enabled', 'pending_inputs', 'experiences', 'patterns', 'matches', 'queue'),
        'discovery': ('enabled', 'running', 'triggers', 'pending', 'failed_signals', 'last_error_code'),
        'generation': ('enabled',),
        'auto_patch': ('enabled', 'running', 'published_today', 'observing', 'readiness', 'last_error_code')}
    for name in keys:
        data = control('skill.' + name + '.status', container)
        require(data.get('enabled') is True, name + '_not_enabled')
        if name != 'generation':
            require(data.get('running') is True, name + '_not_running')
        reports[name] = {key: data[key] for key in keys[name] if key in data}
    learning = reports['learning']
    require(learning.get('consolidation_enabled') is True and learning.get('matching_enabled') is True
            and learning.get('cross_session_enabled') is False and 'queue' in learning,
            'learning_pipeline_not_enabled')
    require(learning.get('mode') == 'guarded_auto_patch', 'learning_publication_mode_mismatch')
    reports['runtime_build_id'] = native['runtime']['build_id']
    reports['activity'] = {key: native['data'].get(key) for key in
                           ('active_operations', 'active_runs', 'queued_runs', 'draining')}
    return reports


def image_binary_hashes(container):
    output = run(['docker', 'exec', container, 'sha256sum',
                  *['/opt/deerflow/bin/' + name for name in BINARIES]])
    result = {}
    for line in output.splitlines():
        digest, path = line.split(None, 1)
        require(re.fullmatch(r'[a-f0-9]{64}', digest), 'invalid_image_binary_hash')
        result[Path(path.strip()).name] = digest
    require(set(result) == set(BINARIES), 'image_binary_list_mismatch')
    return result


def prepare_stage_profile(stage_root, candidate, wrapper):
    stage_data = stage_root / 'data'
    config = stage_data / 'user-data/config/config.yaml'
    config.parent.mkdir(parents=True, mode=0o700)
    atomic(config, synthetic_config(candidate))
    stage_wrapper = stage_root / 'support/deerflow-acpd-wrapper.sh'
    stage_wrapper.parent.mkdir(mode=0o700)
    atomic(stage_wrapper, wrapper, mode=0o755)
    return stage_data, stage_wrapper


def stage_mount_arguments(stage_data, stage_wrapper):
    return ['-v', str(stage_data) + ':/var/lib/deerflow',
            '-v', str(stage_wrapper) + ':/opt/deerflow/bin/deerflow-acpd:ro']


def stage():
    manifest = read_manifest()
    baseline(manifest)
    candidate = image_id()
    save('stage.json', {'passed': False, 'phase': 'running', 'image_id': candidate})
    stage_root = TASK / ('stage-' + uuid.uuid4().hex)
    wrapper = regular(WRAPPER)
    require(sha(wrapper) == manifest['wrapper_sha256'], 'wrapper_changed_since_prepare')
    stage_data, stage_wrapper = prepare_stage_profile(
        stage_root, regular(TASK / 'candidate-config.yaml'), wrapper)
    # Container runs as the same UID as the deployment owner, but gets no production mounts.
    uid, gid = os.getuid(), os.getgid()
    started = False
    try:
        env = dict(os.environ, WAKU_DAEMON_TOKEN=secrets.token_urlsafe(32))
        run(['docker', 'run', '-d', '--name', STAGE, '--network', 'none',
             '--user', str(uid) + ':' + str(gid), '--cap-drop', 'ALL',
             '--security-opt', 'no-new-privileges:true', '--pids-limit', '256',
             '-e', 'WAKU_DAEMON_TOKEN', *stage_mount_arguments(stage_data, stage_wrapper), candidate], env=env)
        started = True
        wait_healthy(STAGE)
        running = inspect(STAGE)
        require(running['HostConfig']['NetworkMode'] == 'none', 'stage_network_not_isolated')
        expected_mounts = {'/var/lib/deerflow': (str(stage_data), True),
                           '/opt/deerflow/bin/deerflow-acpd': (str(stage_wrapper), False)}
        actual_mounts = {item['Destination']: (item['Source'], item['RW']) for item in running['Mounts']}
        require(running['Image'] == candidate and actual_mounts == expected_mounts,
                'stage_mounts_not_isolated')
        require(sha(regular(stage_wrapper)) == manifest['wrapper_sha256'], 'stage_wrapper_changed')
        report = worker_report(STAGE)
        require(image_binary_hashes(STAGE) == manifest['binaries'], 'stage_image_binaries_mismatch')
        diag = management('skill.learning.diagnostics', STAGE, session_id='stage-nonexistent', limit=1)
        require(diag.get('ok') is False and diag.get('code') == 'not_found', 'diagnostics_contract_unavailable')
        summary = database_summary(stage_data / 'user-data/config/go-harness-state/harness.db')
        require(summary['sessions'] == 0 and not summary['maintenance_calls'], 'stage_not_empty')
        report.update(passed=True, image_id=candidate, candidate_sha256=manifest['candidate_sha256'],
                      binaries=manifest['binaries'], isolated_network=True, production_mounts=False,
                      synthetic_configuration=True, wrapper_sha256=manifest['wrapper_sha256'],
                      diagnostics_contract=True, database=summary)
    finally:
        if started:
            run(['docker', 'stop', '-t', '90', STAGE], timeout=120)
            run(['docker', 'rm', STAGE])
    manifest['tested_image_id'] = candidate
    save('manifest.json', manifest)
    save('stage.json', report)
    return report


def gates():
    manifest = read_manifest()
    candidate = image_id()
    staged = json.loads(regular(TASK / 'stage.json'))
    tests = json.loads(regular(TASK / 'linux-tests.json'))
    require(staged.get('passed') is True and staged.get('image_id') == candidate and
            staged.get('candidate_sha256') == manifest['candidate_sha256'] and
            staged.get('binaries') == manifest['binaries'], 'matching_stage_acceptance_required')
    require(tests.get('passed') is True, 'linux_tests_required')
    require(tests.get('image_id') == candidate, 'linux_test_image_identity_required')
    # Bind acceptance to the tested build. The test producer records these exact binary hashes.
    require(tests.get('binaries') == manifest['binaries'], 'linux_test_binary_identity_required')
    return manifest, candidate


def image_env(raw, candidate):
    text, count = re.subn(r'(?m)^DEERFLOW_IMAGE=[^\r\n]*', 'DEERFLOW_IMAGE=' + candidate, raw.decode())
    require(count == 1, 'image_setting_ambiguous')
    return text.encode()


def backup_database():
    require(not (BACKUP / 'harness.db').exists(), 'database_backup_exists')
    with closing(sqlite3.connect(DB.as_uri() + '?mode=ro', uri=True)) as source, \
            closing(sqlite3.connect(BACKUP / 'harness.db')) as target:
        source.backup(target)
        require(target.execute('PRAGMA quick_check').fetchone()[0] == 'ok', 'backup_integrity_failed')
    (BACKUP / 'harness.db').chmod(0o600)
    skills = ROOT / 'skills'
    require(skills.is_dir() and not skills.is_symlink(), 'skills_root_invalid')
    with tarfile.open(BACKUP / 'skills.tar.gz', 'x:gz', dereference=False) as archive:
        archive.add(skills, arcname='skills', recursive=True)
    (BACKUP / 'skills.tar.gz').chmod(0o600)


def drain_idle(*, include_queued=True):
    state = control('daemon.status')
    frontend_idle(state)
    require(state.get('draining') is False, 'already_draining')
    maintenance_idle(database_summary(), include_queued=include_queued)
    # Caller owns a finally/resume before this request, including uncertain response delivery.
    state = control('daemon.drain')
    require(state.get('draining') is True, 'drain_failed')
    frontend_idle(state)
    maintenance_idle(database_summary(), include_queued=include_queued)


def restore_configuration(manifest, candidate):
    # A concurrent edit must never be overwritten by automatic recovery.
    require(sha(regular(COMPOSE)) == manifest['compose_sha256'], 'compose_changed_during_rollout')
    require(sha(regular(WRAPPER)) == manifest['wrapper_sha256'], 'wrapper_changed_during_rollout')
    original_config, original_env = regular(BACKUP / 'config.yaml'), regular(BACKUP / 'env')
    require(sha(original_config) == manifest['config_sha256'] and sha(original_env) == manifest['env_sha256'],
            'backup_configuration_identity_mismatch')
    require(sha(regular(CONFIG)) in (manifest['config_sha256'], manifest['candidate_sha256']),
            'config_changed_during_rollout')
    require(regular(ENV) in (original_env, image_env(original_env, candidate)), 'env_changed_during_rollout')
    atomic(CONFIG, original_config)
    atomic(ENV, original_env)


def prepare_running_recovery():
    """A stopped container can recover without management; unknown live work cannot."""
    if not inspect()['State']['Running']:
        return False
    state = control('daemon.drain')
    require(state.get('draining') is True, 'recovery_drain_failed')
    frontend_idle(state)
    maintenance_idle(database_summary(), include_queued=False)
    return True


def resume_if_running():
    try:
        if inspect()['State']['Running']:
            control('daemon.resume')
        return True
    except Exception:
        save('recovery-attention.json', {'resume_confirmed': False,
                                       'reason': 'management_resume_unavailable'})
        return False


def apply():
    manifest, candidate = gates()
    baseline(manifest)
    require(not BACKUP.exists(), 'backup_exists_use_verify_or_rollback')
    old_env = regular(ENV)
    stopped = False
    needs_resume = False
    try:
        require(control('daemon.status').get('draining') is False, 'already_draining')
        needs_resume = True
        drain_idle()
        baseline(manifest)
        BACKUP.mkdir(mode=0o700)
        for name, path in [('config.yaml', CONFIG), ('env', ENV), ('compose.yaml', COMPOSE), ('wrapper.sh', WRAPPER)]:
            atomic(BACKUP / name, regular(path))
        save('deployment.json', {'phase': 'stopping', 'image_id': candidate, 'backup': str(BACKUP)})
        # Set before invoking stop: a lost response does not prove the container stayed running.
        stopped = True
        run(compose('stop', '-t', '90', 'deerflow'), timeout=120)
        require(inspect()['State']['Running'] is False, 'container_did_not_stop')
        maintenance_idle(database_summary())
        backup_database()
        baseline(manifest)
        atomic(CONFIG, regular(TASK / 'candidate-config.yaml'))
        atomic(ENV, image_env(old_env, candidate))
        run(compose('up', '-d', '--no-deps', 'deerflow'), timeout=180)
        report = verify()
        save('deployment.json', {'phase': 'deployed', 'image_id': candidate, 'backup': str(BACKUP),
                                 'passed': True, 'business_data_restored': False})
        return report
    except Exception:
        if stopped:
            try:
                prepare_running_recovery()
            except Exception:
                save('deployment.json', {'phase': 'rollback_deferred', 'passed': False,
                                         'reason': 'live_work_or_management_unavailable',
                                         'image_id': candidate, 'backup': str(BACKUP),
                                         'business_data_restored': False})
                raise RuntimeError('automatic_rollback_deferred_live_work_or_unknown_state') from None
            # Stop candidate first so configuration recovery cannot race a new process.
            run(compose('stop', '-t', '90', 'deerflow'), timeout=120)
            restore_configuration(manifest, candidate)
            run(compose('up', '-d', '--no-deps', 'deerflow'), timeout=180)
            wait_healthy()
            require(inspect()['Image'] == BASE_ID, 'rollback_image_mismatch')
            save('deployment.json', {'phase': 'automatically_rolled_back', 'passed': False,
                                     'backup': str(BACKUP), 'business_data_restored': False})
        raise
    finally:
        if needs_resume:
            # A fresh daemon starts undrained; resume is also safe after a failed pre-stop drain.
            resume_if_running()


def verify():
    manifest, candidate = gates()
    wait_healthy()
    current = inspect()
    require(current['Image'] == candidate, 'production_image_mismatch')
    check_mounts(current)
    require(sha(regular(CONFIG)) == manifest['candidate_sha256'], 'production_configuration_changed')
    require(sha(regular(COMPOSE)) == manifest['compose_sha256'] and
            sha(regular(WRAPPER)) == manifest['wrapper_sha256'], 'deployment_support_changed')
    report = worker_report()
    require(image_binary_hashes(CONTAINER) == manifest['binaries'], 'production_image_binaries_mismatch')
    with closing(sqlite3.connect(DB.as_uri() + '?mode=ro', uri=True)) as db:
        row = db.execute('SELECT id FROM harness_sessions ORDER BY id LIMIT 1').fetchone()
    require(row, 'production_session_missing')
    diag = control('skill.learning.diagnostics', session_id=row[0], limit=1)
    require(diag.get('running') is True and 'queue' in diag, 'production_diagnostics_unavailable')
    summary = database_summary()
    require(summary['sessions'] >= manifest['baseline_database']['sessions'], 'sessions_lost')
    others = other_containers()
    require(others == manifest['other_containers'], 'unrelated_container_state_changed')
    report.update(passed=True, image_id=candidate, health='healthy', database_path=str(DB),
                  database=summary, diagnostics=True, other_containers=others,
                  budgets_preserved=True, business_data_restored=False)
    save('production.json', report)
    return report


def rollback():
    # Recovery depends only on durable manifest/config backups, not cleaned build/test artifacts.
    manifest = json.loads(regular(TASK / 'manifest.json'))
    require(manifest.get('base_image_id') == BASE_ID and manifest.get('build_id') == BUILD_ID,
            'manifest_identity_changed')
    candidate = manifest.get('tested_image_id')
    require(isinstance(candidate, str) and re.fullmatch(r'sha256:[a-f0-9]{64}', candidate),
            'rollback_candidate_identity_missing')
    require(BACKUP.is_dir() and not BACKUP.is_symlink(), 'backup_missing')
    require(inspect()['Image'] in (BASE_ID, candidate), 'unrelated_image_do_not_rollback')
    needs_resume = False
    try:
        # Recovery remains available when candidate startup failed and the process is stopped.
        needs_resume = inspect()['State']['Running']
        prepare_running_recovery()
        run(compose('stop', '-t', '90', 'deerflow'), timeout=120)
        restore_configuration(manifest, candidate)
        run(compose('up', '-d', '--no-deps', 'deerflow'), timeout=180)
        wait_healthy()
        require(inspect()['Image'] == BASE_ID, 'rollback_image_mismatch')
        report = {'passed': True, 'phase': 'rolled_back', 'image_id': BASE_ID,
                  'business_data_restored': False, 'database': database_summary()}
        save('rollback.json', report)
        return report
    finally:
        if needs_resume:
            resume_if_running()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['prepare', 'stage', 'apply', 'verify', 'rollback'])
    args = parser.parse_args()
    try:
        import fcntl  # Remote release host is Linux; unit tests remain portable.
        TASK.mkdir(mode=0o700, parents=True, exist_ok=True)
        with (TASK / 'release.lock').open('a') as lock:
            os.chmod(lock.name, 0o600)
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                raise RuntimeError('another_release_action_is_running') from None
            report = globals()[args.action]()
        print(json.dumps(report, sort_keys=True), flush=True)
    except Exception as error:
        # Only internal fixed RuntimeError codes are safe to expose.
        code = str(error) if isinstance(error, RuntimeError) else type(error).__name__
        print(json.dumps({'passed': False, 'error': code}), flush=True)
        raise SystemExit(1)


if __name__ == '__main__':
    main()
