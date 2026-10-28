"""Exercise cold and live WS resume using one isolated synthetic conversation.

Run inside FoloToy's /opt/folotoy directory. --run makes three short paid model
requests. The main process's device/session database is never opened. Only
actors recorded in this invocation's temporary SQLite file may be closed.
Native model/run receipts must be audited separately using native_session_id.
"""
import argparse
from contextlib import closing
import json
import logging
import os
from pathlib import Path
import re
import secrets
import sqlite3
import sys
import tempfile
import threading
import time


class ProbeFailure(Exception):
    """Only fixed diagnostic codes may be emitted."""


def emit(value):
    print(json.dumps(value, ensure_ascii=False), flush=True)


def option(info, name):
    return next((item for item in info.get('configOptions', []) if item.get('id') == name), {})


def cursor_of(client, session_id):
    cursor = client._load(session_id).cursor
    if not isinstance(cursor, dict) or cursor.get('provider') != 'deerFlow' or not cursor.get('sessionId'):
        raise ProbeFailure('confirmed_native_cursor_missing')
    return {key: cursor[key] for key in ('provider', 'sessionId')}


def turn(client, session_id, generation, prompt, marker, timeout):
    chunks = client.stream_prompt(session_id, prompt, generation=generation,
                                  cancel_event=threading.Event(),
                                  turn_timeout=timeout, cancel_timeout=5)
    reply = []
    completion = None
    try:
        for update in chunks:
            if '_completion' in update:
                completion = update['_completion']
            elif update.get('sessionUpdate') == 'agent_message_chunk':
                content = update.get('content') or {}
                if content.get('type') == 'text' and isinstance(content.get('text'), str):
                    reply.append(content['text'])
    finally:
        chunks.close()
    value = ''.join(reply).strip()
    result = {'completed': bool(completion and completion.get('stopReason') == 'end_turn'),
              'stop_reason': completion.get('stopReason') if completion else None,
              'marker_matched': marker in value,
              'model': client._load(session_id).model,
              'native_cursor': cursor_of(client, session_id)}
    if not result['completed'] or not result['marker_matched']:
        raise ProbeFailure('turn_not_completed_or_history_marker_missing')
    return result


def owned_actor_ids(path):
    if not path.is_file():
        return set()
    with closing(sqlite3.connect(path.as_uri() + '?mode=ro', uri=True)) as database:
        exists = database.execute("SELECT 1 FROM sqlite_master WHERE type='table' AND name='deerflow_ws_sessions'").fetchone()
        return {row[0] for row in database.execute('SELECT session_id FROM deerflow_ws_sessions')} if exists else set()


def shrink_model_cache(path, session_id, old_model):
    """Mutate only this invocation's synthetic mapping, retaining its cursor."""
    with closing(sqlite3.connect(str(path))) as database, database:
        count = database.execute('SELECT count(*) FROM deerflow_ws_sessions').fetchone()[0]
        if count != 1:
            raise ProbeFailure('unexpected_synthetic_mapping_count')
        result = database.execute(
            'UPDATE deerflow_ws_sessions SET models=?,model=? WHERE session_id=? AND unconfirmed=0',
            (json.dumps([{'id': old_model, 'name': old_model}]), old_model, session_id))
        if result.rowcount != 1:
            raise ProbeFailure('synthetic_cache_update_not_confirmed')


def run(args, report):
    logging.disable(logging.CRITICAL)
    for source in (Path.cwd(), Path('/opt/folotoy')):
        if (source / 'sdk/deerflow_ws_client.py').is_file():
            sys.path.insert(0, str(source))
            break
    else:
        raise ProbeFailure('installed_ws_sdk_not_found')
    from sdk.deerflow_ws_client import DeerFlowWsClient
    from sdk.llm_sdk.deerflow import _configure

    token_env = os.environ.get('DEERFLOW_TOKEN_ENV', 'WAKU_DAEMON_TOKEN')
    if not re.fullmatch(r'[A-Za-z_][A-Za-z0-9_]*', token_env) or not os.environ.get(token_env):
        raise ProbeFailure('daemon_token_environment_missing')
    url = os.environ.get('DEERFLOW_WS_URL')
    if not url:
        raise ProbeFailure('ws_endpoint_environment_missing')
    marker = 'RESUME_' + secrets.token_hex(12).upper()
    started = time.monotonic()
    client = None
    recorded_session = None

    def remaining():
        left = args.timeout - (time.monotonic() - started)
        if left <= 0:
            raise ProbeFailure('overall_deadline_exceeded')
        return left

    def stage(name, **fields):
        item = {'stage': name, 'elapsed_ms': round((time.monotonic() - started) * 1000), **fields}
        report['stages'].append(item)
        emit(item)

    with tempfile.TemporaryDirectory(prefix='folo-ws-resume-') as directory:
        state_path = Path(directory).resolve() / 'state.sqlite3'
        config = {'transport': 'ws', 'ws_url': url,
                  'workspace': os.environ.get('DEERFLOW_WORKSPACE', '/workspaces/folotoy'),
                  'server_binary': os.environ.get('DEERFLOW_SERVER_BINARY', '/var/lib/deerflow/deerflow-acp'),
                  'token_env': token_env, 'ws_mode': 'fullAccess', 'approval': 'allow_always',
                  'connect_timeout': 25, 'cancel_timeout': 5, 'event_queue_size': 1024,
                  'model': args.previous_model, 'state_path': str(state_path)}

        def make_client(model):
            return DeerFlowWsClient({**config, 'model': model})

        def configure(current_client, session_id, generation, info, model):
            _configure(current_client, session_id, generation, info,
                       {**config, 'model': model}, threading.Event(),
                       time.monotonic() + min(30, remaining()))
            if current_client._load(session_id).model != model:
                raise ProbeFailure('per_turn_model_not_confirmed')

        try:
            client = make_client(args.previous_model)
            generation = client.ensure_connected(timeout=min(25, remaining()))
            info = client.request('session/new', {'cwd': client.workspace, 'mcpServers': []},
                                  generation=generation, timeout=min(25, remaining()))
            recorded_session = info['sessionId']
            client.remember_session(recorded_session, generation, info)
            configure(client, recorded_session, generation, info, args.previous_model)
            report['waku_session_id'] = recorded_session
            seed = turn(client, recorded_session, generation,
                        '这是独立会话恢复测试，不调用任何工具。请记住口令 ' + marker + '，只回复该口令。',
                        marker, min(args.turn_timeout, remaining()))
            if seed['model'] != args.previous_model:
                raise ProbeFailure('seed_model_not_confirmed')
            original_cursor = seed['native_cursor']
            report.update(native_session_id=original_cursor['sessionId'], native_cursor=original_cursor,
                          seed=seed)
            stage('previous_model_seed_completed', model=seed['model'], marker_matched=True)

            # Emulate a lost daemon actor while preserving the native session.
            # close_session is restricted to this synthetic mapping's actor.
            client.close_session(recorded_session)
            client.close()
            client = None
            shrink_model_cache(state_path, recorded_session, args.previous_model)

            client = make_client(args.expected_model)
            cached = client._load(recorded_session)
            report['cold_resume_cached_models_before'] = [item.get('id') for item in cached.models]
            if cursor_of(client, recorded_session) != original_cursor:
                raise ProbeFailure('saved_cursor_changed_before_resume')
            generation = client.ensure_connected(timeout=min(25, remaining()))
            info = client.request('session/resume', {'sessionId': recorded_session, 'cwd': client.workspace, 'mcpServers': []},
                                  generation=generation, timeout=min(30, remaining()))
            client.remember_session(recorded_session, generation, info)
            model_option = option(info, 'model')
            offered = [item.get('value') for item in model_option.get('options', [])]
            if args.expected_model not in offered or model_option.get('currentValue') != args.expected_model:
                raise ProbeFailure('cold_resume_did_not_refresh_model_catalog')
            configure(client, recorded_session, generation, info, args.expected_model)
            cold = turn(client, recorded_session, generation,
                        '不要调用工具。只回复本会话此前保存的完整测试口令，不要添加其他内容。',
                        marker, min(args.turn_timeout, remaining()))
            if cold['native_cursor'] != original_cursor or cold['model'] != args.expected_model:
                raise ProbeFailure('cold_resume_changed_history_or_model')
            report.update(cold_resume=cold, cold_resume_offered_models=offered)
            stage('cold_resume_completed', model=cold['model'], native_cursor_preserved=True,
                  marker_matched=True, model_catalog_refreshed=True)

            # Keep the actor live, put it on the retained old model using the
            # actual provider path, then close only this client's connection.
            info = client.session_info(recorded_session, generation)
            configure(client, recorded_session, generation, info, args.previous_model)
            live_runtime = client._load(recorded_session).runtime
            client.close()
            client = None

            client = make_client(args.expected_model)
            generation = client.ensure_connected(timeout=min(25, remaining()))
            info = client.request('session/resume', {'sessionId': recorded_session, 'cwd': client.workspace, 'mcpServers': []},
                                  generation=generation, timeout=min(30, remaining()))
            client.remember_session(recorded_session, generation, info)
            model_before = option(info, 'model').get('currentValue')
            if model_before != args.previous_model or client._load(recorded_session).runtime != live_runtime:
                raise ProbeFailure('live_attach_did_not_preserve_existing_actor')
            configure(client, recorded_session, generation, info, args.expected_model)
            live = turn(client, recorded_session, generation,
                        '不要调用任何工具。只回复本会话保存的完整测试口令，不要附加文字。',
                        marker, min(args.turn_timeout, remaining()))
            if live['native_cursor'] != original_cursor or live['model'] != args.expected_model:
                raise ProbeFailure('live_resume_changed_history_or_model')
            report.update(live_resume=live, live_resume_model_before_configure=model_before,
                          per_turn_configure_verified=True, live_actor_preserved=True,
                          status='ws_resume_passed', passed=True)
            stage('live_attach_and_per_turn_configure_completed', model=live['model'],
                  native_cursor_preserved=True, marker_matched=True)
        finally:
            errors = []
            owned = owned_actor_ids(state_path)
            released = set()
            if client is None and owned:
                try:
                    client = make_client(args.expected_model)
                except Exception as error:
                    errors.append(type(error).__name__)
            if client is not None:
                for actor in sorted(owned):
                    try:
                        client.close_session(actor)
                        released.add(actor)
                    except Exception as error:
                        errors.append(type(error).__name__)
                try:
                    client.close()
                except Exception as error:
                    errors.append(type(error).__name__)
            report['cleanup'] = {'actors_recorded': len(owned), 'actors_released': len(released),
                                 'passed': not errors and owned == released,
                                 'production_session_database_opened': False}
            if errors:
                report['cleanup']['error_types'] = errors
            if report['cleanup']['passed'] is False:
                report.update(status='failed', passed=False, failure_code='synthetic_actor_cleanup_incomplete')


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--run', action='store_true')
    parser.add_argument('--expected-model', default='gemini-3.8-flash-high')
    parser.add_argument('--previous-model', default='gpt-6-luna')
    parser.add_argument('--timeout', type=float, default=300)
    parser.add_argument('--turn-timeout', type=float, default=90)
    parser.add_argument('--report', type=Path)
    args = parser.parse_args(argv)
    if not all(re.fullmatch(r'[A-Za-z0-9_.:/-]{1,160}', name) for name in (args.expected_model, args.previous_model)):
        parser.error('model identifiers must be public strings without whitespace')
    if args.expected_model == args.previous_model:
        parser.error('expected-model and previous-model must differ')
    if not 30 <= args.timeout <= 480 or not 10 <= args.turn_timeout <= 180:
        parser.error('timeout must be 30..480 and turn-timeout must be 10..180 seconds')
    report = {'status': 'not_run', 'passed': False, 'expected_model': args.expected_model,
              'previous_model': args.previous_model, 'transport': 'ws', 'mode': 'fullAccess',
              'approval': 'allow_always', 'requires_native_audit': True,
              'native_model_receipts_verified': False, 'physical_toy_audio_published': False,
              'production_session_database_opened': False, 'stages': []}
    began = time.monotonic()
    if args.run:
        try:
            run(args, report)
        except ProbeFailure as error:
            report.update(status='failed', passed=False, failure_code=str(error))
        except Exception as error:
            # Raw SDK errors may contain provider inputs or private endpoints.
            report.update(status='failed', passed=False, error_type=type(error).__name__)
    report['elapsed_ms'] = round((time.monotonic() - began) * 1000)
    if args.report:
        args.report.parent.mkdir(parents=True, exist_ok=True)
        args.report.write_text(json.dumps(report, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
    emit(report)
    return 0 if not args.run or report['passed'] else 1


if __name__ == '__main__':
    raise SystemExit(main())
