"""Run isolated natural-language tool checks through the deployed FoloToy SDK.

Running this script makes real model/tool requests. Pipe it into the FoloToy
container as Python stdin; select one case with --case or TEST_CASE. Each case
uses the real role 1 settings and its own temporary conversation database.
Only conversation completion is verified here. The caller must audit the native
tool receipts using native_cursor before calling a weather/cloud check passed.
"""

import argparse
from contextlib import closing
import json
import logging
import os
from pathlib import Path
import secrets
import sqlite3
import sys
import tempfile
import time


CASES = {
    'weather': '北京明天的天气怎么样？会不会下雨，出门怎么穿？',
    'cloud_linux': '请用华为云 Linux 技能只读检查服务器状态，简短说一下负载、内存和磁盘，不修改配置。',
    'cloud_sandbox': '请用华为云沙箱技能只读检查沙箱运行状态，不修改策略。',
}


class CheckFailure(Exception):
    """A fixed diagnostic code with no remote/private exception content."""


def emit(value):
    print(json.dumps(value, ensure_ascii=False), flush=True)


def configure_source():
    # __file__ is <stdin> for docker exec -i python3 -, so prefer the real cwd.
    candidates = (Path.cwd(), Path('/opt/folotoy'))
    for source in candidates:
        if (source / 'config.py').is_file() and (source / 'sdk/llm_sdk/deerflow.py').is_file():
            sys.path.insert(0, str(source))
            return
    raise CheckFailure('folotoy_source_not_found')


def actor_ids(state_path):
    if not state_path.is_file():
        return set()
    with closing(sqlite3.connect(str(state_path))) as db:
        exists = db.execute("SELECT 1 FROM sqlite_master WHERE type='table' "
                            "AND name='deerflow_ws_sessions'").fetchone()
        return ({row[0] for row in db.execute('SELECT session_id FROM deerflow_ws_sessions')}
                if exists else set())


def run_case(name, configuration, get_role_config, provider, *, timeout=120):
    started = time.monotonic()
    result = {'case': name, 'status': 'running', 'role': '1',
              'transport': 'ws', 'turn_timeout_seconds': timeout,
              'tool_receipts_verified': False,
              'requires_native_tool_audit': True}
    role = configuration.ROLES.get('1')
    if (not isinstance(role, dict)
            or role.get('llm_type', getattr(configuration, 'LLM_TYPE', None)) != 'deerflow'):
        raise CheckFailure('role_1_is_not_deerflow')
    selected = dict(get_role_config('deerflow', role, configuration))
    if (selected.get('transport') != 'ws'
            or selected.get('ws_mode') != 'fullAccess'
            or selected.get('approval') != 'allow_always'):
        raise CheckFailure('role_1_policy_is_not_full_access_ws')
    client = None
    session_id = None
    chunks = None
    with tempfile.TemporaryDirectory(prefix='folotoy-tools-' + name + '-') as temporary:
        state_path = Path(temporary) / 'sessions.sqlite3'
        # Override after loading real settings; never open production mappings.
        selected.update(state_path=str(state_path), turn_timeout=timeout,
                        connect_timeout=15, cancel_timeout=5)
        try:
            client = provider.get_client(selected)
            if client.mode != 'fullAccess' or client.approval != 'allow_always':
                raise CheckFailure('actual_ws_client_policy_mismatch')
            reply = []
            completed = False
            stop_reason = None
            chunks = provider.chat(
                messages=[{'role': 'user', 'content': CASES[name]}],
                llm_config=selected, stream=True,
                dkey='TOOLSVERIFY' + name.upper() + secrets.token_hex(10).upper(),
                role='1', request_id='tools-' + name + '-' + secrets.token_hex(12))
            for chunk in chunks:
                if not isinstance(chunk, dict):
                    continue
                metadata = chunk.get('extra_data') or {}
                if metadata.get('conversation_id'):
                    session_id = str(metadata['conversation_id'])
                    result['waku_session_id'] = session_id
                if chunk.get('type') == 'metadata' and metadata.get('completed'):
                    completed = True
                    stop_reason = metadata.get('stop_reason')
                if (chunk.get('type') == 'delta_message'
                        and chunk.get('role') == 'assistant'
                        and not chunk.get('reasoning')
                        and isinstance(chunk.get('content'), str)):
                    reply.append(chunk['content'])
            full_reply = ''.join(reply).strip()
            result.update(reply=full_reply[:2400], reply_truncated=len(full_reply) > 2400,
                          completed=completed, stop_reason=stop_reason,
                          waku_session_id=session_id, mode=client.mode,
                          approval=client.approval)
            if not completed or stop_reason != 'end_turn' or not session_id or not full_reply:
                raise CheckFailure('turn_completion_session_or_reply_missing')
            session = client._load(session_id)
            cursor = session.cursor or {}
            native_cursor = {key: cursor[key] for key in ('provider', 'sessionId') if key in cursor}
            result.update(native_cursor=native_cursor,
                          waku_runtime_id=session.runtime, server_model=session.model,
                          confirmed_approval=session.approval)
            if (native_cursor.get('provider') != 'deerFlow'
                    or not native_cursor.get('sessionId')
                    or session.approval != 'allow_always'):
                raise CheckFailure('native_cursor_or_confirmed_approval_missing')
            # A normal assistant answer can still be wrong or contain no tools.
            result['status'] = 'conversation_completed'
        except CheckFailure as error:
            result.update(status='failed', failure_code=str(error))
        except Exception as error:
            # Remote/config exception text may include private values.
            result.update(status='failed', error_type=type(error).__name__)
        finally:
            cleanup_errors = []
            if chunks is not None:
                try:
                    chunks.close()
                except Exception as error:
                    cleanup_errors.append(type(error).__name__)
            if client is not None and session_id and 'native_cursor' not in result:
                try:
                    session = client._load(session_id)
                    cursor = session.cursor or {}
                    result.update(native_cursor={key: cursor[key] for key in ('provider', 'sessionId') if key in cursor},
                                  waku_runtime_id=session.runtime, confirmed_approval=session.approval)
                except Exception as error:
                    result['session_metadata_error_type'] = type(error).__name__
            try:
                recorded = actor_ids(state_path)
            except Exception as error:
                recorded = set()
                cleanup_errors.append(type(error).__name__)
            if session_id:
                recorded.add(session_id)
            released = set()
            if client is not None:
                for actor in sorted(recorded):
                    try:
                        client.close_session(actor)
                        released.add(actor)
                    except Exception as error:
                        cleanup_errors.append(type(error).__name__)
            try:
                provider.close_all()
            except Exception as error:
                cleanup_errors.append(type(error).__name__)
            result['cleanup'] = {'actors_recorded': len(recorded),
                                 'actors_released': len(released),
                                 'passed': not cleanup_errors and recorded == released}
            if cleanup_errors:
                result['cleanup']['error_types'] = cleanup_errors
            if not result['cleanup']['passed']:
                result['status'] = 'failed'
    result['elapsed_ms'] = round((time.monotonic() - started) * 1000)
    return result


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--case', choices=(*CASES, 'all'), default=os.environ.get('TEST_CASE', 'all'))
    parser.add_argument('--timeout', type=float, default=120,
                        help='Per-case provider deadline, greater than zero and at most 120 seconds.')
    args = parser.parse_args(argv)
    if args.case not in (*CASES, 'all'):
        parser.error('TEST_CASE must be weather, cloud_linux, cloud_sandbox or all')
    if not 0 < args.timeout <= 120:
        parser.error('--timeout must be greater than zero and at most 120 seconds')
    logging.disable(logging.CRITICAL)
    provider = None
    results = []
    final_cleanup_ok = True
    try:
        configure_source()
        from config import Config
        from util.config_util import get_role_llm_config
        from sdk.llm_sdk import deerflow as provider
        configuration = Config()
        names = list(CASES) if args.case == 'all' else [args.case]
        for name in names:
            result = run_case(name, configuration, get_role_llm_config, provider,
                              timeout=args.timeout)
            results.append(result)
            emit(result)
    except CheckFailure as error:
        emit({'case': args.case, 'status': 'failed', 'failure_code': str(error)})
        return 1
    except Exception as error:
        emit({'case': args.case, 'status': 'failed', 'error_type': type(error).__name__})
        return 1
    finally:
        if provider is not None:
            try:
                provider.close_all()
            except Exception as error:
                emit({'status': 'failed', 'cleanup_error_type': type(error).__name__})
                final_cleanup_ok = False
    return 0 if final_cleanup_ok and results and all(r['status'] == 'conversation_completed' for r in results) else 1


if __name__ == '__main__':
    raise SystemExit(main())
