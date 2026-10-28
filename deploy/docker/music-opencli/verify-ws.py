"""One bounded administrative WS turn for migrated music/OpenCLI acceptance.

This script makes real model/tool requests when executed, including real audio
playback for music_play_admin. It does not call the FoloToy LLM provider or invent
voice-turn identity. The parent must audit native tool receipts before claiming
search, download, browser access, or physical playback success.
"""
import argparse
from contextlib import closing
import json
import logging
import os
from pathlib import Path, PurePosixPath
import re
import sqlite3
import sys
import tempfile
import threading
import time

CASES = ('music_search_kuwo', 'music_search_qq', 'music_download_qq',
         'opencli_doctor', 'opencli_read', 'music_play_admin')


class ProbeError(Exception):
    pass


def prompt_for(case, media=None):
    if case == 'music_search_kuwo':
        return '请使用 song-download 技能实际搜索歌曲“晴天”，只列前三项标题和歌手。不下载、不播放，不改用其他搜索来源。请根据实际工具结果回答。'
    if case == 'music_search_qq':
        return '请使用 qq-music 技能实际搜索歌曲“晴天”，只列前三项标题和歌手。不下载、不播放，不改用其他搜索来源。请根据实际工具结果回答。'
    if case == 'music_download_qq':
        return ('请使用 qq-music 技能实际下载明确指定的歌曲 MID 000BEsDj0UUhf8，'
                '保存到共享音乐目录，使用默认适合播放的音质。只下载这首，不播放，也不要换成其他歌曲。'
                '请报告实际返回的相对路径、文件大小及摘要；若没有可用链接或失败，说明失败，不猜测成功。')
    if case == 'opencli_doctor':
        return ('请加载 opencli 技能，通过 host_opencli 工具实际执行一次 doctor 连接诊断，'
                '报告宿主 OpenCLI daemon 和浏览器插件的实际连接状态。只做这项诊断，不搜索、不发帖、不操作其他应用。')
    if case == 'opencli_read':
        return ('请使用 opencli 技能的 host_opencli 工具，读取哔哩哔哩当前热门视频并列出前三项标题。'
                '只读取，不登录、不点赞、不评论、不发布。使用实际安装版本支持的命令，必要时可查命令帮助；'
                '若浏览器或接口失败，直接报告，不编造结果。')
    if case == 'music_play_admin':
        if not media or '\\' in media or '\0' in media:
            raise ProbeError('test_media_relative_required')
        path = PurePosixPath(media)
        if path.is_absolute() or '..' in path.parts or path.suffix.lower() not in ('.mp3', '.wav', '.flac', '.m4a', '.ogg', '.aac', '.opus'):
            raise ProbeError('invalid_test_media_relative')
        return ('这是独立桌面/管理会话，没有正在进行的语音轮次。请使用 toy-playmusic 技能，把共享根目录中'
                + json.dumps(media, ensure_ascii=False) + '这一个已准备的短音频加入已配置默认设备的播放队列。'
                '不要下载文件，不要添加或编造 request_id、device_key，调用时省略这两参数，并用 --default-device 明确选择默认设备。'
                '只提交一次 play；如果得到 job.id，可以查询该任务状态，不重复播放。'
                '报告实际排队或下发状态，published 只代表 MQTT 已发，不能说扬声器已播放完成。')
    raise ProbeError('unknown_test_case')


def actor_ids(path):
    if not path.is_file():
        return set()
    with closing(sqlite3.connect(str(path))) as db:
        exists = db.execute("SELECT 1 FROM sqlite_master WHERE type='table' AND name='deerflow_ws_sessions'").fetchone()
        return {row[0] for row in db.execute('SELECT session_id FROM deerflow_ws_sessions')} if exists else set()


def clean_argument(value):
    if isinstance(value, list):
        result, hidden = [], False
        for item in value:
            if hidden:
                result.append('[REDACTED]');hidden = False;continue
            result.append(clean_argument(item))
            hidden = isinstance(item, str) and item.lower() in ('--apikey', '--api-key', '--token', '--password')
        return result
    if isinstance(value, str):
        value = re.sub(r'(?i)Bearer\s+[^\s"\']+', 'Bearer [REDACTED]', value)
        return re.sub(r'(?i)([?&](?:apikey|api_key|token|key)=)[^\s&"\']+', r'\1[REDACTED]', value)[:500]
    return value if isinstance(value, (int, float, bool)) or value is None else '[omitted]'


class SafeActivities:
    def __init__(self):
        self.lock = threading.Lock()
        self.calls = {}

    def observe(self, event):
        if event.get('kind') != 'richActivity' or not isinstance(event.get('payload'), dict):
            return
        value = event['payload']
        source = value.get('source_id') or value.get('sourceId') or value.get('id')
        if not isinstance(source, str) or source == 'acp-plan':
            return
        name = value.get('tool_name') or value.get('toolName') or value.get('title')
        with self.lock:
            if source not in self.calls and len(self.calls) >= 64:
                return
            item = self.calls.setdefault(source, {})
            if isinstance(name, str) and re.fullmatch(r'[a-zA-Z_][a-zA-Z0-9_]{0,99}', name):
                item['name'] = name
            arguments = value.get('arguments')
            if isinstance(arguments, str):
                try:arguments = json.loads(arguments)
                except ValueError:arguments = None
            if isinstance(arguments, dict) and item.get('name') in ('skill', 'read_skill_file', 'skill_run', 'host_opencli'):
                item['arguments'] = {key: clean_argument(arguments[key])
                                     for key in ('skill', 'name', 'action', 'path', 'args', 'site', 'command', 'arguments')
                                     if key in arguments}
            if value.get('complete') is True:
                item['complete'], item['failed'] = True, bool(value.get('failed'))
            # Deliberately do not retain tool output or arbitrary execute/read args.

    def snapshot(self):
        with self.lock:
            return list(self.calls.values())


def source_path():
    for path in (Path.cwd(), Path('/opt/folotoy')):
        if (path / 'sdk/deerflow_ws_client.py').is_file():
            sys.path.insert(0, str(path));return
    raise ProbeError('ws_sdk_source_not_found')


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--case', choices=CASES, default=os.environ.get('TEST_CASE'))
    parser.add_argument('--timeout', type=float, default=None)
    parser.add_argument('--media', default=os.environ.get('TEST_MEDIA_RELATIVE'))
    parser.add_argument('--report', type=Path)
    args = parser.parse_args(argv)
    if args.case not in CASES:
        parser.error('Select TEST_CASE or --case')
    timeout = args.timeout if args.timeout is not None else (240 if args.case == 'music_download_qq' else 120)
    if not 15 <= timeout <= 240:
        parser.error('Timeout must be between 15 and 240 seconds')
    logging.disable(logging.CRITICAL)
    report = {'case': args.case, 'status': 'running', 'transport': 'ws',
              'mode': 'fullAccess', 'approval': 'allow_always', 'turn_timeout_seconds': timeout,
              'tool_receipts_verified': False, 'requires_native_tool_audit': True,
              'voice_turn_context_injected': False}
    started = time.monotonic()
    client, session_id, stream = None, None, None
    audit = SafeActivities()
    try:
        prompt = prompt_for(args.case, args.media)
        source_path()
        from sdk.deerflow_ws_client import DeerFlowWsClient
        token_env = os.environ.get('DEERFLOW_TOKEN_ENV', 'WAKU_DAEMON_TOKEN')
        if not re.fullmatch(r'[A-Za-z_][A-Za-z0-9_]*', token_env) or not os.environ.get(token_env):
            raise ProbeError('daemon_token_environment_not_configured')
        url = os.environ.get('DEERFLOW_WS_URL')
        if not url:
            raise ProbeError('deerflow_ws_url_not_configured')

        class AuditedClient(DeerFlowWsClient):
            def _event(self, message, generation):
                with self._lock:
                    session = self._sessions.get(message.get('sessionId'))
                    own = session is not None and message.get('runtimeId') == session.runtime and session.generation == generation
                if own:audit.observe(message.get('event') or {})
                super()._event(message, generation)

        with tempfile.TemporaryDirectory(prefix='deerflow-admin-ws-') as temporary:
            state_path = Path(temporary) / 'state.sqlite3'
            try:
                client = AuditedClient({'transport': 'ws', 'ws_url': url,
                                        'workspace': os.environ.get('DEERFLOW_WORKSPACE', '/workspaces/folotoy'),
                                        'server_binary': os.environ.get('DEERFLOW_SERVER_BINARY', '/var/lib/deerflow/deerflow-acp'),
                                        'token_env': token_env, 'ws_mode': 'fullAccess', 'approval': 'allow_always',
                                        'connect_timeout': 25, 'cancel_timeout': 5, 'event_queue_size': 1024,
                                        'model': os.environ.get('DEERFLOW_MODEL') or None,
                                        'state_path': str(state_path)})
                generation = client.ensure_connected()
                info = client.request('session/new', {'cwd': client.workspace, 'mcpServers': []},
                                      generation=generation, timeout=25)
                session_id = info['sessionId']
                client.remember_session(session_id, generation, info)
                report['waku_session_id'] = session_id
                cancellation = threading.Event()
                stream = client.stream_prompt(session_id, prompt, generation=generation,
                                              cancel_event=cancellation, turn_timeout=timeout, cancel_timeout=5)
                reply, finished = [], False
                for event in stream:
                    if '_completion' in event:
                        finished = event['_completion'].get('stopReason') == 'end_turn'
                    elif event.get('sessionUpdate') == 'agent_message_chunk':
                        reply.append((event.get('content') or {}).get('text', ''))
                session = client._load(session_id)
                cursor = session.cursor or {}
                response = ''.join(reply).strip().replace(os.environ[token_env], '[REDACTED]')
                report.update(native_cursor={key: cursor[key] for key in ('provider', 'sessionId') if key in cursor},
                              native_session_id=cursor.get('sessionId'), waku_runtime_id=session.runtime,
                              confirmed_approval=session.approval, server_model=session.model,
                              completed=finished, stop_reason='end_turn' if finished else None,
                              reply=response[:4000], reply_truncated=len(response) > 4000,
                              status='conversation_completed' if finished and response and cursor.get('sessionId') and session.approval == 'allow_always' else 'failed')
            finally:
                errors, released = [], set()
                if stream is not None:
                    try:stream.close()
                    except Exception as error:errors.append(type(error).__name__)
                if client is not None:
                    if session_id and 'native_cursor' not in report:
                        try:
                            session = client._load(session_id);cursor = session.cursor or {}
                            report.update(native_cursor={key: cursor[key] for key in ('provider', 'sessionId') if key in cursor},
                                          native_session_id=cursor.get('sessionId'), waku_runtime_id=session.runtime)
                        except Exception as error:report['session_metadata_error_type'] = type(error).__name__
                    recorded = actor_ids(state_path)
                    if session_id:recorded.add(session_id)
                    for actor in sorted(recorded):
                        try:client.close_session(actor);released.add(actor)
                        except Exception as error:errors.append(type(error).__name__)
                    try:client.close()
                    except Exception as error:errors.append(type(error).__name__)
                    report['cleanup'] = {'actors_recorded': len(recorded), 'actors_released': len(released),
                                         'passed': not errors and recorded == released}
                    if errors:report['cleanup']['error_types'] = errors
    except ProbeError as error:
        report.update(status='failed', failure_code=str(error))
    except Exception as error:
        report.update(status='failed', error_type=type(error).__name__)
    report['actual_tool_activities'] = audit.snapshot()
    report['elapsed_ms'] = round((time.monotonic() - started) * 1000)
    if report.get('cleanup', {}).get('passed') is False:
        report['status'] = 'failed'
    if args.report:
        args.report.parent.mkdir(parents=True, exist_ok=True)
        args.report.write_text(json.dumps(report, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
    print(json.dumps(report, ensure_ascii=False), flush=True)
    return 0 if report['status'] == 'conversation_completed' and report.get('cleanup', {}).get('passed') else 1


if __name__ == '__main__':
    raise SystemExit(main())
