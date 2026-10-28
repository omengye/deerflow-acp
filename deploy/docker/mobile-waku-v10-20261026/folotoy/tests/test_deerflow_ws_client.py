"""Offline Waku transport checks; no credentials, network or model requests."""

import concurrent.futures
import json
import os
from pathlib import Path
import queue
import sqlite3
import sys
import tempfile
import threading
import types
import time
import unittest
from contextlib import closing
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
# Use the deployed unchanged companion SDK; only the transport under test is overlaid.
package = types.ModuleType('sdk')
package.__path__ = [str(ROOT / 'sdk'), os.environ.get('FOLO_BASE_SDK', '/opt/folotoy/sdk')]
sys.modules['sdk'] = package
from sdk import deerflow_ws_client as ws


class FakeSocket:
    def __init__(self, server):
        self.server = server
        self.messages = queue.Queue()
        self.closed = False

    def send(self, text):
        self.server.receive(self, json.loads(text))

    def recv(self, timeout=None):
        item = self.messages.get(timeout=timeout)
        if item is None:
            raise OSError('offline connection closed')
        return json.dumps(item)

    def close(self):
        if not self.closed:
            self.closed = True
            self.messages.put(None)


class FakeServer:
    def __init__(self):
        self.frames = []
        self.sockets = []
        self.runtimes = {}
        self.turns = {}
        self.lock = threading.RLock()
        self.reject_approval = False
        self.reject_hello = False
        self.hello_protocol = 10

    def connect(self, *args, **kwargs):
        socket = FakeSocket(self)
        self.sockets.append(socket)
        return socket

    def response(self, socket, frame, payload=None, error=False):
        if frame.get('requestId') == ws.NIL:
            return
        socket.messages.put({'type': 'response', 'requestId': frame['requestId'],
            'outcome': {'status': 'error', 'error': {'message': 'PRIVATE DETAILS'}} if error else
                       {'status': 'ok', 'payload': payload or {'type': 'ack'}}})

    def event(self, socket, sid, kind, payload=None, runtime=None):
        runtime = runtime or self.runtimes[sid]['runtime']
        socket.messages.put({'type': 'event', 'sessionId': sid, 'runtimeId': runtime,
            'epoch': str(ws.uuid.uuid4()), 'sequence': 1, 'replayable': True,
            'event': {'kind': kind, 'payload': payload}})

    def models(self, socket, sid):
        self.event(socket, sid, 'deerflowModelsChanged', {'models': [
            {'id': 'primary', 'name': 'Primary'}, {'id': 'alternate', 'name': 'Alternate'}],
            'model': self.runtimes[sid]['model']})

    def finish(self, socket, sid, success=True):
        self.event(socket, sid, 'turnFinished', {'success': success, 'summary': None})

    def receive(self, socket, frame):
        with self.lock:
            self.frames.append(frame)
            if frame['type'] == 'hello':
                socket.messages.put({'type': 'rejected'} if self.reject_hello else
                                    {'type': 'hello', 'protocolVersion': self.hello_protocol})
                return
            command = frame['command']
            kind, sid = command['type'], frame['sessionId']
            if kind == 'attachSession':
                active = self.runtimes.get(sid)
                self.response(socket, frame, {'type': 'sessionRuntime',
                                             'runtimeId': active['runtime'] if active else None})
            elif kind == 'start':
                options = command['options']
                cursor = options.get('providerCursor') or {'provider': 'deerFlow', 'sessionId': 'native-' + sid}
                self.runtimes[sid] = {'runtime': frame['runtimeId'], 'cursor': cursor,
                                      'model': options.get('model') or 'primary'}
                self.event(socket, sid, 'connected', cursor)
                self.models(socket, sid)
                self.response(socket, frame, {'type': 'started', 'supportsSteer': False})
            elif kind == 'applyOptions':
                if command['options']['model']:
                    self.runtimes[sid]['model'] = command['options']['model']
                self.models(socket, sid)
                self.response(socket, frame, {'type': 'optionsApplied', 'applied': True})
            elif kind == 'setToolApproval':
                self.response(socket, frame, {'type': 'toolApproval', 'mode': command['mode']}, self.reject_approval)
            elif kind == 'prompt':
                self.turns[sid] = command['turnId']
                self.event(socket, sid, 'promptSubmitted', {'turnId': command['turnId']})
                self.event(socket, sid, 'reasoningDelta', 'PRIVATE REASONING')
                self.event(socket, sid, 'textDelta', 'reply:' + command['prompt'])
                if command['prompt'] == '<permission>':
                    self.event(socket, sid, 'permission', {'requestId': 'permission-1', 'options': [
                        {'id': 'allow_once', 'allow': True}, {'id': 'reject_once', 'allow': False}]})
                elif command['prompt'] == '<input>':
                    self.event(socket, sid, 'userInputRequested', {'requestId': 'input-1'})
                elif command['prompt'] != '<wait>':
                    self.finish(socket, sid)
                self.response(socket, frame)
            elif kind == 'respond':
                self.finish(socket, sid)
                self.response(socket, frame)
            elif kind == 'cancel':
                self.finish(socket, sid, False)
                self.response(socket, frame)
            elif kind == 'closeSession':
                self.runtimes.pop(sid, None)
                self.response(socket, frame)
            else:
                self.response(socket, frame, error=True)

    def commands(self, kind):
        return [frame for frame in self.frames if frame.get('command', {}).get('type') == kind]


class WsClientTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.config = {'transport': 'ws', 'ws_url': 'ws://192.0.2.1:18787/v1',
            'token_env': 'FOLO_OFFLINE_WS_TOKEN', 'workspace': '/workspaces/voice',
            'state_path': str(Path(self.temp.name) / 'sessions.sqlite3'),
            'connect_timeout': 2, 'cancel_timeout': 0.3}
        self.server = FakeServer()
        self.environment = patch.dict(os.environ, {'FOLO_OFFLINE_WS_TOKEN': 'offline-test-value'})
        self.environment.start()
        self.connection = patch.object(ws, '_connect', side_effect=self.server.connect)
        self.connection.start()
        self.clients = []
        self.client = self.make_client()

    def make_client(self):
        client = ws.DeerFlowWsClient(self.config)
        self.clients.append(client)
        return client

    def tearDown(self):
        for client in self.clients:
            client.close()
        self.connection.stop()
        self.environment.stop()
        self.temp.cleanup()

    def session(self, client=None):
        client = client or self.client
        generation = client.ensure_connected()
        result = client.request('session/new', {'cwd': '/workspaces/voice'}, generation=generation)
        return result['sessionId'], generation, result

    def stream(self, sid, generation, text='hello', client=None, cancel_event=None, **kwargs):
        return (client or self.client).stream_prompt(sid, text, generation=generation,
            cancel_event=cancel_event or threading.Event(), **kwargs)

    def wait_for(self, predicate):
        deadline = time.monotonic() + 2
        while time.monotonic() < deadline:
            if predicate():
                return
            time.sleep(0.005)
        self.fail('offline observation timed out')

    def test_protocol_start_policy_and_stream_conversion(self):
        sid, generation, info = self.session()
        chunks = list(self.stream(sid, generation))
        hello = self.server.frames[0]
        self.assertEqual(10, hello['protocolVersion'])
        self.assertEqual([], hello['resumeFrom'])
        options = self.server.commands('start')[0]['command']['options']
        self.assertEqual('deerFlow', options['provider'])
        self.assertEqual('fullAccess', options['mode'])
        self.assertFalse(options['computerUseEnabled'])
        self.assertEqual('/var/lib/deerflow/deerflow-acp', options['binary'])
        self.assertEqual(['model', 'approval'], [item['id'] for item in info['configOptions']])
        self.assertEqual('allow_always', info['configOptions'][1]['currentValue'])
        self.assertEqual('allow_always', self.server.commands('setToolApproval')[0]['command']['mode'])
        self.assertEqual('reply:hello', chunks[0]['content']['text'])
        self.assertNotIn('PRIVATE', json.dumps(chunks))
        self.assertEqual({'stopReason': 'end_turn'}, chunks[-1]['_completion'])

    def test_v9_server_is_rejected_before_any_provider_command(self):
        self.server.hello_protocol = 9
        with self.assertRaises(ws.DeerFlowError) as failure:
            self.client.ensure_connected()
        self.assertIn('protocol 10', str(failure.exception))
        self.assertEqual(1, len(self.server.frames))
        self.assertEqual('hello', self.server.frames[0]['type'])

    def test_reconnect_does_not_replay_history_prompts_or_permissions(self):
        sid, generation, _ = self.session()
        list(self.stream(sid, generation))
        previous = len(self.server.frames)
        self.client.close()
        restored = self.make_client()
        generation = restored.ensure_connected()
        restored.request('session/resume', {'sessionId': sid}, generation=generation)
        new_commands = [frame['command']['type'] for frame in self.server.frames[previous:]
                        if frame.get('type') == 'request']
        self.assertEqual(['attachSession', 'applyOptions', 'setToolApproval'], new_commands)
        for kind in ('replayEvents', 'start', 'prompt', 'respond', 'respondUserInput'):
            self.assertNotIn(kind, new_commands)

    def test_model_selection_requires_authoritative_confirmation(self):
        sid, generation, _ = self.session()
        info = self.client.request('session/set_config_option',
            {'sessionId': sid, 'configId': 'model', 'value': 'alternate'}, generation=generation)
        self.assertEqual('alternate', info['configOptions'][0]['currentValue'])
        self.assertEqual('fullAccess', self.server.commands('applyOptions')[0]['command']['options']['mode'])
        with self.assertRaises(ws.DeerFlowError):
            self.client.request('session/set_config_option',
                {'sessionId': sid, 'configId': 'model', 'value': 'unknown'}, generation=generation)
        self.assertEqual(1, len(self.server.commands('applyOptions')))

    def test_reconnect_attaches_existing_runtime_without_restart(self):
        sid, generation, _ = self.session()
        list(self.stream(sid, generation))
        old_runtime = self.server.runtimes[sid]['runtime']
        self.client.close()
        restored = self.make_client()
        generation = restored.ensure_connected()
        restored.request('session/resume', {'sessionId': sid}, generation=generation)
        chunks = list(self.stream(sid, generation, 'second', restored))
        self.assertEqual(1, len(self.server.commands('start')))
        self.assertEqual(old_runtime, self.server.runtimes[sid]['runtime'])
        self.assertEqual(2, len(self.server.commands('prompt')))
        self.assertEqual('reply:second', chunks[0]['content']['text'])

    def test_lost_runtime_recovers_native_cursor_without_replaying_prompt(self):
        sid, generation, _ = self.session()
        list(self.stream(sid, generation))
        cursor = self.server.runtimes.pop(sid)['cursor']
        self.client.close()
        restored = self.make_client()
        generation = restored.ensure_connected()
        restored.request('session/resume', {'sessionId': sid}, generation=generation)
        self.assertEqual(cursor, self.server.commands('start')[1]['command']['options']['providerCursor'])
        self.assertEqual(1, len(self.server.commands('prompt')))
        self.assertEqual(cursor, self.server.runtimes[sid]['cursor'])

    def test_multi_session_concurrency_uses_one_connection(self):
        first, generation, _ = self.session()
        second, _, _ = self.session()
        with concurrent.futures.ThreadPoolExecutor(2) as pool:
            a = pool.submit(lambda: list(self.stream(first, generation, 'first')))
            b = pool.submit(lambda: list(self.stream(second, generation, 'second')))
            self.assertEqual('reply:first', a.result()[0]['content']['text'])
            self.assertEqual('reply:second', b.result()[0]['content']['text'])
        self.assertEqual(1, len(self.server.sockets))

    def test_full_access_uses_actual_allow_once_option(self):
        sid, generation, _ = self.session()
        chunks = list(self.stream(sid, generation, '<permission>'))
        self.assertTrue(chunks[-1]['_completion'])
        response = self.server.commands('respond')[0]['command']
        self.assertEqual('allow_once', response['optionId'])

    def test_full_access_prefers_actual_allow_always_option(self):
        sid, generation, _ = self.session()
        stream = self.stream(sid, generation, '<wait>')
        next(stream)
        self.server.event(self.server.sockets[0], sid, 'permission', {
            'requestId': 'allow-options', 'options': [
                {'id': 'allow_once', 'allow': True}, {'id': 'allow_always', 'allow': True},
                {'id': 'reject_once', 'allow': False}]})
        self.assertTrue(list(stream)[-1]['_completion'])
        self.assertEqual('allow_always', self.server.commands('respond')[0]['command']['optionId'])

    def test_explicit_ask_mode_rejects_tool_permission(self):
        self.client.close()
        self.config.update(ws_mode='ask', approval='ask')
        self.client = self.make_client()
        sid, generation, info = self.session()
        self.assertEqual('ask', self.server.commands('start')[0]['command']['options']['mode'])
        self.assertEqual('ask', info['configOptions'][1]['currentValue'])
        self.assertEqual('ask', self.server.commands('setToolApproval')[0]['command']['mode'])
        self.assertTrue(list(self.stream(sid, generation, '<permission>'))[-1]['_completion'])
        self.assertEqual('reject_once', self.server.commands('respond')[0]['command']['optionId'])

    def test_interactive_input_cancels_instead_of_hanging(self):
        sid, generation, _ = self.session()
        with self.assertRaises(ws.DeerFlowError):
            list(self.stream(sid, generation, '<input>'))
        self.assertTrue(self.server.commands('cancel'))

    def test_cancel_progresses_while_consumer_is_paused(self):
        sid, generation, _ = self.session()
        cancelled = threading.Event()
        stream = self.stream(sid, generation, '<wait>', cancel_event=cancelled)
        self.assertEqual('reply:<wait>', next(stream)['content']['text'])
        cancelled.set()
        self.wait_for(lambda: self.server.commands('cancel'))
        with self.assertRaises(ws.DeerFlowCancelled):
            next(stream)
        self.assertTrue(list(self.stream(sid, generation))[-1]['_completion'])

    def test_turn_timeout_sends_cancel(self):
        sid, generation, _ = self.session()
        with self.assertRaises(ws.DeerFlowTimeout):
            list(self.stream(sid, generation, '<wait>', turn_timeout=0.1))
        self.assertEqual(1, len(self.server.commands('cancel')))

    def test_disconnect_never_replays_unconfirmed_prompt(self):
        sid, generation, _ = self.session()
        stream = self.stream(sid, generation, '<wait>')
        next(stream)
        self.server.sockets[0].close()
        with self.assertRaises(ws.DeerFlowUncertain):
            next(stream)
        self.client.close()
        restored = self.make_client()
        generation = restored.ensure_connected()
        with self.assertRaises(ws.DeerFlowUncertain):
            restored.request('session/resume', {'sessionId': sid}, generation=generation)
        self.assertEqual(1, len(self.server.commands('prompt')))
        self.assertEqual(1, len(self.server.commands('start')))

    def test_same_client_reconnects_without_replacing_actor(self):
        sid, generation, _ = self.session()
        list(self.stream(sid, generation))
        self.server.sockets[0].close()
        self.wait_for(lambda: self.client._ready_generation == 0)
        next_generation = self.client.ensure_connected()
        self.assertGreater(next_generation, generation)
        self.client.request('session/resume', {'sessionId': sid}, generation=next_generation)
        self.assertTrue(list(self.stream(sid, next_generation))[-1]['_completion'])
        self.assertEqual(1, len(self.server.commands('start')))

    def test_consumer_close_cancels_without_closing_other_sessions(self):
        sid, generation, _ = self.session()
        other, _, _ = self.session()
        stream = self.stream(sid, generation, '<wait>')
        next(stream)
        stream.close()
        self.assertEqual(1, len(self.server.commands('cancel')))
        self.assertTrue(list(self.stream(other, generation))[-1]['_completion'])
        self.assertEqual(1, len(self.server.sockets))

    def test_slow_consumer_overflow_cancels_partial_reply(self):
        self.client.capacity = 1
        sid, generation, _ = self.session()
        stream = self.stream(sid, generation, '<wait>')
        next(stream)
        for text in ('part-1', 'part-2', 'part-3'):
            self.server.event(self.server.sockets[0], sid, 'textDelta', text)
        self.wait_for(lambda: self.server.commands('cancel'))
        with self.assertRaises(ws.DeerFlowUncertain):
            next(stream)

    def test_full_access_does_not_fabricate_an_allow_option(self):
        sid, generation, _ = self.session()
        stream = self.stream(sid, generation, '<wait>')
        next(stream)
        self.server.event(self.server.sockets[0], sid, 'permission', {
            'requestId': 'no-allow', 'options': [{'id': 'reject_once', 'allow': False}]})
        with self.assertRaises(ws.DeerFlowError):
            next(stream)
        self.assertTrue(self.server.commands('cancel'))
        self.assertFalse(self.server.commands('respond'))

    def test_unknown_runtime_and_unrelated_text_are_not_spoken(self):
        sid, generation, _ = self.session()
        self.server.event(self.server.sockets[0], sid, 'textDelta', 'OLD AUDIO')
        self.server.event(self.server.sockets[0], sid, 'textDelta', 'OTHER RUNTIME', runtime=str(ws.uuid.uuid4()))
        chunks = list(self.stream(sid, generation))
        self.assertEqual('reply:hello', ''.join(item.get('content', {}).get('text', '') for item in chunks))

    def test_approval_rejection_fails_before_prompt_and_redacts_details(self):
        self.server.reject_approval = True
        with self.assertRaises(ws.DeerFlowError) as error:
            self.session()
        self.assertNotIn('PRIVATE', str(error.exception))
        self.assertFalse(self.server.commands('prompt'))

    def test_state_persists_cursor_without_token(self):
        sid, generation, _ = self.session()
        list(self.stream(sid, generation))
        with closing(sqlite3.connect(self.config['state_path'])) as db:
            row = db.execute('SELECT session_id,runtime_id,cursor,unconfirmed FROM deerflow_ws_sessions').fetchone()
        self.assertEqual(sid, row[0])
        self.assertEqual(self.server.runtimes[sid]['cursor'], json.loads(row[2]))
        self.assertEqual(0, row[3])
        self.assertNotIn('offline-test-value', Path(self.config['state_path']).read_bytes().decode('utf-8', 'ignore'))

    def test_explicit_cleanup_releases_only_requested_actor(self):
        first, _, _ = self.session()
        second, _, _ = self.session()
        self.client.close_session(first)
        self.assertNotIn(first, self.server.runtimes)
        self.assertIn(second, self.server.runtimes)

    def test_cleanup_does_not_close_replaced_runtime(self):
        sid, _, _ = self.session()
        self.server.runtimes[sid]['runtime'] = str(ws.uuid.uuid4())
        with self.assertRaises(ws.DeerFlowError):
            self.client.close_session(sid)
        self.assertFalse(self.server.commands('closeSession'))

    def test_invalid_address_or_policy_is_rejected(self):
        for address in ('http://host/v1', 'ws://user:secret@host/v1', 'ws://host/other', 'ws://host/v1?token=x'):
            with self.assertRaises(ws.DeerFlowError):
                ws.normalize_ws_url(address)
        with self.assertRaises(ws.DeerFlowError):
            ws.DeerFlowWsClient({**self.config, 'approval': 'read_only'})
        with self.assertRaises(ws.DeerFlowError):
            ws.DeerFlowWsClient({**self.config, 'ws_mode': 'unknown'})
        with self.assertRaises(ws.DeerFlowError):
            ws.DeerFlowWsClient({**self.config, 'ws_mode': 'ask', 'approval': 'allow_always'})

    def test_failed_authentication_redacts_token(self):
        self.server.reject_hello = True
        with self.assertRaises(ws.DeerFlowError) as error:
            self.client.ensure_connected()
        self.assertNotIn('offline-test-value', str(error.exception))


if __name__ == '__main__':
    unittest.main()
