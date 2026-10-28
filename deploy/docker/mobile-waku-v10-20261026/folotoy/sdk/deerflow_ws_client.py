"""Persistent Waku protocol 10 transport for the FoloToy text/voice provider.

This is a Waku client, not an ACP-over-WebSocket implementation. Only the
existing provider-facing API is translated. No submitted prompt is retried.
"""

import hashlib
import json
import os
import queue
import re
import sqlite3
import threading
import time
import uuid
from contextlib import closing
from urllib.parse import urlsplit, urlunsplit

from sdk.deerflow_client import (
    DeerFlowError, DeerFlowCancelled, DeerFlowTimeout, DeerFlowUncertain,
    _ProcessLock, positive_number,
)

NIL = str(uuid.UUID(int=0))


def normalize_ws_url(value):
    parsed = urlsplit(str(value or '').strip())
    if (parsed.scheme not in ('ws', 'wss') or not parsed.hostname
            or parsed.username or parsed.password or parsed.fragment or parsed.query
            or parsed.path not in ('', '/', '/v1')):
        raise DeerFlowError('ws_url must be a WS(S) server address with path /v1 and no credentials or query')
    return urlunsplit((parsed.scheme.lower(), parsed.netloc.lower(), '/v1', '', ''))


def _connect(*args, **kwargs):
    # Lazy import keeps the optional WS dependency out of legacy ACP startup.
    from websockets.sync.client import connect
    return connect(*args, **kwargs)


class _Pending:
    def __init__(self, generation):
        self.generation = generation
        self.done = threading.Event()
        self.result = None
        self.error = None


class _Session:
    def __init__(self, session_id, runtime_id, cursor=None, models=None,
                 model=None, unconfirmed=False):
        self.id, self.runtime = session_id, runtime_id
        self.cursor, self.models, self.model = cursor, models or [], model
        self.unconfirmed = bool(unconfirmed)
        self.connected = False
        self.revision = 0
        self.error = None
        self.generation = 0
        self.approval = None
        self.condition = threading.Condition(threading.RLock())


class _Turn:
    def __init__(self, session, generation, capacity):
        self.session, self.generation = session, generation
        self.id = str(uuid.uuid4())
        self.accepted = False
        self.events = queue.Queue(capacity)
        self.done = threading.Event()
        self.abandoned = threading.Event()
        self.watcher_done = threading.Event()
        self.error = None
        self.stop_error = None
        self.success = False


class DeerFlowWsClient:
    def __init__(self, config):
        self.config = dict(config)
        self.gateway = normalize_ws_url(config.get('ws_url'))
        self.pid = os.getpid()
        self.workspace = str(config.get('workspace') or '')
        self.binary = str(config.get('server_binary') or '/var/lib/deerflow/deerflow-acp')
        self.token_env = str(config.get('token_env') or 'WAKU_DAEMON_TOKEN')
        self.mode = str(config.get('ws_mode') or 'fullAccess')
        if self.mode not in ('ask', 'fullAccess'):
            raise DeerFlowError('ws_mode must be ask or fullAccess')
        self.approval = 'allow_always' if self.mode == 'fullAccess' else 'ask'
        if not self.workspace or not self.binary.startswith('/'):
            raise DeerFlowError('workspace and an absolute Linux server_binary are required')
        if config.get('approval', self.approval) != self.approval:
            raise DeerFlowError('Waku approval must match ws_mode')
        if not re.fullmatch(r'[A-Za-z_][A-Za-z0-9_]*', self.token_env):
            raise DeerFlowError('token_env must be an environment variable name')
        self.connect_timeout = positive_number(config, 'connect_timeout', 15)
        self.cancel_timeout = positive_number(config, 'cancel_timeout', 5)
        try:
            self.capacity = int(config.get('event_queue_size', 256))
        except (ValueError, TypeError):
            raise DeerFlowError('event_queue_size must be an integer') from None
        if not 1 <= self.capacity <= 65536:
            raise DeerFlowError('event_queue_size must be between 1 and 65536')
        self.state = os.path.abspath(os.path.expanduser(str(config.get('state_path') or 'data/deerflow-sessions.sqlite3')))
        os.makedirs(os.path.dirname(self.state), exist_ok=True)
        self._db_lock = threading.RLock()
        with closing(sqlite3.connect(self.state, timeout=5)) as db, db:
            db.execute('''CREATE TABLE IF NOT EXISTS deerflow_ws_sessions (
                endpoint TEXT NOT NULL, workspace TEXT NOT NULL,
                session_id TEXT NOT NULL, runtime_id TEXT NOT NULL,
                cursor TEXT, models TEXT NOT NULL, model TEXT,
                unconfirmed INTEGER NOT NULL DEFAULT 0,
                PRIMARY KEY(endpoint,workspace,session_id))''')
        lock_path = self.state + '.' + hashlib.sha256(self.gateway.encode()).hexdigest()[:16] + '.bridge.lock'
        self._process_lock = _ProcessLock(lock_path)
        self._lock = threading.RLock()
        self._start_lock = threading.RLock()
        self._socket = None
        self._generation = 0
        self._ready_generation = 0
        self._closed = False
        self._pending = {}
        self._sessions = {}
        self._turns = {}
        self._outgoing = None
        self._threads = []

    def _check_pid(self):
        if self.pid != os.getpid():
            raise DeerFlowError('a Waku client cannot be reused after multiprocessing fork')

    def _persist(self, session):
        with self._db_lock, closing(sqlite3.connect(self.state, timeout=5)) as db, db:
            db.execute('''INSERT OR REPLACE INTO deerflow_ws_sessions
                VALUES (?,?,?,?,?,?,?,?)''', (self.gateway, self.workspace, session.id,
                session.runtime, json.dumps(session.cursor) if session.cursor else None,
                json.dumps(session.models), session.model, int(session.unconfirmed)))

    def _load(self, session_id):
        with self._lock:
            session = self._sessions.get(session_id)
            if session is not None:
                return session
            with self._db_lock, closing(sqlite3.connect(self.state, timeout=5)) as db:
                row = db.execute('''SELECT runtime_id,cursor,models,model,unconfirmed
                    FROM deerflow_ws_sessions WHERE endpoint=? AND workspace=? AND session_id=?''',
                    (self.gateway, self.workspace, session_id)).fetchone()
            if row is None:
                raise DeerFlowError('no Waku session mapping exists for this conversation; explicitly reset it')
            try:
                uuid.UUID(session_id)
                uuid.UUID(row[0])
                session = _Session(session_id, row[0], json.loads(row[1]) if row[1] else None,
                                   json.loads(row[2]), row[3], row[4])
            except (ValueError, TypeError):
                raise DeerFlowError('invalid saved Waku session mapping') from None
            self._sessions[session_id] = session
            return session

    def ensure_connected(self, timeout=None):
        self._check_pid()
        deadline = time.monotonic() + min(timeout or self.connect_timeout, self.connect_timeout)
        if not self._start_lock.acquire(timeout=max(0, deadline - time.monotonic())):
            raise DeerFlowTimeout('timed out waiting for the Waku connection')
        try:
            with self._lock:
                if self._closed:
                    raise DeerFlowError('DeerFlow client is closed')
                if self._socket is not None and self._ready_generation == self._generation:
                    return self._generation
                old_socket, old_outgoing = self._socket, self._outgoing
                token = os.environ.get(self.token_env)
                if not token:
                    raise DeerFlowError('the configured Waku token environment variable is not set')
            if old_outgoing is not None:
                try:
                    old_outgoing.put_nowait(None)
                except queue.Full:
                    pass
            if old_socket is not None:
                old_socket.close()
            socket = None
            try:
                socket = _connect(self.gateway, open_timeout=max(0.001, deadline - time.monotonic()),
                                  close_timeout=min(2, self.connect_timeout),
                                  compression=None, max_size=4 * 1024 * 1024)
                # Hello contains a credential: never include frames in errors.
                socket.send(json.dumps({'type': 'hello', 'protocolVersion': 10,
                    'token': token, 'clientId': str(uuid.uuid4()), 'resumeFrom': []}))
                hello = json.loads(socket.recv(timeout=max(0.001, deadline - time.monotonic())))
                if hello.get('type') != 'hello' or hello.get('protocolVersion') != 10:
                    raise DeerFlowError('Waku authentication or protocol negotiation failed')
            except Exception:
                if socket is not None:
                    socket.close()
                raise DeerFlowError('could not authenticate a Waku protocol 10 connection') from None
            finally:
                token = None
            with self._lock:
                self._generation += 1
                generation = self._generation
                self._socket = socket
                self._ready_generation = generation
                self._outgoing = queue.Queue(128)
                outgoing = self._outgoing
            self._threads = [thread for thread in self._threads if thread.is_alive()]
            for target, arguments in ((self._write_loop, (socket, generation, outgoing)),
                                      (self._read_loop, (socket, generation))):
                thread = threading.Thread(target=target, args=arguments, daemon=True)
                self._threads.append(thread)
                thread.start()
            return generation
        finally:
            self._start_lock.release()

    def _fail(self, generation):
        with self._lock:
            if self._generation == generation:
                self._ready_generation = 0
            for request_id, pending in list(self._pending.items()):
                if pending.generation == generation:
                    pending.error = DeerFlowUncertain('Waku disconnected; submitted work was not replayed')
                    pending.done.set()
                    del self._pending[request_id]
            for turn in self._turns.values():
                if turn.generation == generation and not turn.done.is_set():
                    turn.error = DeerFlowUncertain('Waku disconnected; submitted work was not replayed')
                    turn.done.set()
            for session in self._sessions.values():
                with session.condition:
                    if session.generation == generation:
                        session.error = DeerFlowUncertain('Waku disconnected during session preparation')
                        session.condition.notify_all()

    def _write_loop(self, socket, generation, outgoing):
        try:
            while True:
                job = outgoing.get()
                if job is None:
                    return
                frame, deadline, done, errors = job
                try:
                    if time.monotonic() >= deadline:
                        raise TimeoutError()
                    socket.send(json.dumps(frame, ensure_ascii=False, separators=(',', ':')))
                except Exception:
                    errors.append(DeerFlowUncertain('Waku dispatch failed; the request was not replayed'))
                    self._fail(generation)
                    return
                finally:
                    done.set()
        finally:
            self._fail(generation)

    def _send(self, frame, generation, timeout, wait=True):
        deadline = time.monotonic() + timeout
        with self._lock:
            if self._closed or generation != self._ready_generation:
                raise DeerFlowUncertain('Waku is disconnected; the request was not replayed')
            outgoing = self._outgoing
        done, errors = threading.Event(), []
        try:
            outgoing.put((frame, deadline, done, errors), timeout=max(0, deadline - time.monotonic()))
        except queue.Full:
            raise DeerFlowTimeout('Waku outgoing queue is full', uncertain=True) from None
        if wait:
            if not done.wait(max(0, deadline - time.monotonic())):
                self._fail(generation)
                threading.Thread(target=self._socket.close, daemon=True).start()
                raise DeerFlowTimeout('Waku send timed out; dispatch is uncertain', uncertain=True)
            if errors:
                raise errors[0]

    def _rpc(self, command, generation, timeout, session=None):
        request_id = str(uuid.uuid4())
        pending = _Pending(generation)
        with self._lock:
            self._pending[request_id] = pending
        deadline = time.monotonic() + timeout
        try:
            self._send({'type': 'request', 'requestId': request_id,
                'sessionId': session.id if session else NIL,
                'runtimeId': session.runtime if session else NIL,
                'command': command}, generation, timeout)
            if not pending.done.wait(max(0, deadline - time.monotonic())):
                raise DeerFlowTimeout('Waku request timed out; no automatic replay was attempted', uncertain=True)
            if pending.error:
                raise pending.error
            return pending.result
        finally:
            with self._lock:
                self._pending.pop(request_id, None)

    def _notify(self, command, session, generation):
        self._send({'type': 'request', 'requestId': NIL, 'sessionId': session.id,
                    'runtimeId': session.runtime, 'command': command},
                   generation, self.cancel_timeout, wait=False)

    def _read_loop(self, socket, generation):
        try:
            while not self._closed and generation == self._ready_generation:
                message = json.loads(socket.recv())
                if not isinstance(message, dict):
                    raise ValueError()
                if message.get('type') == 'response':
                    with self._lock:
                        pending = self._pending.get(message.get('requestId'))
                    if pending is not None and pending.generation == generation:
                        outcome = message.get('outcome') or {}
                        if outcome.get('status') == 'ok':
                            pending.result = outcome.get('payload')
                        else:
                            pending.error = DeerFlowError('Waku rejected the request (remote details withheld)')
                        pending.done.set()
                elif message.get('type') == 'event':
                    self._event(message, generation)
        except Exception:
            pass  # No raw frames, prompts, tokens or remote error contents in logs.
        finally:
            self._fail(generation)

    def _event(self, message, generation):
        with self._lock:
            session = self._sessions.get(message.get('sessionId'))
            if (session is None or message.get('runtimeId') != session.runtime
                    or session.generation != generation):
                return
            turn = self._turns.get(session.id)
        event = message.get('event') or {}
        kind, payload = event.get('kind'), event.get('payload')
        with session.condition:
            if kind == 'connected':
                if not isinstance(payload, dict) or payload.get('provider') != 'deerFlow' or not payload.get('sessionId'):
                    session.error = DeerFlowError('Waku returned no valid DeerFlow resume cursor')
                else:
                    session.cursor, session.connected = payload, True
                    self._persist(session)
            elif kind == 'deerflowModelsChanged':
                if isinstance(payload, dict) and isinstance(payload.get('models'), list):
                    session.models = payload['models']
                    session.model = payload.get('model')
                    session.revision += 1
                    self._persist(session)
            elif kind in ('error', 'processExited'):
                session.error = DeerFlowError('DeerFlow reported a session error (remote details withheld)')
                if turn is not None and not turn.done.is_set():
                    turn.error = session.error
                    turn.done.set()
            session.condition.notify_all()
        if kind == 'permission':
            options = payload.get('options', []) if isinstance(payload, dict) else []
            if self.mode == 'fullAccess':
                selected = next((item for option_id in ('allow_always', 'allow_once')
                                 for item in options if item.get('allow') is True
                                 and item.get('id') == option_id), None)
            else:
                selected = next((item for item in options if item.get('allow') is False), None)
            if selected is not None and payload.get('requestId'):
                self._notify({'type': 'respond', 'requestId': payload['requestId'],
                              'optionId': selected['id']}, session, generation)
            else:
                if turn is not None:
                    turn.error = DeerFlowError('no suitable tool permission option was provided by DeerFlow')
                self._notify({'type': 'cancel'}, session, generation)
        elif kind == 'userInputRequested':
            if turn is not None:
                turn.error = DeerFlowError('DeerFlow requested interactive input unavailable in voice mode')
            self._notify({'type': 'cancel'}, session, generation)
        if turn is None or turn.generation != generation or turn.done.is_set():
            return
        if kind == 'promptSubmitted':
            turn.accepted = isinstance(payload, dict) and payload.get('turnId') == turn.id
        elif kind == 'textDelta' and turn.accepted and isinstance(payload, str):
            try:
                turn.events.put_nowait({'sessionUpdate': 'agent_message_chunk',
                                       'content': {'type': 'text', 'text': payload}})
            except queue.Full:
                turn.error = DeerFlowUncertain('Waku event queue overflowed; the partial reply was stopped')
        elif kind == 'turnFinished' and turn.accepted:
            turn.success = isinstance(payload, dict) and payload.get('success') is True
            session.unconfirmed = False
            self._persist(session)
            turn.done.set()

    def _wait_session(self, session, predicate, deadline):
        with session.condition:
            while True:
                if session.error:
                    raise session.error
                if predicate():
                    return
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise DeerFlowTimeout('timed out waiting for confirmed Waku session state', uncertain=True)
                session.condition.wait(remaining)

    def _options(self, session):
        return {'sessionId': session.id, 'configOptions': [
            {'id': 'model', 'currentValue': session.model,
             'options': [{'value': item['id'], 'name': item.get('name', item['id'])}
                         for item in session.models if isinstance(item, dict) and item.get('id')]},
            {'id': 'approval', 'currentValue': session.approval,
             'options': [{'value': self.approval, 'name': self.approval}]},
        ]}

    def session_info(self, session_id, generation):
        if not session_id:
            return None
        session = self._sessions.get(session_id)
        if session is None or session.generation != generation or session.approval != self.approval or session.error:
            return None
        return self._options(session)

    def remember_session(self, session_id, generation, response):
        session = self._load(session_id)
        if generation != self._ready_generation or session.generation != generation:
            raise DeerFlowUncertain('Waku connection changed while attaching the session')

    def _wire_options(self, model=None):
        return {'mode': self.mode, 'model': model, 'reasoningEffort': None,
                'thinkingMode': None, 'serviceTier': None, 'contextWindow': None}

    def _apply_model(self, session, model, generation, deadline):
        revision = session.revision
        result = self._rpc({'type': 'applyOptions', 'options': self._wire_options(model)},
                           generation, max(0.001, deadline - time.monotonic()), session)
        if not isinstance(result, dict) or result.get('applied') is not True:
            raise DeerFlowError('Waku did not accept the model options')
        self._wait_session(session, lambda: session.revision > revision and (model is None or session.model == model), deadline)

    def request(self, method, params, *, generation=None, timeout=None):
        deadline = time.monotonic() + (timeout or self.connect_timeout)
        generation = generation or self.ensure_connected(timeout=max(0.001, deadline - time.monotonic()))
        if generation != self._ready_generation:
            raise DeerFlowUncertain('Waku transport changed; the request was not replayed')
        if method == 'session/new':
            session = _Session(str(uuid.uuid4()), str(uuid.uuid4()))
            with self._lock:
                self._sessions[session.id] = session
            self._persist(session)
        elif method in ('session/resume', 'session/set_config_option'):
            session = self._load(params.get('sessionId'))
        else:
            raise DeerFlowError('unsupported Waku provider API operation')
        if method in ('session/new', 'session/resume'):
            session.generation, session.error, session.approval = generation, None, None
            attached = None
            if method == 'session/resume':
                attached = self._rpc({'type': 'attachSession'}, generation,
                                     max(0.001, deadline - time.monotonic()), session)
                if not isinstance(attached, dict) or attached.get('type') != 'sessionRuntime':
                    raise DeerFlowError('Waku returned an invalid attach response')
            runtime = attached.get('runtimeId') if attached else None
            if runtime:
                try:
                    uuid.UUID(runtime)
                except (ValueError, TypeError):
                    raise DeerFlowError('Waku returned an invalid runtime identity') from None
                session.runtime = runtime
                self._persist(session)
                # Attaching must never replace a live runtime or replay a turn.
                if session.unconfirmed:
                    raise DeerFlowUncertain('the previous Waku turn has no confirmed end; explicitly reset before submitting another')
                self._apply_model(session, session.model, generation, deadline)
            else:
                if method == 'session/resume' and not session.cursor:
                    raise DeerFlowUncertain('the saved Waku runtime has no native resume cursor; explicitly reset')
                session.runtime = str(uuid.uuid4())
                session.connected, session.revision = False, 0
                self._persist(session)
                options = {**self._wire_options(self.config.get('model') or session.model),
                    'provider': 'deerFlow', 'binary': self.binary, 'cwd': self.workspace,
                    'agentPreset': None, 'computerUseEnabled': False,
                    'providerCursor': session.cursor}
                result = self._rpc({'type': 'start', 'options': options}, generation,
                                   max(0.001, deadline - time.monotonic()), session)
                if not isinstance(result, dict) or result.get('type') != 'started':
                    raise DeerFlowError('Waku did not start the DeerFlow runtime')
                self._wait_session(session, lambda: session.connected and session.revision > 0, deadline)
                session.unconfirmed = False
                self._persist(session)
            result = self._rpc({'type': 'setToolApproval', 'mode': self.approval}, generation,
                               max(0.001, deadline - time.monotonic()), session)
            if not isinstance(result, dict) or result.get('mode') != self.approval:
                raise DeerFlowError('Waku did not confirm the requested tool approval policy')
            session.approval = self.approval
            return self._options(session)
        if session.generation != generation:
            raise DeerFlowUncertain('the Waku session is not attached to this connection')
        config_id, value = params.get('configId'), params.get('value')
        if config_id == 'model':
            if value not in [item.get('id') for item in session.models]:
                raise DeerFlowError('DeerFlow does not offer the requested model')
            self._apply_model(session, value, generation, deadline)
        elif config_id != 'approval' or value != self.approval or session.approval != self.approval:
            raise DeerFlowError('the Waku voice adapter requires its confirmed configured approval policy')
        return self._options(session)

    def _watch_turn(self, turn, cancelled, deadline, cancel_timeout):
        try:
            while True:
                error = turn.error
                if error is None and (cancelled.is_set() or turn.abandoned.is_set()):
                    error = DeerFlowCancelled('DeerFlow turn cancelled')
                if error is None and time.monotonic() >= deadline:
                    error = DeerFlowTimeout('DeerFlow turn deadline exceeded')
                if error is not None:
                    turn.stop_error = error
                    cancel_deadline = time.monotonic() + cancel_timeout
                    try:
                        self._rpc({'type': 'cancel'}, turn.generation, cancel_timeout, turn.session)
                        confirmed = turn.done.wait(max(0, cancel_deadline - time.monotonic()))
                    except DeerFlowError:
                        confirmed = False
                    error.uncertain = error.uncertain or not confirmed
                    return
                if turn.done.wait(0.025):
                    if turn.error or cancelled.is_set() or turn.abandoned.is_set():
                        continue
                    return
        finally:
            turn.watcher_done.set()

    def stream_prompt(self, session_id, text, *, generation, cancel_event,
                      turn_timeout=90, cancel_timeout=None):
        session = self._load(session_id)
        if cancel_event.is_set():
            raise DeerFlowCancelled('turn cancelled before dispatch')
        if session.generation != generation or session.approval != self.approval:
            raise DeerFlowError('Waku session approval has not been confirmed')
        if session.error:
            raise session.error
        deadline = time.monotonic() + turn_timeout
        turn = _Turn(session, generation, self.capacity)
        with self._lock:
            previous = self._turns.get(session_id)
            if session.unconfirmed or (previous and not previous.done.is_set()):
                raise DeerFlowUncertain('the previous Waku turn has no confirmed end; no prompt was replayed')
            self._turns[session_id] = turn
            session.unconfirmed = True
            self._persist(session)
        try:
            self._rpc({'type': 'prompt', 'prompt': text, 'turnId': turn.id,
                       'messageId': str(uuid.uuid4())}, generation,
                      min(turn_timeout, self.connect_timeout), session)
        except BaseException:
            turn.error = DeerFlowUncertain('Waku prompt dispatch was not confirmed; no replay was attempted')
            raise
        cancel_timeout = cancel_timeout or self.cancel_timeout
        threading.Thread(target=self._watch_turn,
            args=(turn, cancel_event, deadline, cancel_timeout), daemon=True).start()
        terminal = False
        try:
            while True:
                if turn.stop_error is not None or turn.error is not None or cancel_event.is_set():
                    turn.watcher_done.wait(cancel_timeout + 0.1)
                    terminal = True
                    raise turn.stop_error or turn.error or DeerFlowCancelled('DeerFlow turn cancelled')
                if turn.done.is_set() and turn.events.empty():
                    terminal = True
                    if not turn.success:
                        raise DeerFlowError('DeerFlow turn did not complete successfully')
                    yield {'_completion': {'stopReason': 'end_turn'}}
                    return
                try:
                    yield turn.events.get(timeout=0.05)
                except queue.Empty:
                    pass
        finally:
            if not terminal:
                turn.abandoned.set()
                turn.watcher_done.wait(cancel_timeout + 0.1)
            with self._lock:
                if turn.done.is_set() and self._turns.get(session_id) is turn:
                    del self._turns[session_id]

    def close(self):
        self._check_pid()
        with self._start_lock:
            if self._closed:
                return
            self._closed = True
            self._fail(self._generation)
            if self._outgoing is not None:
                try:
                    self._outgoing.put_nowait(None)
                except queue.Full:
                    pass
            if self._socket is not None:
                self._socket.close()
                self._socket = None
            for thread in self._threads:
                if thread is not threading.current_thread():
                    thread.join(timeout=2)
            self._process_lock.close()

    def close_session(self, session_id):
        """Explicit maintenance cleanup; ordinary disconnect preserves actors."""
        session = self._load(session_id)
        generation = self.ensure_connected()
        attached = self._rpc({'type': 'attachSession'}, generation, self.connect_timeout, session)
        if not isinstance(attached, dict) or attached.get('type') != 'sessionRuntime':
            raise DeerFlowError('Waku returned an invalid attach response during cleanup')
        runtime = attached.get('runtimeId') if isinstance(attached, dict) else None
        if runtime:
            if runtime != session.runtime:
                raise DeerFlowError('the Waku runtime changed; refusing to close an unowned actor')
            self._rpc({'type': 'closeSession'}, generation, self.connect_timeout, session)
