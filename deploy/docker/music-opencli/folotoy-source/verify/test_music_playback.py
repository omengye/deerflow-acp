"""Offline main-process music queue/HTTP/voice ordering tests; no real MQTT."""
import ast
from concurrent.futures import ThreadPoolExecutor
import importlib.util
import json
from pathlib import Path
import queue
import sys
import tempfile
import threading
import time
import types
import unittest
from unittest.mock import patch
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parents[1]


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


core = types.ModuleType('core')
core.__path__ = [str(ROOT / 'src/core')]
sys.modules['core'] = core
CONTROL = load('core.conversation_control', ROOT / 'src/core/conversation_control.py')
MUSIC = load('core.music_playback', ROOT / 'src/core/music_playback.py')


class FakeApp:
    def __init__(self, audio):
        self.config = types.SimpleNamespace(AUDIO_SAVE_PATH=str(audio),
            AUDIO_DOWNLOAD_URL='http://audio.invalid', VOICE_EXECUTOR_MAX_WORKERS=1)
        self.connected_flag = True
        self.devices = {'device': {'token': 'device-session'}}
        self.sessions = {'device-session': {}}
        self.product_key = 'folotoy'
        self.qos = 1
        self.sent = []
        self.msg_id = 0
        self.mqtt_client = types.SimpleNamespace(publish=self.publish)
        self.stt_sdk = object()
        self.llm_sdk = object()
        self.tts_sdk = types.SimpleNamespace(text_to_sound=lambda *a: (True, 'voice.wav'))

    def publish(self, topic, payload, qos):
        self.sent.append((topic, json.loads(payload)))
        return types.SimpleNamespace(rc=0)

    def get_and_increase_msg_id(self, dkey):
        self.msg_id += 1
        return self.msg_id

    def command_call(self, dkey, message):
        self.sent.append(('/user/folotoy/' + dkey + '/thing/command/call', message))


class PlaybackTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        root = Path(self.directory.name)
        self.media = root / 'music'; self.media.mkdir()
        self.audio = root / 'audio'; self.audio.mkdir()
        self.source = self.media / 'song.mp3'; self.source.write_bytes(b'ID3fixture')
        self.events = queue.Queue()
        self.app = FakeApp(self.audio)
        self.config = {'token': 'test-bearer-' + 'x' * 32,
            'media_root': str(self.media), 'public_audio_base': 'http://audio.invalid',
            'default_device_key': 'device', 'listen_host': '127.0.0.1',
            'listen_port': 0, 'prepare_timeout': 5}
        self.service = MUSIC.MusicPlaybackService(self.app, self.config,
            fetch_events=self.events, prepare=self.prepare)
        self.app.music_playback = self.service
        self.addCleanup(self.service.stop)
        self.control = CONTROL.get_conversation_control(self.app)

    def prepare(self, job):
        filename = 'music-' + job.id + '.mp3'
        (self.audio / filename).write_bytes(b'ID3prepared')
        return filename, 'http://audio.invalid/' + filename

    def submit(self, **values):
        return self.service.submit({'path': 'song.mp3',
            'idempotency_key': 'request-123456', **values})

    def await_state(self, job, expected, timeout=3):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            value = self.service.get(job['id'])
            if value['state'] in expected:
                return value
            time.sleep(.01)
        self.fail('job did not reach ' + repr(expected))

    def processor(self):
        config_module = types.ModuleType('config')
        config_module.Config = type('Config', (), {})
        fake_app_module = types.ModuleType('core.folotoy_app')
        fake_app_module.FolotoyApp = object
        config_util = types.ModuleType('util.config_util')
        for name in ('get_role_llm_config', 'get_role_stt_config', 'get_role_tts_config'):
            setattr(config_util, name, lambda *a: {})
        with patch.dict(sys.modules, {'config': config_module,
            'core.folotoy_app': fake_app_module, 'util.config_util': config_util}):
            module = load('_music_test_speech', ROOT / 'src/core/speech_wav_processor.py')
        processor = module.SpeechWavProcessor(self.app)
        self.addCleanup(processor.voice_executor.shutdown)
        return processor

    def test_standalone_publishes_existing_protocol_without_claiming_delivery(self):
        job, repeated = self.submit()
        final = self.await_state(job, {'published'})
        self.assertFalse(repeated)
        self.assertFalse(final['delivery_confirmed'])
        self.assertFalse(final['device_audio_requested'])
        topic, message = self.app.sent[0]
        self.assertEqual(topic, '/user/folotoy/device/thing/command/call')
        self.assertEqual(message['identifier'], 'updatestartvoice')
        self.assertEqual(message['inputParams']['keyCode'], 7)
        self.assertEqual(message['inputParams']['url'], final['url'])
        self.assertEqual(len(message['inputParams']['etag']), 32)

    def test_idempotency_does_not_replay_after_source_removed(self):
        job, _ = self.submit()
        self.await_state(job, {'published'})
        self.source.unlink()
        replay, repeated = self.submit()
        self.assertTrue(repeated)
        self.assertEqual(replay['id'], job['id'])
        self.assertEqual(len(self.app.sent), 1)
        with self.assertRaises(MUSIC.MusicError) as failure:
            self.submit(path='different.mp3')
        self.assertEqual(failure.exception.code, 'idempotency_conflict')

    def test_path_escape_invalid_extension_and_empty_file_rejected(self):
        outside = Path(self.directory.name) / 'outside.mp3'; outside.write_bytes(b'ID3')
        for value in ('../outside.mp3', str(outside), 'folder\\song.mp3', 'missing.mp3'):
            with self.subTest(value=value), self.assertRaises(MUSIC.MusicError):
                self.submit(path=value)
        private = self.media / 'config.json'; private.write_text('{}')
        with self.assertRaises(MUSIC.MusicError):self.submit(path='config.json')
        self.source.write_bytes(b'')
        with self.assertRaises(MUSIC.MusicError):self.submit()
        self.assertEqual(self.app.sent, [])

    def test_symlink_outside_media_root_rejected_when_supported(self):
        outside = Path(self.directory.name) / 'outside.mp3'; outside.write_bytes(b'ID3')
        link = self.media / 'escape.mp3'
        try:link.symlink_to(outside)
        except OSError:self.skipTest('OS does not grant symlink creation')
        with self.assertRaises(MUSIC.MusicError):self.submit(path=link.name)

    def test_dry_run_prepares_but_never_publishes(self):
        job, _ = self.submit(dry_run=True)
        final = self.await_state(job, {'dry_run'})
        self.assertTrue((self.audio / final['filename']).is_file())
        self.assertEqual(self.app.sent, [])

    def test_voice_music_follows_tts_and_precedes_one_terminal_even_if_prepare_late(self):
        gate = threading.Event()
        self.service._prepare_override = lambda job: (gate.wait(2) and self.prepare(job))
        turn = self.control.begin('device', '1', 'device-session', 42)
        processor = self.processor()
        processor._send_deerflow_sentence(turn, '准备音乐。', None, None)
        job, _ = self.submit(request_id=turn.request_id, device_key='device')
        with ThreadPoolExecutor(1) as pool:
            finished = pool.submit(processor._finish_deerflow, turn)
            time.sleep(.08)
            self.assertFalse(finished.done())
            self.assertEqual([m['identifier'] for _, m in self.app.sent], ['audioplay'])
            gate.set();finished.result(3)
        processor._finish_deerflow(turn)
        self.assertEqual([m['identifier'] for _, m in self.app.sent],
                         ['audioplay', 'audioplay', 'audioplay_cmd'])
        self.assertEqual(self.app.sent[1][1]['inputParams']['order'], 2)
        self.assertEqual(self.app.sent[2][1]['inputParams'], {'recordingId':42,'total':2})
        self.assertEqual(self.service.get(job['id'])['state'], 'published')

    def test_new_recording_cancels_old_music_and_drops_old_terminal(self):
        gate = threading.Event()
        self.service._prepare_override = lambda job: (gate.wait(2) and self.prepare(job))
        turn = self.control.begin('device', '1', 'device-session', 42)
        job, _ = self.submit(request_id=turn.request_id)
        processor = self.processor()
        with ThreadPoolExecutor(1) as pool:
            finished = pool.submit(processor._finish_deerflow, turn)
            time.sleep(.05)
            self.control.begin('device', '1', 'device-session', 43)
            finished.result(2)
            gate.set()
        self.await_state(job, {'cancelled'})
        time.sleep(.05)
        self.assertEqual(self.service.get(job['id'])['state'], 'cancelled')
        self.assertEqual(self.app.sent, [])

    def test_finished_voice_rejects_new_jobs_but_retries_existing(self):
        turn = self.control.begin('device', '1', 'device-session', 42)
        job, _ = self.submit(request_id=turn.request_id, dry_run=True)
        self.await_state(job, {'dry_run'})
        self.processor()._finish_deerflow(turn)
        _, repeated = self.submit(request_id=turn.request_id, dry_run=True)
        self.assertTrue(repeated)
        with self.assertRaises(MUSIC.MusicError) as error:
            self.submit(request_id=turn.request_id,idempotency_key='another-request')
        self.assertEqual(error.exception.status,409)

    def test_standalone_waits_for_voice_and_is_cancelled_by_new_voice(self):
        turn = self.control.begin('device', '1', 'device-session', 42)
        job, _ = self.submit()
        self.await_state(job, {'waiting_turn'})
        self.assertEqual(self.app.sent, [])
        self.control.apply(turn,lambda:setattr(turn,'terminal_sent',True))
        self.await_state(job, {'published'})
        next_turn = self.control.begin('device','1','device-session',43)
        other,_=self.submit(idempotency_key='another-request')
        self.await_state(other, {'waiting_turn'})
        self.control.begin('device','1','device-session',44)
        self.await_state(other,{'cancelled'})
        self.assertEqual(len(self.app.sent),1)

    def test_only_configured_default_can_play_before_device_relogin(self):
        self.app.devices.clear()
        job,_=self.submit()
        self.await_state(job,{'published'})
        with self.assertRaises(MUSIC.MusicError):self.submit(device_key='unknown',idempotency_key='other-12345')

    def test_audio_fetch_audit_distinguishes_generic_http_from_esp32(self):
        job,_=self.submit();final=self.await_state(job,{'published'})
        self.events.put({'filename':final['filename'],'esp32':False})
        self.assertTrue(self.service.get(job['id'])['audio_requested'])
        self.assertFalse(self.service.get(job['id'])['device_audio_requested'])
        self.events.put({'filename':final['filename'],'esp32':True})
        self.assertTrue(self.service.get(job['id'])['device_audio_requested'])
        self.assertFalse(self.service.get(job['id'])['delivery_confirmed'])

    def test_mqtt_unavailable_and_invalid_voice_request_rejected(self):
        self.app.connected_flag=False
        with self.assertRaises(MUSIC.MusicError) as error:self.submit()
        self.assertEqual(error.exception.code,'mqtt_unavailable')
        self.app.connected_flag=True
        with self.assertRaises(MUSIC.MusicError) as error:self.submit(request_id='not-a-current-turn')
        self.assertEqual(error.exception.code,'voice_request_not_current')

    def request(self,path,body=None,token=None,content_type='application/json'):
        url='http://127.0.0.1:'+str(self.service._server.server_port)+path
        headers={'Authorization':'Bearer '+(token or self.config['token'])}
        if body is not None:headers['Content-Type']=content_type
        req=urllib.request.Request(url,data=json.dumps(body).encode() if body is not None else None,headers=headers)
        try:
            with urllib.request.urlopen(req,timeout=3) as response:return response.status,json.load(response)
        except urllib.error.HTTPError as error:return error.code,json.load(error)

    def test_real_loopback_http_auth_and_json_contract(self):
        self.service.start()
        status,body=self.request('/v1/music/health',token='wrong')
        self.assertEqual((status,body),(401,{'ok':False,'error':'unauthorized'}))
        self.assertEqual(self.request('/v1/music/health')[0],200)
        self.assertEqual(self.request('/v1/music/play',{},content_type='text/plain')[0],415)
        status,body=self.request('/v1/music/play',{'path':'song.mp3','idempotency_key':'http-test-123'})
        self.assertEqual(status,202)
        self.assertEqual(self.request('/v1/music/jobs/'+body['job']['id'])[0],200)
        self.assertEqual(self.request('/v1/music/play',{'path':'song.mp3','idempotency_key':'http-test-123'})[0],200)

    def test_provider_only_injects_matching_registered_voice_context(self):
        tree=ast.parse((ROOT/'src/sdk/llm_sdk/deerflow.py').read_text(encoding='utf8'))
        function=next(n for n in tree.body if isinstance(n,ast.FunctionDef) and n.name=='_music_context')
        namespace={'json':json};exec(compile(ast.Module(body=[function],type_ignores=[]),'<context>','exec'),namespace)
        helper=namespace['_music_context']
        context={'device_key':'device','request_id':'request-1','role':'2'}
        self.assertIn('"device_key":"device"',helper({'_music_voice_context':context},'device','request-1','2'))
        self.assertEqual(helper({'_music_voice_context':context},'other','request-1','2'),'')
        self.assertEqual(helper({'_music_voice_context':context},'device','another','2'),'')
        self.assertEqual(helper({},'device','request-1','2'),'')

    def test_audio_middleware_records_only_successful_music_gets(self):
        tree=ast.parse((ROOT/'src/core/api_server.py').read_text(encoding='utf8'))
        function=next(n for n in tree.body if isinstance(n,ast.FunctionDef) and n.name=='music_fetch_event')
        namespace={};exec(compile(ast.Module(body=[function],type_ignores=[]),'<audio-fetch>','exec'),namespace)
        event=namespace['music_fetch_event'];filename='/music-'+'a'*32+'.mp3'
        self.assertEqual(event('GET',200,filename,'ESP32HTTPClient'),{'filename':filename[1:],'esp32':True})
        self.assertFalse(event('GET',206,filename,'curl/8')['esp32'])
        self.assertIsNone(event('HEAD',200,filename,'ESP32HTTPClient'))
        self.assertIsNone(event('GET',404,filename,'ESP32HTTPClient'))
        self.assertIsNone(event('GET',200,'/config.json','ESP32HTTPClient'))


if __name__=='__main__':unittest.main()
