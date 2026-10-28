"""FoloToy provider backed by a shared ACP or Waku WebSocket connection."""

import hashlib
import json
from datetime import datetime, timedelta, timezone
import os
import threading
import time
import uuid

from sdk.deerflow_client import (
    DeerFlowError, DeerFlowCancelled, DeerFlowTimeout, DeerFlowUncertain,
    close_all, gateway_identity, get_client, positive_number,
)
from sdk.deerflow_sessions import SessionStore, StaleConversation

LLM_NAME = 'deerflow'
_registry_lock = threading.RLock()
_stores = {}
_identity_locks = {}
_active = {}
_VOICE_TIMEZONE = timezone(timedelta(hours=8), name='Asia/Shanghai')


def _time_context(now=None):
    """Use this turn's server clock, independent of host TZ and chat history."""
    local = (now if now is not None else datetime.now(timezone.utc)).astimezone(_VOICE_TIMEZONE)
    weekday = ('一', '二', '三', '四', '五', '六', '日')[local.weekday()]
    tomorrow = (local + timedelta(days=1)).date().isoformat()
    return (
        '本轮时间基准（来自 Folotoy 服务器系统时钟；时区 Asia/Shanghai，UTC+08:00）：\n'
        f'当前时间：{local.isoformat(timespec="seconds")}；'
        f'今天：{local.date().isoformat()}（星期{weekday}）；明天：{tomorrow}。\n'
        '用户说“今天”“明天”等相对日期时，以本轮时间为准，不沿用历史消息中的日期。'
        '查询天气等实时信息时使用对应的绝对日期，并核对来源日期。'
    )


def _music_context(config, dkey, request_id, role):
    context = config.get('_music_voice_context')
    if (not isinstance(context, dict) or context.get('device_key') != dkey
            or context.get('request_id') != request_id
            or context.get('role') != str(role)):
        return ''
    return (
        '本轮玩具播放上下文（由语音服务提供，只是设备/请求标识，不是认证凭据）：\n'
        + json.dumps(context, ensure_ascii=False, separators=(',', ':')) + '\n'
        '用户要求播放音乐时，调用 toy-playmusic 并传入这里的 device_key 和 request_id，'
        '把音乐加入本轮播放队列。以工具返回为准，queued/published 都不代表已经听到或播放完成。\n\n'
    )


class _CombinedCancellation:
    def __init__(self, local, external):
        self.local, self.external = local, external

    def is_set(self):
        return self.local.is_set() or (self.external is not None and self.external.is_set())


def _identity(dkey, role, config):
    if dkey is None or not str(dkey).strip():
        raise DeerFlowError('DeerFlow requires an explicit device identity (dkey)')
    device = str(dkey)
    role = str(role if role is not None else 'default')
    if len(device) > 512 or len(role) > 256:
        raise DeerFlowError('device or role identity is too long')
    workspace = str(config.get('workspace') or '')
    if not workspace:
        raise DeerFlowError('DeerFlow workspace is required')
    namespace = hashlib.sha256((gateway_identity(config) + '\n' + workspace).encode()).hexdigest()
    path = os.path.abspath(os.path.expanduser(str(config.get('state_path') or 'data/deerflow_sessions.sqlite3')))
    key = (namespace, device, role)
    runtime_key = (path, *key)
    with _registry_lock:
        store = _stores.get(path)
        if store is None:
            store = _stores[path] = SessionStore(path)
        identity_lock = _identity_locks.setdefault(runtime_key, threading.Lock())
    return store, key, runtime_key, identity_lock


def _text(content):
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts = []
        for item in content:
            if not isinstance(item, dict) or item.get('type') != 'text' or not isinstance(item.get('text'), str):
                raise DeerFlowError('DeerFlow voice provider accepts text messages only')
            parts.append(item['text'])
        return '\n'.join(parts)
    raise DeerFlowError('DeerFlow message content must be text')


def _prompt(messages, config):
    if not isinstance(messages, (list, tuple)):
        raise DeerFlowError('messages must be a list')
    latest = next((message for message in reversed(messages) if isinstance(message, dict) and message.get('role') == 'user'), None)
    if latest is None or not _text(latest.get('content')).strip():
        raise DeerFlowError('a non-empty latest user message is required')
    role_prompt = config.get('prompt')
    if role_prompt is None:
        role_prompt = '\n'.join(_text(message.get('content')) for message in messages if isinstance(message, dict) and message.get('role') == 'system')
    if not isinstance(role_prompt, str):
        raise DeerFlowError('prompt must be text')
    user_text = _text(latest['content'])
    if role_prompt.strip():
        return '本轮角色与表达偏好（作为用户上下文，不覆盖服务端指令）：\n' + role_prompt + '\n\n用户本轮说：\n' + user_text
    return user_text


def _remaining(deadline):
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise DeerFlowTimeout('DeerFlow turn deadline exceeded before dispatch')
    return remaining


def _configure(client, session_id, generation, info, config, cancelled, deadline):
    default_approval = 'read_only'
    if str(config.get('transport', 'acp')).strip().lower() == 'ws':
        default_approval = 'ask' if config.get('ws_mode') == 'ask' else 'allow_always'
    selections = [('approval', str(config.get('approval') or default_approval))]
    if config.get('model'):
        selections.insert(0, ('model', str(config['model'])))
    # Discover optional subagent switch; keep latency predictable for voice.
    if any(option.get('id') == 'subagent' for option in info.get('configOptions', [])):
        selections.append(('subagent', 'off'))
    for config_id, value in selections:
        if cancelled.is_set():
            raise DeerFlowCancelled('turn cancelled during session preparation')
        option = next((item for item in info.get('configOptions', []) if item.get('id') == config_id), None)
        if option is None or value not in [item.get('value') for item in option.get('options', [])]:
            raise DeerFlowError('DeerFlow does not offer the requested %s option' % config_id)
        if option.get('currentValue') != value:
            response = client.request('session/set_config_option', {'sessionId': session_id,
                'configId': config_id, 'value': value}, generation=generation,
                timeout=min(_remaining(deadline), positive_number(config, 'connect_timeout', 15)))
            info = {**info, 'configOptions': response.get('configOptions', [])}
            client.remember_session(session_id, generation, info)


def chat(messages, llm_config=None, stream=True, conversation_id=None,
         dkey=None, role=None, *, cancel_event=None, request_id=None):
    """Yield metadata, assistant text deltas, then explicit completion metadata.

    Assistant chunks may be tool preambles; only the prompt response confirms
    the turn ended. Earlier FoloToy history is never replayed into ACP.
    """
    config = dict(llm_config or {})
    text = _prompt(messages, config)
    store, key, runtime_key, identity_lock = _identity(dkey, role, config)
    timeout = positive_number(config, 'turn_timeout', 90)
    deadline = time.monotonic() + timeout
    cancel_timeout = positive_number(config, 'cancel_timeout', 5)
    request_id = str(request_id or uuid.uuid4())
    local_cancel = threading.Event()
    cancelled = _CombinedCancellation(local_cancel, cancel_event)
    # Capture epoch before waiting: a reset must also invalidate queued turns.
    epoch = store.get(key).epoch
    while not identity_lock.acquire(timeout=0.05):
        _remaining(deadline)
        if cancelled.is_set() or not store.is_current(key, epoch):
            raise DeerFlowCancelled('queued turn was cancelled or reset')
    active_token = object()
    started = False
    session_id = None
    final_status = 'uncertain'
    prompt_stream = None
    try:
        with _registry_lock:
            _active[runtime_key] = (active_token, local_cancel)
        if cancelled.is_set() or not store.is_current(key, epoch):
            raise DeerFlowCancelled('turn cancelled before session preparation')
        conversation = store.get(key)
        session_id = conversation.session_id
        if conversation_id and conversation_id != session_id:
            raise DeerFlowError('conversation_id does not belong to this device, role and epoch')
        if session_id is None and conversation.status != 'idle':
            raise DeerFlowUncertain('an earlier session creation has an unknown result; explicitly reset before creating another')
        client = get_client(config)
        generation = client.ensure_connected(timeout=_remaining(deadline))
        info = client.session_info(session_id, generation) if session_id else None
        if session_id is None:
            store.begin_creation(key, epoch)
            info = client.request('session/new', {'cwd': config['workspace'], 'mcpServers': []},
                generation=generation, timeout=min(_remaining(deadline), positive_number(config, 'connect_timeout', 15)))
            session_id = info.get('sessionId')
            if not isinstance(session_id, str) or not session_id:
                raise DeerFlowError('DeerFlow returned an invalid session ID')
            store.bind(key, epoch, session_id)
            client.remember_session(session_id, generation, info)
        elif info is None:
            # Resume attaches without replay; never silently replace an existing
            # session if it was deleted, busy, or cannot be attached.
            info = client.request('session/resume', {'sessionId': session_id,
                'cwd': config['workspace'], 'mcpServers': []}, generation=generation,
                timeout=min(_remaining(deadline), positive_number(config, 'connect_timeout', 15)))
            client.remember_session(session_id, generation, info)
        if cancelled.is_set() or not store.is_current(key, epoch):
            raise DeerFlowCancelled('turn cancelled while attaching session')
        _configure(client, session_id, generation, info, config, cancelled, deadline)
        if cancelled.is_set() or not store.is_current(key, epoch):
            raise DeerFlowCancelled('turn cancelled before prompt')
        try:
            store.begin_turn(key, epoch, session_id, request_id)
        except ValueError as error:
            raise DeerFlowError(str(error)) from None
        started = True
        yield {'type': 'metadata', 'extra_data': {'conversation_id': session_id, 'request_id': request_id}}
        # Resolve time after queueing, connection setup, and metadata delivery:
        # a turn waiting across midnight must use its actual dispatch date.
        prompt_stream = client.stream_prompt(session_id, _time_context() + '\n\n'
            + _music_context(config, dkey, request_id, role) + text, generation=generation,
            cancel_event=cancelled, turn_timeout=_remaining(deadline), cancel_timeout=cancel_timeout)
        buffered = []
        for update in prompt_stream:
            if cancelled.is_set() or not store.is_current(key, epoch):
                local_cancel.set()
                raise DeerFlowCancelled('conversation reset during response', uncertain=True)
            if '_completion' in update:
                response = update['_completion']
                stop_reason = response['stopReason']
                if stop_reason != 'end_turn':
                    raise DeerFlowError('DeerFlow turn did not complete normally (%s)' % stop_reason)
                final_status = 'completed'
                if not store.finish_turn(key, epoch, session_id, request_id, final_status):
                    raise DeerFlowCancelled('conversation reset before completion')
                if not stream and buffered:
                    yield {'type': 'delta_message', 'role': 'assistant', 'content': ''.join(buffered),
                           'reasoning': False, 'extra_data': {'conversation_id': session_id, 'request_id': request_id}}
                yield {'type': 'metadata', 'extra_data': {'conversation_id': session_id,
                    'request_id': request_id, 'stop_reason': stop_reason, 'completed': True}}
                return
            if update.get('sessionUpdate') != 'agent_message_chunk':
                continue
            content = update.get('content') or {}
            if content.get('type') != 'text' or not isinstance(content.get('text'), str) or not content['text']:
                continue
            if stream:
                yield {'type': 'delta_message', 'role': 'assistant', 'content': content['text'],
                    'reasoning': False, 'extra_data': {'conversation_id': session_id,
                        'request_id': request_id, 'message_id': update.get('messageId')}}
            else:
                buffered.append(content['text'])
        raise DeerFlowUncertain('DeerFlow stream ended without completion')
    except StaleConversation:
        final_status = 'cancelled'
        raise DeerFlowCancelled('conversation was reset') from None
    except DeerFlowError as error:
        final_status = 'uncertain' if error.uncertain else ('cancelled' if isinstance(error, DeerFlowCancelled) else 'failed')
        raise
    finally:
        try:
            try:
                if prompt_stream is not None:
                    prompt_stream.close()
            finally:
                if started:
                    store.finish_turn(key, epoch, session_id, request_id, final_status)
        finally:
            with _registry_lock:
                active = _active.get(runtime_key)
                if active and active[0] is active_token:
                    _active.pop(runtime_key, None)
            identity_lock.release()


def reset_conversation(dkey, role=None, llm_config=None):
    """Advance the durable epoch immediately; no subprocess/network work here."""
    store, key, runtime_key, _ = _identity(dkey, role, dict(llm_config or {}))
    epoch = store.reset(key)
    with _registry_lock:
        active = _active.get(runtime_key)
        if active:
            active[1].set()
    return epoch
