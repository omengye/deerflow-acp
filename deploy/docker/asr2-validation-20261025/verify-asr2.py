"""Isolated model check; runs inside FoloToy without changing its active ASR.

--run makes real ASR requests. --synthesize creates a known test utterance with
the existing TTS configuration, then removes just that newly created file.
This verifier never sends MQTT, invokes DeerFlow, or reads user recordings.
"""
import argparse
import asyncio
import hashlib
import json
import logging
import os
from pathlib import Path
import subprocess
import tempfile
import time
import wave


def emit(value):
    print(json.dumps(value, ensure_ascii=False), flush=True)


def existing_credentials():
    return {'api_key': os.environ.get('VOLCENGINE_ASR2_API_KEY') or None,
            'app_id': os.environ.get('VOLCENGINE_ASR_APP_ID') or None,
            'access_token': os.environ.get('VOLCENGINE_ASR_ACCESS_TOKEN') or None}


def prepare_speech(args, directory, report):
    if args.wav:
        return args.wav.resolve(strict=True)
    from config import Config
    from sdk.tts import TtsSdk
    from util.config_util import get_role_tts_config
    config = Config()
    role = config.ROLES.get('1', {})
    tts_type = role.get('tts_type', config.TTS_TYPE)
    audio_root = Path(config.AUDIO_SAVE_PATH).expanduser().resolve(strict=True)
    before = {p.name for p in audio_root.iterdir()}
    options = dict(get_role_tts_config(tts_type, role, config))
    options['timeout'] = min(args.timeout, 30)
    ok, name = TtsSdk(config).text_to_sound(args.text, tts_type, options)
    if not ok or not name or Path(str(name)).name != name:
        raise RuntimeError('test_tts_failed')
    source = (audio_root / name).resolve(strict=True)
    if source.parent != audio_root or name in before:
        raise RuntimeError('test_audio_not_newly_created')
    try:
        wav = directory / 'sample.wav'
        result = subprocess.run(['ffmpeg', '-nostdin', '-v', 'error', '-i', str(source),
                                 '-ac', '1', '-ar', '16000', '-c:a', 'pcm_s16le', str(wav)],
                                capture_output=True, timeout=25)
        if result.returncode:
            raise RuntimeError('test_audio_conversion_failed')
    finally:
        source.unlink()
        report['synthetic_source_audio_removed'] = True
    report['input_kind'] = 'synthetic_tts_not_human_error_sample'
    report['reference_text'] = args.text
    return wav


async def old_baseline(wav, timeout):
    from sdk.stt_sdk.volcengine_asr import AsrWsClient
    client = AsrWsClient(str(wav), cluster=os.environ['VOLCENGINE_ASR_CLUSTER_ID'],
                         appid=os.environ['VOLCENGINE_ASR_APP_ID'],
                         token=os.environ['VOLCENGINE_ASR_ACCESS_TOKEN'], format='wav')
    started = time.monotonic()
    raw = await asyncio.wait_for(client.execute(), timeout)
    payload = raw.get('payload_msg') or {}
    text = ((payload.get('result') or [{}])[0]).get('text')
    return {'status': 'recognized' if payload.get('code') == 1000 and text else 'failed',
            'provider_code': payload.get('code'), 'text': text,
            'elapsed_ms': round((time.monotonic()-started)*1000)}


async def new_candidate(wav, args):
    from volcengine_asr2 import Asr2Error, transcribe
    started = time.monotonic()
    try:
        value = await transcribe(wav, **existing_credentials(), timeout=args.timeout,
                                 resource_id=args.resource_id)
        return {'status': 'recognized' if value.get('final') and value.get('text') else 'failed',
                **value}
    except Asr2Error as error:
        return {'status': 'blocked' if error.status in (401, 403) else 'failed',
                'error': error.code, 'http_status': error.status,
                'elapsed_ms': round((time.monotonic()-started)*1000)}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--run', action='store_true')
    group = parser.add_mutually_exclusive_group()
    group.add_argument('--wav', type=Path)
    group.add_argument('--synthesize', action='store_true')
    parser.add_argument('--text', default='请查询华为云服务器的运行状态。')
    parser.add_argument('--timeout', type=int, choices=range(5, 91), default=45)
    parser.add_argument('--resource-id', choices=('volc.seedasr.sauc.duration', 'volc.seedasr.sauc.concurrent'), default='volc.seedasr.sauc.duration')
    args = parser.parse_args(argv)
    if not args.run:
        emit({'status': 'not_run', 'reason': 'Supply --run and --wav or --synthesize.'})
        return 0
    if not args.wav and not args.synthesize:
        parser.error('--wav or --synthesize is required with --run')
    if not 1 <= len(args.text) <= 100:
        parser.error('test text must contain 1..100 characters')
    logging.disable(logging.CRITICAL)
    report = {'status': 'running', 'resource_id': args.resource_id,
              'hotwords_enabled': False, 'context_enabled': False,
              'post_correction_enabled': False, 'production_asr_switched': False,
              'physical_toy_audio_published': False}
    try:
        with tempfile.TemporaryDirectory(prefix='folotoy-asr2-input-') as directory:
            wav = prepare_speech(args, Path(directory), report)
            with wave.open(str(wav), 'rb') as source:
                report['audio'] = {'sample_rate': source.getframerate(), 'channels': source.getnchannels(),
                                   'sample_width': source.getsampwidth(),
                                   'duration_seconds': round(source.getnframes()/source.getframerate(), 3),
                                   'sha256': hashlib.sha256(wav.read_bytes()).hexdigest()}
            try:
                report['old_v2'] = asyncio.run(old_baseline(wav, args.timeout))
            except Exception as error:
                report['old_v2'] = {'status': 'failed', 'error_type': type(error).__name__}
            report['asr2'] = asyncio.run(new_candidate(wav, args))
            report['status'] = report['asr2']['status']
            report['asr2_recognition_available'] = report['status'] == 'recognized'
            report['accuracy_comparison_possible'] = report['old_v2']['status'] == 'recognized' and report['asr2_recognition_available']
            report['human_error_audio_evaluated'] = False
    except Exception as error:
        report.update(status='failed', error_type=type(error).__name__)
    emit(report)
    return 0 if report.get('asr2_recognition_available') else 1


if __name__ == '__main__':
    raise SystemExit(main())
