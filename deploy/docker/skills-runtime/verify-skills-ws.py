"""Verify native skill_run readiness and pinned --help execution over Waku WS.

No network or SDK access occurs without --run. The real run makes model calls.
Only event-backed tool results count as success; assistant assertions do not.
"""

import argparse
import json
import os
from pathlib import Path
import re
import sys
import tempfile
import threading
import time

SKILL = 'huaweicloud-sandbox'
ENTRYPOINT = 'scripts/huaweicloud_sandbox.py'


def decode_object(value):
    if isinstance(value, dict):
        return value
    if isinstance(value, str):
        try:
            parsed = json.loads(value)
        except (TypeError, ValueError):
            return None
        return parsed if isinstance(parsed, dict) else None
    return None


def result_objects(value, depth=0):
    """Unwrap ACP content envelopes without guessing from assistant prose."""
    if depth > 10:
        return
    if isinstance(value, str):
        try:
            value = json.loads(value)
        except (TypeError, ValueError):
            # Waku can join multiple ACP content entries with blank lines.
            decoder = json.JSONDecoder()
            offset = 0
            while offset < len(value):
                match = re.search(r'[\[{]', value[offset:])
                if match is None:
                    break
                offset += match.start()
                try:
                    entry, end = decoder.raw_decode(value, offset)
                except ValueError:
                    offset += 1
                    continue
                offset = end
                yield from result_objects(entry, depth + 1)
            return
    if isinstance(value, dict):
        if value.get('name') == SKILL and isinstance(value.get('readiness'), dict):
            yield value
        for key in ('content', 'text', 'structuredContent', 'result', 'data'):
            if key in value:
                yield from result_objects(value[key], depth + 1)
    elif isinstance(value, list):
        for entry in value:
            yield from result_objects(entry, depth + 1)


class ToolAudit:
    def __init__(self, cancellation):
        self.lock = threading.RLock()
        self.calls = {}
        self.unexpected = set()
        self.cancellation = cancellation

    def observe(self, event):
        if event.get('kind') != 'richActivity':
            return
        value = event.get('payload')
        if not isinstance(value, dict):
            return
        call_id = value.get('source_id') or value.get('sourceId') or value.get('id')
        if not isinstance(call_id, str):
            return
        if call_id == 'acp-plan':
            return  # Native plan progress is not an executed tool call.
        with self.lock:
            if call_id not in self.calls and len(self.calls) >= 64:
                self.unexpected.add('activity_limit_exceeded')
                self.cancellation.set()
                return
            call = self.calls.setdefault(call_id, {})
            title = value.get('tool_name') or value.get('toolName') or value.get('title')
            if isinstance(title, str) and title:
                call['tool'] = title if re.fullmatch(r'[A-Za-z_][A-Za-z0-9_]{0,100}', title) else 'unrecognized_activity'
            arguments = decode_object(value.get('arguments'))
            if arguments is not None:
                call['arguments'] = arguments
            if value.get('output') is not None:
                call['output'] = value['output']
            if value.get('complete') is True:
                call['complete'] = True
                call['failed'] = bool(value.get('failed'))
            tool = call.get('tool')
            if tool and tool not in ('skill', 'read_skill_file', 'skill_run'):
                self.unexpected.add(tool)
                self.cancellation.set()
            if tool == 'skill_run' and arguments:
                safe = (arguments.get('name') == SKILL and (
                    arguments.get('action') == 'check'
                    and not arguments.get('path') and not arguments.get('args')
                    or arguments.get('action') == 'run'
                    and arguments.get('path') == ENTRYPOINT
                    and arguments.get('args') == ['--help']))
                if not safe:
                    self.unexpected.add('unexpected_skill_run_arguments')
                    self.cancellation.set()
            if tool == 'read_skill_file' and arguments:
                if (arguments.get('skill') != SKILL
                        or arguments.get('path') not in ('SKILL.md', 'skill-runtime.json', 'requirements.txt')):
                    self.unexpected.add('unexpected_skill_file')
                    self.cancellation.set()

    def summary(self):
        with self.lock:
            calls = list(self.calls.values())
            unexpected = sorted(self.unexpected)
        loaded = False
        checks = []
        executions = []
        names = []
        for call in calls:
            tool = call.get('tool', '')
            names.append(tool)
            if not call.get('complete') or call.get('failed'):
                continue
            arguments = call.get('arguments') or {}
            output = call.get('output', '')
            if tool in ('skill', 'read_skill_file'):
                text = output if isinstance(output, str) else json.dumps(output)
                if SKILL in text and ('# Huawei Cloud Sandbox' in text or 'Installed skill:' in text
                                      or re.search(r'^name:\s*[\"\x27]?huaweicloud-sandbox\b', text, re.M)):
                    loaded = True
            if tool != 'skill_run' or arguments.get('name') != SKILL:
                continue
            for result in result_objects(output):
                readiness = result['readiness']
                if (arguments.get('action') == 'check'
                        and readiness.get('ready') is True
                        and result.get('execution_checked') is False):
                    checks.append({'ready': True,
                                   'dependencies_declared': readiness.get('dependencies_declared') is True,
                                   'python_version': readiness.get('python_version'),
                                   'paramiko_version': (readiness.get('packages') or {}).get('paramiko'),
                                   'version': result.get('version')})
                if (arguments.get('action') != 'run' or arguments.get('path') != ENTRYPOINT
                        or arguments.get('args') != ['--help']):
                    continue
                command = result.get('command') or {}
                stdout = command.get('stdout') or {}
                text = stdout.get('text', '')
                if (readiness.get('ready') is True and result.get('execution_checked') is True
                        and command.get('exitCode') == 0
                        and command.get('terminationConfirmed') is True
                        and command.get('state') == 'completed'
                        and stdout.get('truncated') is not True
                        and isinstance(text, str) and 'usage:' in text.lower()
                        and '--known-hosts' in text):
                    executions.append({'execution_checked': True, 'exit_code': 0,
                                       'termination_confirmed': True, 'help_output_verified': True,
                                       'version': result.get('version')})
        pinned = any(check.get('version') and check.get('version') == run.get('version')
                     for check in checks for run in executions)
        return {'skill_loaded_from_tool_result': loaded,
                'actual_tool_calls': names,
                'readiness_receipts': checks,
                'execution_receipts': executions,
                'same_pinned_version': pinned,
                'unexpected_tool_calls': unexpected,
                'passed': bool(loaded and checks and executions and pinned and not unexpected)}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--run', action='store_true')
    parser.add_argument('--url', default=os.environ.get('DEERFLOW_WS_URL'))
    parser.add_argument('--workspace', default='/workspaces')
    parser.add_argument('--token-env', default='WAKU_DAEMON_TOKEN')
    parser.add_argument('--binary', default='/var/lib/deerflow/deerflow-acp')
    parser.add_argument('--timeout', type=float, default=120)
    parser.add_argument('--report', type=Path)
    args = parser.parse_args(argv)
    if not args.run:
        print(json.dumps({'status': 'not_run', 'reason': 'Supply --run for a real model and skill execution test.'}))
        return 0
    if (not args.url or not re.fullmatch(r'[A-Za-z_][A-Za-z0-9_]*', args.token_env)
            or not os.environ.get(args.token_env) or not 0 < args.timeout <= 300):
        parser.error('Configure --url, a token environment variable, and a timeout between 0 and 300 seconds.')
    for source in (Path.cwd(), Path('/opt/folotoy')):
        if (source / 'sdk/deerflow_ws_client.py').is_file():
            sys.path.insert(0, str(source))
            break
    from sdk.deerflow_ws_client import DeerFlowWsClient

    cancellation = threading.Event()
    audit = ToolAudit(cancellation)

    class AuditedClient(DeerFlowWsClient):
        def _event(self, message, generation):
            with self._lock:
                session = self._sessions.get(message.get('sessionId'))
                own = (session is not None and message.get('runtimeId') == session.runtime
                       and session.generation == generation)
            if own:
                audit.observe(message.get('event') or {})
            super()._event(message, generation)

    report = {'status': 'running', 'skill': SKILL, 'mode': 'fullAccess',
              'evidence_source': 'server richActivity tool inputs and outputs'}
    started = time.monotonic()
    client = None
    session_id = None
    prompt = (
        '执行本次技能迁移验收，严格只完成下面三个步骤。首先使用 skill 工具加载 huaweicloud-sandbox '
        '（或用 read_skill_file 读取它的 SKILL.md）。然后调用 '
        'skill_run，参数 {"action":"check","name":"huaweicloud-sandbox"}。'
        '只有返回 readiness.ready=true 后，再调用 skill_run，参数 '
        '{"action":"run","name":"huaweicloud-sandbox","path":"scripts/huaweicloud_sandbox.py",'
        '"args":["--help"],"timeout_seconds":20}。'
        '本次只授权这两个 skill_run 调用以及读取该技能的说明、runtime manifest、requirements。'
        '禁止调用 execute、shell、SSH、任何网络工具、subagent、安装或其他命令；'
        '不要执行技能说明中的 status、get-config 或 run 等远程主机操作。'
        '--help 是唯一允许的脚本参数，它会在连接SSH之前退出。'
        '不得用模型自述代替实际工具调用。完成后仅简短报告实际结果。'
    )
    try:
        with tempfile.TemporaryDirectory(prefix='deerflow-skill-ws-') as temporary:
            try:
                client = AuditedClient({'transport': 'ws', 'ws_url': args.url,
                                        'workspace': args.workspace, 'server_binary': args.binary,
                                        'token_env': args.token_env, 'ws_mode': 'fullAccess',
                                        'approval': 'allow_always', 'connect_timeout': 25,
                                        'cancel_timeout': 5, 'event_queue_size': 512,
                                        'state_path': str(Path(temporary) / 'state.sqlite3')})
                generation = client.ensure_connected()
                info = client.request('session/new', {'cwd': args.workspace, 'mcpServers': []},
                                      generation=generation, timeout=25)
                session_id = info['sessionId']
                client.remember_session(session_id, generation, info)
                report['session_id'] = session_id
                text_length = 0
                finished = False
                for event in client.stream_prompt(session_id, prompt, generation=generation,
                                                  cancel_event=cancellation, turn_timeout=args.timeout,
                                                  cancel_timeout=5):
                    if '_completion' in event:
                        finished = event['_completion'].get('stopReason') == 'end_turn'
                    elif event.get('sessionUpdate') == 'agent_message_chunk':
                        text_length += len((event.get('content') or {}).get('text', ''))
                session = client._load(session_id)
                if isinstance(session.cursor, dict):
                    report['native_session_id'] = session.cursor.get('sessionId')
                report.update(audit.summary())
                report['turn_completed'] = finished
                report['assistant_text_characters'] = text_length
                report['status'] = 'passed' if finished and report['passed'] else 'failed'
            finally:
                if client is not None:
                    try:
                        if session_id:
                            session = client._load(session_id)
                            if isinstance(session.cursor, dict):
                                report['native_session_id'] = session.cursor.get('sessionId')
                            client.close_session(session_id)
                            report['test_actor_released'] = True
                    except Exception as error:
                        report['test_actor_released'] = False
                        report['cleanup_error_type'] = type(error).__name__
                    finally:
                        client.close()
    except Exception as error:
        report.update(audit.summary())
        report['status'] = 'failed'
        report['error_type'] = type(error).__name__
    report['elapsed_ms'] = round((time.monotonic() - started) * 1000)
    if report.get('test_actor_released') is False:
        report['status'] = 'failed'
    if args.report:
        args.report.parent.mkdir(parents=True, exist_ok=True)
        args.report.write_text(json.dumps(report, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
    print(json.dumps(report, ensure_ascii=False), flush=True)
    return 0 if report['status'] == 'passed' else 1


if __name__ == '__main__':
    raise SystemExit(main())
