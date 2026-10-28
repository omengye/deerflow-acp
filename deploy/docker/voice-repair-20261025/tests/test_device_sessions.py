"""Offline login restart regression, authentication-file checks and UDP acceptance."""
from concurrent.futures import ThreadPoolExecutor
import importlib.util
import json
import os
from pathlib import Path
import stat
import sys
import tempfile
import types
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
CORE = ROOT / 'folotoy-source/core'
BASE = Path(os.environ.get('FOLOTOY_BASE_SOURCE', 'D:/Tools/folo_server_src/src/folotoy-decompiled/src'))


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


core = types.ModuleType('core'); core.__path__ = [str(CORE), str(BASE / 'core')]
sys.modules['core'] = core
STORE = load('core.device_session_store', CORE / 'device_session_store.py')
CONTROL = load('core.conversation_control', BASE / 'core/conversation_control.py')


class StoreTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.path = Path(self.directory.name) / 'private/device-sessions.json'
        self.store = STORE.DeviceSessionStore(self.path, 'folotoy')
        self.records = [{'device': '48ca439d3b78', 'token': 'a' * 32, 'role': '1', 'msg_id': 7}]

    def private_write(self, data):
        self.path.parent.mkdir(mode=0o700)
        self.path.write_bytes(data)
        self.path.chmod(0o600)

    def test_absent_is_empty(self):
        self.assertEqual([], self.store.load())

    def test_round_trip_and_private_modes(self):
        self.store.save(self.records)
        self.assertEqual(self.records, self.store.load())
        if os.name == 'posix':
            self.assertEqual(0o700, stat.S_IMODE(self.path.parent.stat().st_mode))
            self.assertEqual(0o600, stat.S_IMODE(self.path.stat().st_mode))

    def test_atomic_failure_keeps_original(self):
        self.store.save(self.records)
        original = self.path.read_bytes()
        with patch.object(STORE.os, 'replace', side_effect=OSError('fixture')):
            with self.assertRaises(OSError):
                self.store.save([])
        self.assertEqual(original, self.path.read_bytes())
        self.assertEqual([self.path], list(self.path.parent.iterdir()))

    def test_malformed_json(self):
        self.private_write(b'{broken')
        with self.assertRaises(STORE.DeviceSessionStateError): self.store.load()

    def test_duplicate_json_key(self):
        self.private_write(b'{"version":1,"version":1,"namespace":"folotoy","devices":[]}')
        with self.assertRaises(STORE.DeviceSessionStateError): self.store.load()

    def test_unknown_fields_and_namespace_rejected(self):
        for changes in ({'extra': 'no'}, {'namespace': 'another'}, {'version': True}):
            with self.subTest(fields=list(changes)):
                value = {'version': 1, 'namespace': 'folotoy', 'devices': self.records, **changes}
                with self.assertRaises(STORE.DeviceSessionStateError): self.store.validate(value)

    def test_entry_type_length_and_duplicate_guards(self):
        changes = [{'token': 'b' * 31}, {'token': 'B' * 32}, {'device': 'a/b'},
                   {'device': 'x' * 65}, {'role': 1}, {'role': '0'}, {'msg_id': True},
                   {'msg_id': 1 << 53}, {'history': []}]
        for change in changes:
            with self.subTest(fields=list(change)):
                records = [{**self.records[0], **change}]
                with self.assertRaises(STORE.DeviceSessionStateError): self.store.save(records)
        for records in (self.records * 2,
                        self.records + [{**self.records[0], 'device': 'second'}],
                        self.records * 257):
            with self.assertRaises(STORE.DeviceSessionStateError): self.store.save(records)

    def test_oversized_file_rejected(self):
        self.private_write(b' ' * (self.store.MAX_BYTES + 1))
        with self.assertRaises(STORE.DeviceSessionStateError): self.store.load()

    @unittest.skipUnless(os.name == 'posix', 'POSIX private file permissions')
    def test_world_readable_file_rejected(self):
        self.store.save(self.records); self.path.chmod(0o644)
        with self.assertRaises(STORE.DeviceSessionStateError): self.store.load()

    @unittest.skipUnless(os.name == 'posix', 'POSIX private directory permissions')
    def test_shared_parent_rejected(self):
        self.store.save(self.records); self.path.parent.chmod(0o755)
        with self.assertRaises(STORE.DeviceSessionStateError): self.store.load()

    @unittest.skipUnless(os.name == 'posix', 'POSIX symlinks')
    def test_symlink_rejected(self):
        self.store.save(self.records)
        other = self.path.with_name('other.json'); self.path.rename(other); self.path.symlink_to(other)
        with self.assertRaises(STORE.DeviceSessionStateError): self.store.load()

    @unittest.skipUnless(os.name == 'posix', 'POSIX link metadata')
    def test_hardlink_rejected(self):
        self.store.save(self.records); os.link(self.path, self.path.with_name('other.json'))
        with self.assertRaises(STORE.DeviceSessionStateError): self.store.load()


def app_modules():
    mqtt = types.ModuleType('paho.mqtt.client')
    mqtt.Client = lambda **kwargs: types.SimpleNamespace()
    mqtt.CallbackAPIVersion = types.SimpleNamespace(VERSION2=2)
    paho = types.ModuleType('paho'); mqtt_package = types.ModuleType('paho.mqtt')
    paho.mqtt = mqtt_package; mqtt_package.client = mqtt
    config = types.ModuleType('config'); config.Config = object
    sdk = types.ModuleType('sdk'); sdk.__path__ = []
    util = types.ModuleType('util'); util.__path__ = []
    modules = {'paho': paho, 'paho.mqtt': mqtt_package, 'paho.mqtt.client': mqtt,
               'config': config, 'sdk': sdk, 'util': util}
    for name, class_name in (('llm', 'LlmSdk'), ('stt', 'SttSdk'), ('tts', 'TtsSdk')):
        module = types.ModuleType('sdk.' + name); setattr(module, class_name, object)
        modules['sdk.' + name] = module
    for name in ('topic_util', 'file_util', 'config_util', 'format_util'):
        modules['util.' + name] = types.ModuleType('util.' + name)
    for name in ('get_role_llm_config', 'get_role_tts_config', 'merge_config'):
        setattr(modules['util.config_util'], name, lambda *args: {})
    for name in ('format_m1_card_code', 'format_rtu_card_code'):
        setattr(modules['util.format_util'], name, lambda *args: None)
    return modules


class ApplicationTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory(); self.addCleanup(self.directory.cleanup)
        self.path = Path(self.directory.name) / 'private/device-sessions.json'
        self.environment = patch.dict(os.environ, {'FOLOTOY_DEVICE_SESSIONS_PATH': str(self.path)})
        self.environment.start(); self.addCleanup(self.environment.stop)
        self.modules = patch.dict(sys.modules, app_modules()); self.modules.start(); self.addCleanup(self.modules.stop)
        self.app_module = load('core.folotoy_app', CORE / 'folotoy_app.py')
        self.message_module = load('core.folotoy_msg_processor', CORE / 'folotoy_msg_processor.py')
        self.config = types.SimpleNamespace(
            MQTT_CONFIG={'ip': 'localhost', 'port': 1883, 'cid': 'fixture', 'keep_alive': 30,
                         'qos': 1, 'username': 'fixture', 'password': 'fixture'},
            PROJECT_NAME='folotoy', DEERFLOW_STATE_PATH=str(Path(self.directory.name) / 'state.sqlite3'),
            SPEECH_UDP_SERVER_HOST='127.0.0.1', SPEECH_UDP_SERVER_PORT=8082,
            LLM_TYPE='deerflow', ROLES={str(i): {'llm_type': 'deerflow'} for i in range(1, 8)})
        self.app = self.new_app()

    def new_app(self):
        app = self.app_module.FolotoyApp(self.config, None, None, None)
        app.sent = []
        app.command_call = lambda device, message: app.sent.append((device, message))
        return app

    def login(self):
        self.message_module.FolotoyMsgProcessor(self.app).process_login_event('48ca439d3b78', {})
        return self.app.devices['48ca439d3b78']['token']

    def test_restart_preserves_token_and_drops_active_history(self):
        token = self.login()
        self.app.sessions[token]['messages'] = {'1': [{'role': 'user', 'content': 'private fixture'}]}
        self.app.sessions[token]['currentRecording'] = {'recordingId': 45, 'submitted': True}
        self.app.persist_device_sessions()
        restored = self.new_app()
        self.assertEqual(self.app.devices, restored.devices)
        for name in ('messages', 'currentRecording', 'conversation_ids', 'conversation_rounds'):
            self.assertEqual({}, restored.sessions[token][name])
        self.assertFalse(hasattr(restored, 'conversation_control'))
        self.assertNotIn('private fixture', self.path.read_text())

    def test_message_ids_survive_restart_and_remain_unique_under_concurrency(self):
        self.login(); used = [message['msgId'] for _, message in self.app.sent]
        restored = self.new_app()
        with ThreadPoolExecutor(max_workers=4) as pool:
            next_ids = list(pool.map(lambda _: restored.get_and_increase_msg_id('48ca439d3b78'), range(24)))
        self.assertEqual(24, len(set(next_ids)))
        self.assertGreater(min(next_ids), max(used))
        self.assertGreater(self.new_app().get_and_increase_msg_id('48ca439d3b78'), max(next_ids))

    def test_login_rotation_replaces_previous_token(self):
        old = self.login(); new = self.login(); restored = self.new_app()
        self.assertNotEqual(old, new); self.assertNotIn(old, restored.sessions)
        self.assertIn(new, restored.sessions)

    def test_role_change_and_logout_persist(self):
        token = self.login(); self.app.set_device_role('48ca439d3b78', '4')
        restored = self.new_app(); self.assertEqual('4', restored.devices['48ca439d3b78']['role'])
        self.assertIn(token, restored.sessions)
        restored.forget_device_session('48ca439d3b78')
        self.assertEqual({}, self.new_app().devices)
        self.assertEqual([], STORE.DeviceSessionStore(self.path, 'folotoy').load())

    def test_failed_login_save_does_not_issue_or_install_new_token(self):
        token = self.login(); before = self.app.sent.copy()
        with patch.object(self.app._device_session_store, 'save', side_effect=OSError('fixture')):
            with self.assertRaises(OSError): self.login()
        self.assertEqual(token, self.app.devices['48ca439d3b78']['token'])
        self.assertEqual(before, self.app.sent)

    def test_login_reserves_command_ids_in_one_save(self):
        original = self.app._device_session_store.save
        with patch.object(self.app._device_session_store, 'save', wraps=original) as save:
            token = self.login()
        self.assertEqual(1, save.call_count)
        self.assertEqual([('updatetoken', 0), ('updateconfig', 1)],
                         [(message['identifier'], message['msgId']) for _, message in self.app.sent])
        self.assertEqual(1, self.app.sessions[token]['msgIdCount'])
        self.assertEqual(2, self.new_app().get_and_increase_msg_id('48ca439d3b78'))

    def test_login_does_not_write_again_before_issuing_token(self):
        original = self.app._device_session_store.save
        writes = []
        def fail_any_later_save(records):
            writes.append(True)
            if len(writes) > 1:
                raise OSError('fixture subsequent save failure')
            return original(records)
        with patch.object(self.app._device_session_store, 'save', side_effect=fail_any_later_save):
            token = self.login()
            self.assertEqual(1, len(writes))
            self.assertEqual(2, len(self.app.sent))
            self.assertEqual(token, self.app.sent[0][1]['inputParams']['token'])
            with self.assertRaises(OSError):
                self.app.get_and_increase_msg_id('48ca439d3b78')
        self.assertEqual(token, self.new_app().devices['48ca439d3b78']['token'])

    def test_login_without_config_reserves_only_token_message(self):
        original = self.app._device_session_store.save
        with patch.object(self.app._device_session_store, 'save', wraps=original) as save:
            self.message_module.FolotoyMsgProcessor(self.app).process_login_event(
                '48ca439d3b78', {}, update_config=False)
        self.assertEqual(1, save.call_count)
        self.assertEqual([('updatetoken', 0)],
                         [(message['identifier'], message['msgId']) for _, message in self.app.sent])
        self.assertEqual(1, self.new_app().get_and_increase_msg_id('48ca439d3b78'))

    def test_failed_counter_save_does_not_advance(self):
        token = self.login(); previous = self.app.sessions[token]['msgIdCount']
        with patch.object(self.app._device_session_store, 'save', side_effect=OSError('fixture')):
            with self.assertRaises(OSError): self.app.get_and_increase_msg_id('48ca439d3b78')
        self.assertEqual(previous, self.app.sessions[token]['msgIdCount'])

    def test_bad_state_and_unconfigured_role_are_not_restored(self):
        self.login(); self.path.write_text('{broken')
        self.assertEqual({}, self.new_app().devices)
        STORE.DeviceSessionStore(self.path, 'folotoy').save([
            {'device': '48ca439d3b78', 'token': 'a' * 32, 'role': '999', 'msg_id': 2}])
        self.assertEqual({}, self.new_app().devices)

    def test_old_token_recording_is_accepted_by_udp_after_reconstruction(self):
        token = self.login(); restored = self.new_app(); calls = []
        processor = types.ModuleType('core.speech_wav_processor')
        processor.SpeechWavProcessor = lambda app: types.SimpleNamespace(
            write_wav=lambda frames, auth, recording: ('fixture.wav', bool(frames)),
            do_task=lambda *args: calls.append(args))
        with patch.dict(sys.modules, {'core.speech_wav_processor': processor}):
            speech = load('core.speech_server', BASE / 'core/speech_server.py')
        header = bytes.fromhex(token) + (48).to_bytes(2, 'big')
        packets = iter([header + b'\x02\x01', header + b'\x01' + b'\x00' * 1280, header + b'\x02\x02'])
        def receive(size):
            try: return next(packets), ('127.0.0.1', 1234)
            except StopIteration: raise OSError('fixture done')
        with patch.object(speech.socket, 'socket', return_value=types.SimpleNamespace(recvfrom=receive)):
            server = speech.SpeechServer(restored)
        server.on_recording(); server.recording_executor.shutdown(wait=True)
        self.assertEqual(1, len(calls)); self.assertEqual(token, calls[0][0]); self.assertTrue(calls[0][1])
        self.assertEqual(32000, restored.sessions[token]['rate'])
        self.assertTrue(CONTROL.get_conversation_control(restored).is_current(calls[0][-1]))


if __name__ == '__main__':
    unittest.main()
