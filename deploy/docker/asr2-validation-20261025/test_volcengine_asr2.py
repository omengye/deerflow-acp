"""Offline protocol/behavior tests. No credentials, network or paid API calls."""
import asyncio
import gzip
import importlib.util
import json
from pathlib import Path
import struct
import sys
import tempfile
import unittest
from unittest.mock import patch
import wave

FILE = Path(__file__).with_name('volcengine_asr2.py')
SPEC = importlib.util.spec_from_file_location('asr2_candidate_test_module', FILE)
asr = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = asr
SPEC.loader.exec_module(asr)


def response(payload=None, *, flags=1, sequence=1, compressed=True, kind=9, error_code=45000001):
    body = json.dumps(payload if payload is not None else {'result': {'text': '最终文本'}}).encode()
    if compressed:
        body = gzip.compress(body)
    header = bytes((0x11, (kind << 4) | flags, 0x10 | int(compressed), 0))
    if kind == 15:
        prefix = struct.pack('>I', error_code)
    else:
        prefix = struct.pack('>i', sequence) if flags & 1 else b''
    return header + prefix + struct.pack('>I', len(body)) + body


def wav_file(directory, *, frames=7200, rate=16000, channels=1, width=2):
    path = Path(directory) / 'voice.wav'
    with wave.open(str(path), 'wb') as audio:
        audio.setnchannels(channels)
        audio.setsampwidth(width)
        audio.setframerate(rate)
        audio.writeframes(b'\x01\x02' * (frames * channels * width // 2))
    return path


class FakeSocket:
    def __init__(self, *, final=None, close_after_interim=False, server_error=False, hang=False):
        self.sent = []
        self.queue = asyncio.Queue()
        self.final = final if final is not None else response({'result': {'text': '最终文本'}}, flags=3, sequence=3)
        self.close_after_interim = close_after_interim
        self.server_error = server_error
        self.hang = hang
        self.closed = False
        self.received_interim = False

    async def send(self, frame):
        self.sent.append(frame)
        kind, flags = frame[1] >> 4, frame[1] & 15
        if self.server_error and kind == 1:
            await self.queue.put(response({'message': 'fixture-PRIVATE-token'}, kind=15, flags=0))
        elif kind == 1:
            await self.queue.put(response({'result': {'text': '暂定文本'}}, flags=1))
        elif kind == 2 and flags & 2 and not self.hang:
            if self.close_after_interim:
                await self.queue.put(ConnectionError('closed with fixture-PRIVATE-token'))
            else:
                await self.queue.put(self.final)

    async def recv(self):
        value = await self.queue.get()
        if isinstance(value, Exception):
            raise value
        self.received_interim = True
        return value


class FakeConnection:
    def __init__(self, socket, error=None):
        self.socket, self.error, self.calls = socket, error, []

    def __call__(self, endpoint, **kwargs):
        self.calls.append((endpoint, kwargs))
        return self

    async def __aenter__(self):
        if self.error:
            raise self.error
        return self.socket

    async def __aexit__(self, *unused):
        self.socket.closed = True


class ProtocolTests(unittest.TestCase):
    def test_client_frames_have_bounded_compressed_payload_and_explicit_last_flag(self):
        frame = asr.client_frame(b'{"a":1}')
        self.assertEqual(frame[:4], bytes((0x11, 0x10, 0x11, 0)))
        self.assertEqual(struct.unpack('>I', frame[4:8])[0], len(frame) - 8)
        self.assertEqual(gzip.decompress(frame[8:]), b'{"a":1}')
        frame = asr.client_frame(b'\x01\x02', audio=True, final=True)
        self.assertEqual(frame[:4], bytes((0x11, 0x22, 0x01, 0)))
        self.assertEqual(gzip.decompress(frame[8:]), b'\x01\x02')

    def test_signed_server_sequence_and_final_flag_including_official_positive_example(self):
        for sequence in (-3, 3):
            item = asr.parse_server_frame(response(flags=3, sequence=sequence))
            self.assertTrue(item.final)
            self.assertEqual(item.sequence, sequence)
        self.assertFalse(asr.parse_server_frame(response(flags=1, sequence=4)).final)
        item = asr.parse_server_frame(response(flags=2, compressed=False))
        self.assertTrue(item.final)
        self.assertIsNone(item.sequence)
        for sequence in (0, -3):
            with self.assertRaises(asr.Asr2Error):
                asr.parse_server_frame(response(flags=1, sequence=sequence))

    def test_rejects_truncated_wrong_length_unsupported_and_non_json_responses(self):
        good = response()
        cases = [b'', good[:-1], good + b'X', b'\x21' + good[1:],
                 good[:2] + b'\x12' + good[3:], b'\x11\x94' + good[2:],
                 response([], flags=2), b'\x11\x92\x10\x00\x00\x00\x00\x01{']
        for frame in cases:
            with self.subTest(frame=frame[:4]), self.assertRaises(asr.Asr2Error):
                asr.parse_server_frame(frame)

    def test_valid_header_extensions_and_bad_gzip(self):
        good = response(flags=2)
        extended = b'\x12' + good[1:4] + b'\0\0\0\0' + good[4:]
        self.assertTrue(asr.parse_server_frame(extended).final)
        invalid = b'\x11\x92\x11\x00\x00\x00\x00\x04nope'
        with self.assertRaises(asr.Asr2Error) as caught:
            asr.parse_server_frame(invalid)
        self.assertEqual(caught.exception.code, 'asr2_invalid_gzip')

    def test_gzip_expansion_is_bounded(self):
        frame = response({'result': {'text': 'X' * 1000}}, flags=2)
        with patch.object(asr, 'MAX_JSON_BYTES', 64), self.assertRaises(asr.Asr2Error) as caught:
            asr.parse_server_frame(frame)
        self.assertEqual(caught.exception.code, 'asr2_response_too_large')

    def test_server_errors_never_return_private_echoes(self):
        with self.assertRaises(asr.Asr2Error) as caught:
            asr.parse_server_frame(response({'message': 'fixture-PRIVATE-token'}, kind=15, flags=0))
        self.assertEqual(caught.exception.public(), {'error': 'asr2_server_error', 'status': 45000001})
        self.assertNotIn('fixture-PRIVATE', str(caught.exception))
        with self.assertRaises(asr.Asr2Error) as caught:
            asr.parse_server_frame(response({'code': 45000151, 'message': 'fixture-PRIVATE-token'}))
        self.assertEqual(caught.exception.status, 45000151)

    def test_authentication_modes_and_only_2_0_resources(self):
        headers = asr.authorization_headers(api_key='fixture-api', app_id='legacy', access_token='old')
        self.assertEqual(headers['X-Api-Key'], 'fixture-api')
        self.assertNotIn('X-Api-Access-Key', headers)
        for resource in asr.RESOURCE_IDS:
            headers = asr.authorization_headers(app_id='fixture-app', access_token='fixture-token', resource_id=resource)
            self.assertEqual(headers['X-Api-Access-Key'], 'fixture-token')
            self.assertNotIn('X-Api-Key', headers)
        for kwargs in ({}, {'api_key': ''}, {'api_key': 'bad\r\nHeader: injected'},
                       {'api_key': 'fixture', 'resource_id': 'volc.bigasr.sauc.duration'}):
            with self.subTest(kwargs=kwargs), self.assertRaises(asr.Asr2Error):
                asr.authorization_headers(**kwargs)

    def test_wav_is_validated_and_pcm_excludes_riff_header(self):
        with tempfile.TemporaryDirectory() as directory:
            path = wav_file(directory, frames=16)
            pcm = asr.read_pcm_wav(path)
            self.assertEqual(pcm, b'\x01\x02' * 16)
            self.assertFalse(pcm.startswith(b'RIFF'))
            for change in ({'rate': 8000}, {'channels': 2}, {'width': 1}):
                path = wav_file(directory, **change)
                with self.assertRaises(asr.Asr2Error):
                    asr.read_pcm_wav(path)
            path = wav_file(directory)
            path.write_bytes(path.read_bytes()[:-2])
            with self.assertRaises(asr.Asr2Error) as caught:
                asr.read_pcm_wav(path)
            self.assertEqual(caught.exception.code, 'asr2_truncated_audio')


class AsyncBehaviorTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.path = wav_file(self.directory.name)
        self.real_sleep = asyncio.sleep

    async def asyncTearDown(self):
        self.directory.cleanup()

    async def no_delay(self, unused):
        await self.real_sleep(0)

    async def recognize(self, connection, **kwargs):
        with patch.object(asr.asyncio, 'sleep', self.no_delay):
            return await asr.transcribe(self.path, api_key='fixture-PRIVATE-token', _connector=connection, **kwargs)

    async def test_nostream_sends_all_audio_without_waiting_for_per_chunk_response(self):
        socket = FakeSocket()
        connection = FakeConnection(socket)
        result = await asyncio.wait_for(self.recognize(connection), timeout=1)
        self.assertEqual(result['text'], '最终文本')
        self.assertTrue(result['final'])
        self.assertEqual(result['resource_id'], asr.RESOURCE_ID)
        self.assertTrue(socket.closed)
        self.assertEqual(len(connection.calls), 1)
        endpoint, options = connection.calls[0]
        self.assertEqual(endpoint, asr.ENDPOINT)
        self.assertIn('extra_headers', options)
        self.assertNotIn('fixture-PRIVATE', json.dumps(result))
        payload = json.loads(gzip.decompress(socket.sent[0][8:]))
        self.assertEqual(payload['audio'], {'format':'pcm','codec':'raw','rate':16000,'bits':16,'channel':1,'language':'zh-CN'})
        self.assertFalse(payload['request']['enable_ddc'])
        self.assertFalse(any(word in json.dumps(payload).lower() for word in ('hotword', 'corpus', 'context')))
        audio_frames = socket.sent[1:]
        self.assertEqual(len(audio_frames), 3)
        self.assertTrue(all(frame[1] == 0x20 for frame in audio_frames[:-1]))
        self.assertEqual(audio_frames[-1][1], 0x22)
        self.assertEqual(b''.join(gzip.decompress(frame[8:]) for frame in audio_frames), asr.read_pcm_wav(self.path))

    async def test_interim_text_followed_by_disconnect_cannot_be_a_success(self):
        connection = FakeConnection(FakeSocket(close_after_interim=True))
        with self.assertRaises(asr.Asr2Error) as caught:
            await self.recognize(connection)
        self.assertEqual(caught.exception.code, 'asr2_transport_error')
        self.assertNotIn('fixture-PRIVATE', str(caught.exception))
        self.assertEqual(len(connection.calls), 1)

    async def test_last_frame_must_contain_nonempty_final_text(self):
        for payload, expected in [({}, 'asr2_final_text_missing'), ({'result': {'text': ' '}}, 'asr2_empty_final_text')]:
            connection = FakeConnection(FakeSocket(final=response(payload, flags=2)))
            with self.subTest(payload=payload), self.assertRaises(asr.Asr2Error) as caught:
                await self.recognize(connection)
            self.assertEqual(caught.exception.code, expected)

    async def test_early_server_error_stops_sender_and_never_falls_back(self):
        socket = FakeSocket(server_error=True)
        connection = FakeConnection(socket)
        with self.assertRaises(asr.Asr2Error) as caught:
            await self.recognize(connection)
        self.assertEqual(caught.exception.status, 45000001)
        self.assertTrue(socket.closed)
        self.assertEqual(len(connection.calls), 1)
        count = len(socket.sent)
        await self.real_sleep(0.01)
        self.assertEqual(len(socket.sent), count)

    async def test_timeout_does_not_return_partial_text_or_leave_tasks_running(self):
        socket = FakeSocket(hang=True)
        connection = FakeConnection(socket)
        with self.assertRaises(asr.Asr2Error) as caught:
            await self.recognize(connection, timeout=0.03)
        self.assertEqual(caught.exception.code, 'asr2_timeout')
        self.assertTrue(socket.closed)
        self.assertEqual(len(connection.calls), 1)

    async def test_handshake_rejection_status_is_safe(self):
        class Rejection(Exception):
            status_code = 403
        connection = FakeConnection(FakeSocket(), error=Rejection('fixture-PRIVATE-token'))
        with self.assertRaises(asr.Asr2Error) as caught:
            await self.recognize(connection)
        self.assertEqual(caught.exception.public(), {'error':'asr2_handshake_rejected','status':403})
        self.assertNotIn('fixture-PRIVATE', str(caught.exception))

    async def test_premature_final_response_is_rejected(self):
        socket = FakeSocket()
        await socket.queue.put(response(flags=2))
        with self.assertRaises(asr.Asr2Error) as caught:
            await self.recognize(FakeConnection(socket))
        self.assertEqual(caught.exception.code, 'asr2_premature_final_response')

    async def test_bad_timeout_is_rejected_without_connecting(self):
        connection = FakeConnection(FakeSocket())
        for timeout in (0, -1, True, float('nan'), float('inf'), 181):
            with self.subTest(timeout=timeout), self.assertRaises(asr.Asr2Error):
                await self.recognize(connection, timeout=timeout)
        self.assertEqual(connection.calls, [])

    async def test_sync_wrapper_rejects_running_loop_without_creating_a_coroutine(self):
        with self.assertRaises(asr.Asr2Error) as caught:
            asr.VolcengineAsr2Engine().sound_to_text(self.path, {'api_key':'fixture'})
        self.assertEqual(caught.exception.code, 'asr2_sync_called_from_running_loop')


if __name__ == '__main__':
    unittest.main()
