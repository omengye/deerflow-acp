"""Volcengine ASR 2.0 engine for FoloToy, selected by volcengine-asr2.

Only the trusted v3 nostream endpoint and ASR 2.0 resource IDs are supported.
Input is an already-recorded, 8/16/32 kHz mono signed 16-bit PCM WAV file.
8/32 kHz inputs are actually resampled to 16 kHz; 16 kHz PCM is unchanged.
There are no hotwords, textual corrections, or fallback recognition models.

Protocol: https://docs.volcengine.com/docs/DoubaoVoice/LargemodelstreamingautomaticspeechrecognitionAPI?lang=zh
Uses websockets.legacy.client.connect, including Gentoo's websockets 12.0.
"""
import asyncio
from dataclasses import dataclass
import gzip
import json
import logging
import math
from pathlib import Path
import shutil
import struct
import subprocess
import time
import uuid
import wave
import zlib

LOGGER = logging.getLogger(__name__)

ENDPOINT = 'wss://openspeech.bytedance.com/api/v3/sauc/bigmodel_nostream'
RESOURCE_ID = 'volc.seedasr.sauc.duration'
RESOURCE_IDS = frozenset((RESOURCE_ID, 'volc.seedasr.sauc.concurrent'))
RATE = 16000
CHANNELS = 1
SAMPLE_WIDTH = 2
CHUNK_MS = 200
CHUNK_BYTES = RATE * SAMPLE_WIDTH * CHUNK_MS // 1000
MAX_AUDIO_SECONDS = 120
MAX_AUDIO_BYTES = RATE * SAMPLE_WIDTH * MAX_AUDIO_SECONDS
INPUT_RATES = frozenset((8000, 16000, 32000))
MAX_INPUT_AUDIO_BYTES = max(INPUT_RATES) * SAMPLE_WIDTH * MAX_AUDIO_SECONDS
RESAMPLE_TIMEOUT_SECONDS = 10
MAX_WIRE_BYTES = 2 * 1024 * 1024
MAX_JSON_BYTES = 2 * 1024 * 1024


class Asr2Error(Exception):
    """Safe failure code; never retains a server message, URL, or credentials."""
    def __init__(self, code, status=None):
        self.code = code
        self.status = status if isinstance(status, int) and not isinstance(status, bool) else None
        super().__init__(code)

    def public(self):
        return {'error': self.code, **({'status': self.status} if self.status is not None else {})}


@dataclass(frozen=True)
class ServerMessage:
    payload: dict
    final: bool
    sequence: int | None = None


def _json_bytes(value):
    return json.dumps(value, ensure_ascii=False, separators=(',', ':'), allow_nan=False).encode('utf-8')


def client_frame(payload, *, audio=False, final=False):
    """Official no-sequence client layout: flags 0 normally, 2 on last audio."""
    if final and not audio:
        raise Asr2Error('asr2_invalid_client_frame')
    if not isinstance(payload, bytes):
        raise Asr2Error('asr2_invalid_client_frame')
    message_type, serialization = (2, 0) if audio else (1, 1)
    compressed = gzip.compress(payload)
    header = bytes((0x11, (message_type << 4) | (2 if final else 0), (serialization << 4) | 1, 0))
    return header + struct.pack('>I', len(compressed)) + compressed


def _decompress(payload):
    try:
        decoder = zlib.decompressobj(16 + zlib.MAX_WBITS)
        decoded = decoder.decompress(payload, MAX_JSON_BYTES + 1)
        if len(decoded) > MAX_JSON_BYTES or decoder.unconsumed_tail:
            raise Asr2Error('asr2_response_too_large')
        decoded += decoder.flush(MAX_JSON_BYTES + 1 - len(decoded))
        if len(decoded) > MAX_JSON_BYTES:
            raise Asr2Error('asr2_response_too_large')
        if not decoder.eof or decoder.unused_data:
            raise Asr2Error('asr2_invalid_gzip')
        return decoded
    except zlib.error:
        raise Asr2Error('asr2_invalid_gzip') from None


def parse_server_frame(frame):
    if not isinstance(frame, bytes) or len(frame) < 8 or len(frame) > MAX_WIRE_BYTES:
        raise Asr2Error('asr2_invalid_frame')
    version, header_words = frame[0] >> 4, frame[0] & 15
    header_length = header_words * 4
    kind, flags = frame[1] >> 4, frame[1] & 15
    serialization, compression = frame[2] >> 4, frame[2] & 15
    if version != 1 or header_words < 1 or header_length + 4 > len(frame):
        raise Asr2Error('asr2_invalid_header')
    if kind not in (9, 15) or flags not in (0, 1, 2, 3) or compression not in (0, 1):
        raise Asr2Error('asr2_unsupported_frame')
    offset, sequence, error_code = header_length, None, None
    if kind == 15:
        if flags != 0 or len(frame) < offset + 8:
            raise Asr2Error('asr2_invalid_error_frame')
        error_code = struct.unpack_from('>I', frame, offset)[0]
        offset += 4
    elif flags & 1:
        if len(frame) < offset + 8:
            raise Asr2Error('asr2_invalid_sequence')
        sequence = struct.unpack_from('>i', frame, offset)[0]
        offset += 4
        if sequence == 0:
            raise Asr2Error('asr2_invalid_sequence')
        # The official final-frame example uses flags=3 with a positive seq.
        # Finality is flags&2, never the sign of a sequence number alone.
        if not (flags & 2) and sequence < 0:
            raise Asr2Error('asr2_invalid_sequence')
    payload_length = struct.unpack_from('>I', frame, offset)[0]
    offset += 4
    if payload_length != len(frame) - offset:
        raise Asr2Error('asr2_invalid_payload_length')
    if kind == 15:
        # Error bodies can echo headers or credentials. Do not decode/store them.
        raise Asr2Error('asr2_server_error', status=error_code)
    if serialization != 1:
        raise Asr2Error('asr2_unsupported_serialization')
    body = _decompress(frame[offset:]) if compression == 1 else frame[offset:]
    if len(body) > MAX_JSON_BYTES:
        raise Asr2Error('asr2_response_too_large')
    try:
        payload = json.loads(body.decode('utf-8'))
    except (UnicodeError, ValueError, RecursionError):
        raise Asr2Error('asr2_invalid_json') from None
    if not isinstance(payload, dict):
        raise Asr2Error('asr2_invalid_json')
    code = payload.get('code')
    if code not in (None, 0, 20000000):
        raise Asr2Error('asr2_server_error', status=code)
    return ServerMessage(payload, bool(flags & 2), sequence)


def _resample_pcm(pcm, source_rate, *, timeout=RESAMPLE_TIMEOUT_SECONDS):
    """Normalize validated PCM without exposing a filename or URL to FFmpeg."""
    if (isinstance(timeout, bool) or not isinstance(timeout, (int, float))
            or not math.isfinite(timeout) or timeout <= 0):
        raise Asr2Error('asr2_invalid_resample_timeout')
    ffmpeg = shutil.which('ffmpeg')
    if not ffmpeg:
        raise Asr2Error('asr2_resampler_unavailable')
    # Both ends are fixed raw PCM streams. Input bytes and duration were bounded
    # before this call; the output is also capped to 120 seconds of PCM16 mono.
    argv = [ffmpeg, '-hide_banner', '-loglevel', 'error', '-nostdin',
            '-f', 's16le', '-ar', str(source_rate), '-ac', '1', '-i', 'pipe:0',
            '-map', '0:a:0', '-vn', '-c:a', 'pcm_s16le', '-ar', str(RATE),
            '-ac', '1', '-t', str(MAX_AUDIO_SECONDS), '-f', 's16le', 'pipe:1']
    try:
        result = subprocess.run(argv, input=pcm, stdout=subprocess.PIPE,
                                stderr=subprocess.PIPE, check=False,
                                timeout=min(timeout, RESAMPLE_TIMEOUT_SECONDS))
    except subprocess.TimeoutExpired:
        raise Asr2Error('asr2_resample_timeout') from None
    except OSError:
        raise Asr2Error('asr2_resampler_unavailable') from None
    if result.returncode:
        raise Asr2Error('asr2_resample_failed')
    output = result.stdout
    expected_frames = len(pcm) * RATE / (SAMPLE_WIDTH * source_rate)
    if (not isinstance(output, bytes) or not output or len(output) % SAMPLE_WIDTH
            or len(output) > MAX_AUDIO_BYTES
            or abs(len(output) / SAMPLE_WIDTH - expected_frames) > 1):
        raise Asr2Error('asr2_invalid_resampled_audio')
    return output


def read_pcm_wav(filename, *, resample_timeout=RESAMPLE_TIMEOUT_SECONDS):
    try:
        path = Path(filename)
        if not path.is_file() or path.stat().st_size > MAX_INPUT_AUDIO_BYTES + 65536:
            raise Asr2Error('asr2_invalid_audio_file')
        with wave.open(str(path), 'rb') as audio:
            source_rate = audio.getframerate()
            if (source_rate not in INPUT_RATES
                    or (audio.getnchannels(), audio.getsampwidth(), audio.getcomptype())
                    != (CHANNELS, SAMPLE_WIDTH, 'NONE')):
                raise Asr2Error('asr2_requires_supported_mono_s16_wav')
            frames = audio.getnframes()
            if frames <= 0 or frames > source_rate * MAX_AUDIO_SECONDS:
                raise Asr2Error('asr2_audio_duration_out_of_range')
            pcm = audio.readframes(frames + 1)
            if len(pcm) != frames * SAMPLE_WIDTH:
                raise Asr2Error('asr2_truncated_audio')
        if source_rate == RATE:
            return pcm
        normalized = _resample_pcm(pcm, source_rate, timeout=resample_timeout)
        LOGGER.info('ASR2 normalized audio input_rate=%s output_rate=%s input_frames=%s output_frames=%s',
                    source_rate, RATE, frames, len(normalized) // SAMPLE_WIDTH)
        return normalized
    except Asr2Error:
        raise
    except (OSError, EOFError, wave.Error, TypeError, ValueError):
        raise Asr2Error('asr2_invalid_audio_file') from None


def _credential(value):
    return isinstance(value, str) and bool(value.strip()) and len(value) <= 4096 and not any(c in value for c in '\r\n\x00')


def authorization_headers(*, api_key=None, app_id=None, access_token=None, resource_id=RESOURCE_ID, request_id=None):
    if resource_id not in RESOURCE_IDS:
        raise Asr2Error('asr2_requires_2_0_resource')
    request_id = request_id or str(uuid.uuid4())
    headers = {'X-Api-Resource-Id': resource_id, 'X-Api-Connect-Id': request_id,
               'X-Api-Request-Id': request_id, 'X-Api-Sequence': '-1'}
    if api_key is not None:
        if not _credential(api_key):
            raise Asr2Error('asr2_invalid_credentials')
        headers['X-Api-Key'] = api_key
    else:
        if not _credential(app_id) or not _credential(access_token):
            raise Asr2Error('asr2_missing_credentials')
        headers.update({'X-Api-App-Key': app_id, 'X-Api-Access-Key': access_token})
    return headers


def _legacy_connect(*args, **kwargs):
    try:
        from websockets.legacy.client import connect
    except ImportError:
        raise Asr2Error('asr2_websockets_dependency_unavailable') from None
    return connect(*args, **kwargs)


def _final_text(payload):
    result = payload.get('result')
    if isinstance(result, list) and len(result) == 1:
        result = result[0]
    if not isinstance(result, dict) or not isinstance(result.get('text'), str):
        raise Asr2Error('asr2_final_text_missing')
    text = result['text'].strip()
    if not text:
        raise Asr2Error('asr2_empty_final_text')
    return text


async def transcribe(file_path, *, api_key=None, app_id=None, access_token=None,
                     timeout=45, resource_id=RESOURCE_ID, _connector=None):
    """Recognize with ASR 2.0 only; raises safe Asr2Error on every failure.

    _connector is an offline-test seam; production uses the legacy websocket API.
    The operation timeout includes the handshake, audio pacing and final result.
    """
    if isinstance(timeout, bool) or not isinstance(timeout, (int, float)) or not math.isfinite(timeout) or not 0 < timeout <= 180:
        raise Asr2Error('asr2_invalid_timeout')
    started = time.monotonic()
    request_id = str(uuid.uuid4())
    headers = authorization_headers(api_key=api_key, app_id=app_id, access_token=access_token,
                                    resource_id=resource_id, request_id=request_id)
    pcm = read_pcm_wav(file_path, resample_timeout=min(timeout, RESAMPLE_TIMEOUT_SECONDS))
    remaining_timeout = timeout - (time.monotonic() - started)
    if remaining_timeout <= 0:
        raise Asr2Error('asr2_timeout')
    request = {'user': {'uid': 'folotoy-asr2-validation'},
               'audio': {'format': 'pcm', 'codec': 'raw', 'rate': RATE, 'bits': 16, 'channel': 1, 'language': 'zh-CN'},
               'request': {'model_name': 'bigmodel', 'result_type': 'full', 'enable_itn': True,
                           'enable_punc': True, 'enable_ddc': False, 'show_utterances': True}}
    connector = _connector or _legacy_connect

    async def operation():
        async with connector(ENDPOINT, extra_headers=headers, open_timeout=min(15, timeout),
                             close_timeout=1, max_size=MAX_WIRE_BYTES, compression=None,
                             ping_interval=20, ping_timeout=10) as socket:
            await socket.send(client_frame(_json_bytes(request)))
            final_audio_started = asyncio.Event()

            async def send_audio():
                for offset in range(0, len(pcm), CHUNK_BYTES):
                    chunk = pcm[offset:offset + CHUNK_BYTES]
                    last = offset + len(chunk) == len(pcm)
                    if last:
                        final_audio_started.set()
                    await socket.send(client_frame(chunk, audio=True, final=last))
                    if not last:
                        await asyncio.sleep(CHUNK_MS / 1000)

            async def receive_final():
                while True:
                    message = parse_server_frame(await socket.recv())
                    if message.final:
                        if not final_audio_started.is_set():
                            raise Asr2Error('asr2_premature_final_response')
                        return _final_text(message.payload)

            sender = asyncio.create_task(send_audio())
            receiver = asyncio.create_task(receive_final())
            try:
                _, text = await asyncio.gather(sender, receiver)
                return text
            finally:
                for task in (sender, receiver):
                    if not task.done():
                        task.cancel()
                await asyncio.gather(sender, receiver, return_exceptions=True)

    try:
        text = await asyncio.wait_for(operation(), timeout=remaining_timeout)
    except Asr2Error:
        raise
    except asyncio.TimeoutError:
        raise Asr2Error('asr2_timeout') from None
    except Exception as error:
        status = getattr(error, 'status_code', None)
        if not isinstance(status, int):
            status = getattr(getattr(error, 'response', None), 'status_code', None)
        if isinstance(status, int) and 100 <= status <= 599:
            raise Asr2Error('asr2_handshake_rejected', status=status) from None
        raise Asr2Error('asr2_transport_error') from None
    return {'text': text, 'elapsed_ms': round((time.monotonic() - started) * 1000),
            'request_id': request_id, 'resource_id': resource_id, 'final': True}


class VolcengineAsr2Engine:
    """FoloToy wrapper; safe failure is None, with no legacy-model fallback."""
    def __init__(self, config=None):
        self.config = config
        self.name = 'volcengine-asr2'

    def sound_to_text(self, filename, stt_config=None):
        try:
            options = dict(stt_config or {})
            try:
                asyncio.get_running_loop()
            except RuntimeError:
                pass
            else:
                raise Asr2Error('asr2_sync_called_from_running_loop')
            # Production uses only X-Api-Key. Old AppID/Token fields and any
            # extra role settings are deliberately not forwarded.
            result = asyncio.run(transcribe(
                filename, api_key=options.get('api_key'),
                timeout=options.get('timeout', 45),
                resource_id=options.get('resource_id', RESOURCE_ID)))
        except Asr2Error as error:
            LOGGER.warning('ASR2 failed provider=%s error=%s status=%s',
                           self.name, error.code, error.status)
            return None
        except Exception as error:
            # Exception messages can contain request headers or query values.
            LOGGER.warning('ASR2 failed provider=%s error_type=%s',
                           self.name, type(error).__name__)
            return None
        result['provider'] = self.name
        LOGGER.info('ASR2 completed provider=%s resource_id=%s request_id=%s elapsed_ms=%s',
                    self.name, result['resource_id'], result['request_id'], result['elapsed_ms'])
        return result
