"""ASR2 input-rate dispatch and isolated role1 MQTT/UDP -> WS -> TTS validation.

Run inside its Linux/Python 3.11 container with the service's environment.
--run makes paid TTS/ASR/model requests; without it no configuration or network
is accessed. Only a fresh synthetic device's exact MQTT topic is observed.
Roles 2..7 validate their actual SttSdk dispatch and TTS readiness; current
FoloToy has no supported synthetic-device role selection event. Role1 requires
--audit-main-log afterwards to prove the main process used ASR2, not fallback.
"""

import argparse
import ast
from contextlib import closing
import json
import logging
import math
import os
from pathlib import Path
import queue
import re
import secrets
import socket
import sqlite3
import subprocess
import sys
import tempfile
import threading
import time
import wave
from urllib.parse import urlsplit
from urllib.request import ProxyHandler, Request, build_opener


PROJECT = Path(globals().get('__file__', str(Path.cwd()/'verify-rate-voice.py'))).resolve().parents[1]
PROVIDER = 'volcengine-asr2'
RESOURCE_ID = 'volc.seedasr.sauc.duration'
SAMPLE_RATES = (8000, 16000, 32000)
FRAME_TYPES = {32000: b'\x01', 16000: b'\x04', 8000: b'\x05'}


def audio_frame_type(sample_rate):
    if isinstance(sample_rate, bool) or sample_rate not in SAMPLE_RATES:
        raise VoiceFailure('unsupported_device_sample_rate')
    return FRAME_TYPES[sample_rate]


def convert_pcm(source_file, sample_rate, deadline):
    """Generate real PCM at the declared device rate, without changing headers."""
    audio_frame_type(sample_rate)
    conversion = subprocess.run([
        'ffmpeg', '-v', 'error', '-nostdin', '-i', str(source_file),
        '-ac', '1', '-ar', str(sample_rate), '-acodec', 'pcm_s16le', '-f', 's16le', 'pipe:1',
    ], capture_output=True, timeout=remaining(deadline, 20))
    pcm = conversion.stdout
    if (conversion.returncode != 0 or not sample_rate * 2 // 10 <= len(pcm) <= sample_rate * 2 * 20
            or len(pcm) % 2):
        raise VoiceFailure('source_pcm_conversion_failed')
    return pcm


def upload_pcm(udp, destination, token, recording_id, pcm, sample_rate, deadline):
    """Use the actual device frame type and its PCM byte-rate for pacing."""
    frame_type = audio_frame_type(sample_rate)
    if not isinstance(token, bytes) or len(token) != 16 or not 0 <= recording_id <= 65535:
        raise VoiceFailure('invalid_synthetic_recording_identity')
    if not isinstance(pcm, bytes) or not pcm or len(pcm) % 2:
        raise VoiceFailure('invalid_device_pcm')
    header = token + recording_id.to_bytes(2, 'big')
    udp.sendto(header + b'\x02\x01', destination)
    time.sleep(min(.1, remaining(deadline)))
    packets = 0
    for offset in range(0, len(pcm), 1280):
        packet = pcm[offset:offset + 1280]
        udp.sendto(header + frame_type + packet, destination)
        packets += 1
        time.sleep(min(len(packet) / (sample_rate * 2), remaining(deadline)))
    udp.sendto(header + b'\x02\x02', destination)
    return packets


class VoiceFailure(Exception):
    """Fixed diagnostics; provider exceptions and credentials are never printed."""


def emit(value):
    print(json.dumps(value, ensure_ascii=False), flush=True)


def remaining(deadline, maximum=None):
    value = deadline - time.monotonic()
    if value <= 0:
        raise VoiceFailure('overall_deadline_exceeded')
    return min(value, maximum) if maximum is not None else value


def wait_message(messages, deadline, predicate):
    while True:
        try:
            message = messages.get(timeout=remaining(deadline, 0.5))
        except queue.Empty:
            continue
        if predicate(message):
            return message


def state_for_device(path, dkey):
    """Read only the fresh test identity, without changing the production DB."""
    location = Path(path).expanduser().resolve()
    if not location.is_file():
        return None
    try:
        with closing(sqlite3.connect(location.as_uri() + '?mode=ro', uri=True, timeout=1)) as database:
            row = database.execute(
                'SELECT status,session_id,request_id FROM deerflow_sessions '
                'WHERE device=? ORDER BY updated_at DESC LIMIT 1', (dkey,)).fetchone()
            native = None
            if row and row[1]:
                ws = database.execute('SELECT cursor FROM deerflow_ws_sessions WHERE session_id=?', (row[1],)).fetchone()
                native = json.loads(ws[0]) if ws and ws[0] else None
    except sqlite3.OperationalError:
        return None
    return {'status': row[0], 'session_present': bool(row[1]), 'session_id': row[1],
            'request_id': row[2], 'native_cursor': native} if row else None


def cleanup_identity(config, dkey, cleanup, role=1):
    """Close only the recorded synthetic actor, using a separate local WS DB."""
    if not re.fullmatch(r'ASR2TEST[0-9A-F]{16}', dkey):
        raise VoiceFailure('cleanup_device_not_synthetic')
    from util.config_util import get_role_llm_config
    from sdk.llm_sdk import deerflow as provider
    production = Path(config.DEERFLOW_STATE_PATH).resolve()
    with closing(sqlite3.connect(production)) as db:
        sessions = db.execute('SELECT session_id FROM deerflow_sessions WHERE device=? AND session_id IS NOT NULL', (dkey,)).fetchall()
        ids = sorted({row[0] for row in sessions})
        ws = []
        for sid in ids:
            ws.extend(db.execute('SELECT endpoint,workspace,session_id,runtime_id,cursor,models,model,unconfirmed FROM deerflow_ws_sessions WHERE session_id=?', (sid,)).fetchall())
    selected = dict(get_role_llm_config('deerflow', config.ROLES[str(role)], config))
    released = []
    with tempfile.TemporaryDirectory(prefix='music-voice-cleanup-') as temporary:
        selected['state_path'] = str(Path(temporary) / 'cleanup.sqlite3')
        client = provider.get_client(selected)
        try:
            with closing(sqlite3.connect(selected['state_path'])) as db, db:
                for row in ws:
                    db.execute('INSERT OR REPLACE INTO deerflow_ws_sessions VALUES (?,?,?,?,?,?,?,?)', row)
            for sid in ids:
                client.close_session(sid)
                released.append(sid)
        finally:
            provider.close_all()
    if len(released) != len(ids):
        raise VoiceFailure('synthetic_actor_cleanup_incomplete')
    with closing(sqlite3.connect(production, timeout=5)) as db, db:
        db.execute('PRAGMA busy_timeout=5000')
        db.execute('DELETE FROM deerflow_sessions WHERE device=?', (dkey,))
        db.execute('DELETE FROM deerflow_identities WHERE device=?', (dkey,))
        for row in ws:
            db.execute('DELETE FROM deerflow_ws_sessions WHERE endpoint=? AND workspace=? AND session_id=?', row[:3])
    cleanup.update(actors_recorded=len(ids), actors_released=len(released),
                   durable_synthetic_identity_removed=True,
                   transient_device_note='Server has no logout handler; test login remains in RAM until service restart.')


def download_audio(url, configured_base, deadline):
    parsed = urlsplit(str(url))
    base = urlsplit(str(configured_base))
    if (parsed.scheme not in ('http', 'https') or parsed.username or parsed.password
            or parsed.fragment or parsed.netloc != base.netloc
            or parsed.scheme != base.scheme
            or not parsed.path.startswith(base.path.rstrip('/') + '/')):
        raise VoiceFailure('audio_url_does_not_match_service_origin')
    opener = build_opener(ProxyHandler({}))
    with opener.open(str(url), timeout=remaining(deadline, 10)) as response:
        if response.status != 200:
            raise VoiceFailure('audio_http_status_failed')
        if urlsplit(response.geturl()).netloc != base.netloc:
            raise VoiceFailure('audio_redirect_left_service_origin')
        size = len(response.read(10 * 1024 * 1024 + 1))
    if not 0 < size <= 10 * 1024 * 1024:
        raise VoiceFailure('downloaded_audio_size_invalid')
    return size


def role_dispatch_check(config, role, pcm, deadline, sample_rate=32000):
    """Make one real ASR request through the installed public SttSdk path."""
    from sdk.stt import SttSdk
    from sdk.stt_sdk import volcengine_asr2
    from util.config_util import get_role_stt_config
    audio_frame_type(sample_rate)
    selected = config.ROLES[str(role)]
    provider = selected.get('stt_type', config.STT_TYPE)
    if provider != PROVIDER:
        raise VoiceFailure('selected_role_is_not_asr2')
    options = dict(get_role_stt_config(provider, selected, config))
    if options.get('resource_id') != RESOURCE_ID or not options.get('api_key'):
        raise VoiceFailure('asr2_resource_or_private_api_key_unavailable')
    if any(options.get(k) for k in ('hotwords', 'context', 'corpus')):
        raise VoiceFailure('unexpected_asr_hints_enabled')
    options['timeout'] = min(float(options.get('timeout',45)), remaining(deadline,60))
    with tempfile.TemporaryDirectory(prefix='asr2-role-input-') as directory:
        file = Path(directory)/'input.wav'
        with wave.open(str(file),'wb') as output:
            output.setnchannels(1);output.setsampwidth(2);output.setframerate(sample_rate);output.writeframes(pcm)
        result = SttSdk(config).transcribe(str(file), stt_type=provider, stt_config=options)
    if (not isinstance(result,dict) or result.get('provider') != PROVIDER
            or result.get('resource_id') != RESOURCE_ID or result.get('final') is not True
            or not isinstance(result.get('text'),str) or not result['text'].strip()
            or not re.fullmatch(r'[0-9a-fA-F-]{36}',str(result.get('request_id','')))):
        raise VoiceFailure('registered_asr2_dispatch_did_not_return_final_text')
    return {'provider':result['provider'], 'resource_id':result['resource_id'],
            'request_id':result['request_id'], 'final':True,
            'text':result['text'], 'elapsed_ms':result.get('elapsed_ms'),
            'elapsed_includes_audio_replay':True, 'private_api_key_present':True,
            'engine_module_file':volcengine_asr2.__file__, 'fallback_used':False,
            'input_sample_rate':sample_rate, 'input_duration_seconds':len(pcm)/(sample_rate*2)}


def audit_main_log(report, log_text):
    """Attach only the synthetic device's structured ASR2 receipt from logs."""
    if report.get('role') != 1 or report.get('scope') != 'role1_mqtt_udp_ws_tts':
        raise VoiceFailure('main_log_audit_requires_role1_voice_report')
    device = report.get('test_device','')
    if not re.fullmatch(r'ASR2TEST[0-9A-F]{16}',device):
        raise VoiceFailure('audit_device_not_synthetic')
    marker = 'Dkey='+device+': Transcribed:'
    receipts=[]
    for line in log_text.splitlines():
        if marker not in line:
            continue
        raw=line.split(marker,1)[1].strip()
        if len(raw)>65536:
            continue
        try:value=ast.literal_eval(raw)
        except (ValueError,SyntaxError,RecursionError):continue
        if not isinstance(value,dict):continue
        if (value.get('provider') == PROVIDER and value.get('resource_id') == RESOURCE_ID
                and value.get('final') is True and isinstance(value.get('text'),str)
                and value['text'].strip() and re.fullmatch(r'[0-9a-fA-F-]{36}',str(value.get('request_id','')))):
            receipts.append({k:value[k] for k in ('provider','resource_id','request_id','final')})
    report['main_process_asr_receipt'] = receipts[-1] if len(receipts)==1 else None
    report['voice_pipeline_provider_receipt_verified'] = len(receipts)==1
    sample_rate=report.get('input_sample_rate')
    report['device_rate_path_verified']=bool(
        sample_rate in SAMPLE_RATES
        and report.get('device_wire_sample_rate')==sample_rate
        and report.get('device_wire_frame_type_hex')==FRAME_TYPES[sample_rate].hex()
        and (report.get('asr_dispatch') or {}).get('input_sample_rate')==sample_rate)
    if len(receipts)==1:
        report['direct_dispatch_request_id']=report.get('request_id')
        report['request_id']=receipts[0]['request_id']
    report['passed'] = bool(report.get('pipeline_completed') is True
                            and report.get('asr2_dispatch_verified') is True
                            and report['device_rate_path_verified'] is True
                            and len(receipts)==1 and report.get('cleanup',{}).get('durable_synthetic_identity_removed') is True)
    report['status'] = 'passed' if report['passed'] else 'failed'
    if not report['passed']:
        report['failure_code']='main_process_asr2_receipt_missing_or_pipeline_failed'
    return report


def run(args, report):
    started = time.monotonic()
    deadline = started + args.timeout
    # Some provider modules log request payloads on failure. Disable their logs
    # only in this verifier process; the running service is never altered.
    logging.disable(logging.CRITICAL)
    source = next((path for path in (PROJECT / 'src', PROJECT, Path.cwd())
                   if (path / 'config.py').is_file()), PROJECT)
    sys.path.insert(0, str(source))
    import paho.mqtt.client as mqtt
    from config import Config
    from sdk.tts import TtsSdk
    from util.config_util import get_role_tts_config

    config = Config()
    role = config.ROLES.get(str(args.role), {})
    if role.get('llm_type', config.LLM_TYPE) != 'deerflow':
        raise VoiceFailure('device_default_role_is_not_deerflow')
    if getattr(config, 'DEERFLOW_TRANSPORT', '') != 'ws':
        raise VoiceFailure('service_configuration_is_not_websocket')
    if getattr(config,'DEERFLOW_WS_MODE','') != 'fullAccess' or getattr(config,'DEERFLOW_APPROVAL','') != 'allow_always':
        raise VoiceFailure('service_configuration_is_not_full_access')
    if config.STORAGE_TYPE != 'file':
        raise VoiceFailure('live_verifier_requires_local_audio_storage')
    broker = config.MQTT_CONFIG
    dkey = 'ASR2TEST' + secrets.token_hex(8).upper()
    report['test_device'] = dkey
    report.update(role=args.role, provider=PROVIDER, stt_provider=PROVIDER, resource_id=RESOURCE_ID,
                  scope='role1_mqtt_udp_ws_tts' if args.role==1 else 'role_dispatch_and_tts_readiness',
                  transport='ws', ws_mode='fullAccess', approval='allow_always',
                  hotwords_enabled=False, context_enabled=False, post_correction_enabled=False,
                  fallback_used=False, physical_toy_audio_published=False,
                  input_sample_rate=args.sample_rate,
                  device_wire_sample_rate=args.sample_rate,
                  device_wire_frame_type_hex=audio_frame_type(args.sample_rate).hex())
    messages = queue.Queue(maxsize=128)
    subscribed = threading.Event()
    connected = threading.Event()
    overflowed = threading.Event()
    rejected = threading.Event()
    topic = '/user/%s/%s/thing/command/call' % (args.namespace, dkey)
    events_topic = '/user/%s/%s/thing/event/post' % (args.namespace, dkey)
    client = None
    udp = None
    source_file = None
    source_created = False
    loop_started = False
    recording_id = secrets.randbelow(65535) + 1
    report['recording_id'] = recording_id
    report['simulated_device'] = True
    report['physical_toy_playback_claimed'] = False
    terminal_messages = []
    observed_audio = {}
    unexpected_standalone = threading.Event()
    cleanup = report['cleanup'] = {}
    emit({'stage': 'begin', 'test_device': dkey})

    def stage(name, **fields):
        entry = {'stage': name, 'elapsed_ms': round((time.monotonic() - started) * 1000), **fields}
        report['stages'].append(entry)
        emit(entry)

    def on_connect(active, userdata, flags, reason_code, properties):
        if getattr(reason_code, 'is_failure', False) or int(reason_code.value) != 0:
            rejected.set()
            connected.set()
            return
        connected.set()
        active.subscribe(topic, qos=1)

    def on_subscribe(active, userdata, mid, reason_codes, properties):
        if any(getattr(code, 'is_failure', False) for code in reason_codes):
            rejected.set()
        subscribed.set()

    def on_message(active, userdata, message):
        if message.topic != topic or len(message.payload) > 65536:
            return
        try:
            value = json.loads(message.payload)
            if isinstance(value, dict):
                params = value.get('inputParams') or {}
                if value.get('identifier') == 'updatestartvoice':
                    unexpected_standalone.set()
                if params.get('recordingId') == recording_id:
                    if value.get('identifier') == 'audioplay_cmd':
                        terminal_messages.append(value)
                    elif value.get('identifier') == 'audioplay':
                        observed_audio[int(params.get('order', 0))] = str(params.get('url', ''))
                messages.put_nowait(value)
        except queue.Full:
            overflowed.set()
        except (ValueError, UnicodeError):
            pass

    def publish(identifier):
        result = client.publish(events_topic,
            json.dumps({'identifier': identifier, 'inputParams': {}}), qos=1)
        result.wait_for_publish(timeout=remaining(deadline, 5))
        if not result.is_published():
            raise VoiceFailure('mqtt_publish_unconfirmed')

    try:
        tts_type = role.get('tts_type', config.TTS_TYPE)
        tts_config = dict(get_role_tts_config(tts_type, role, config))
        tts_config['timeout'] = remaining(deadline, 30)
        audio_root = Path(config.AUDIO_SAVE_PATH).expanduser().resolve()
        existing = {path.name for path in audio_root.iterdir()} if audio_root.is_dir() else set()
        ok, filename = TtsSdk(config).text_to_sound(args.text, tts_type, tts_config)
        if not ok or not filename or Path(str(filename)).name != filename:
            raise VoiceFailure('source_tts_generation_failed')
        source_file = (audio_root / filename).resolve()
        if source_file.parent != audio_root or not source_file.is_file():
            raise VoiceFailure('source_tts_audio_file_missing')
        source_created = filename not in existing
        stage('source_tts_generated', audio_bytes=source_file.stat().st_size)
        pcm = convert_pcm(source_file,args.sample_rate,deadline)
        stage('source_pcm_ready', pcm_bytes=len(pcm), sample_rate=args.sample_rate,
              duration_seconds=len(pcm)/(args.sample_rate*2))

        dispatch = role_dispatch_check(config,args.role,pcm,deadline,args.sample_rate)
        report.update(asr_dispatch=dispatch, asr2_dispatch_verified=True,
                      request_id=dispatch['request_id'], tts_readiness_verified=True)
        stage('registered_asr2_dispatch_completed', resource_id=dispatch['resource_id'])
        if args.role != 1:
            # No existing protocol safely selects a role for a fresh device.
            # Do not fake a different role or introduce a production test hook.
            report.update(status='passed',passed=True,full_voice_pipeline_tested=False,
                          limitations=['Roles2-7 use their real ASR dispatch and TTS; role1 separately covers the shared MQTT/UDP/WS voice path.'])
            return

        client = mqtt.Client(callback_api_version=mqtt.CallbackAPIVersion.VERSION2,
                             client_id='folotoy-live-' + secrets.token_hex(8))
        client.suppress_exceptions = True
        client.on_connect = on_connect
        client.on_subscribe = on_subscribe
        client.on_message = on_message
        client.username_pw_set(broker['username'], broker['password'])
        client.connect_timeout = remaining(deadline, 10)
        client.connect(host=broker['ip'], port=int(broker['port']), keepalive=30)
        client.loop_start()
        loop_started = True
        if not connected.wait(remaining(deadline, 10)) or rejected.is_set():
            raise VoiceFailure('mqtt_connection_failed')
        if not subscribed.wait(remaining(deadline, 10)) or rejected.is_set():
            raise VoiceFailure('mqtt_subscription_failed')
        publish('login')
        login = wait_message(messages, min(deadline, time.monotonic() + 10),
                             lambda message: message.get('identifier') == 'updatetoken')
        token_hex = str((login.get('inputParams') or {}).get('token', ''))
        if not re.fullmatch(r'[0-9a-fA-F]{32}', token_hex):
            raise VoiceFailure('device_login_token_invalid')
        token = bytes.fromhex(token_hex)
        stage('mqtt_device_logged_in')

        udp = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        udp.settimeout(remaining(deadline, 5))
        destination = (args.udp_host or config.SPEECH_UDP_SERVER_HOST,
                       args.udp_port or int(config.SPEECH_UDP_SERVER_PORT))
        packets=upload_pcm(udp,destination,token,recording_id,pcm,args.sample_rate,deadline)
        uploaded_at = time.monotonic()
        stage('udp_recording_uploaded', audio_packets=packets, pcm_bytes=len(pcm),
              sample_rate=args.sample_rate,frame_type_hex=audio_frame_type(args.sample_rate).hex())

        downloads = {}
        total = None
        while total is None or len(downloads) < total:
            if overflowed.is_set():
                raise VoiceFailure('test_mqtt_queue_overflowed')
            message = wait_message(messages, deadline, lambda item:
                (item.get('inputParams') or {}).get('recordingId') == recording_id
                and item.get('identifier') in ('audioplay', 'audioplay_cmd'))
            params = message.get('inputParams') or {}
            if message['identifier'] == 'audioplay_cmd':
                total = int(params.get('total', 0))
                if not 1 <= total <= 64:
                    raise VoiceFailure('service_returned_no_playable_audio')
                stage('mqtt_audio_completion', segments=total)
            else:
                order = int(params.get('order', 0))
                if order not in downloads:
                    if not 1 <= order <= 64:
                        raise VoiceFailure('audio_segment_order_invalid')
                    downloads[order] = download_audio(params.get('url', ''),
                                                      config.AUDIO_DOWNLOAD_URL, deadline)
                    stage('http_audio_downloaded', order=order, audio_bytes=downloads[order],
                          since_upload_ms=round((time.monotonic() - uploaded_at) * 1000))
        if set(downloads) != set(range(1, total + 1)):
            raise VoiceFailure('audio_segment_sequence_incomplete')
        # The same publisher/connection preserves MQTT order. There must be one
        # completion. This check never commands or plays on a physical device.
        time.sleep(min(.3, remaining(deadline)))
        music_orders = [order for order, url in observed_audio.items()
                        if Path(urlsplit(url).path).name.startswith('music-')
                        and Path(urlsplit(url).path).name.endswith('.mp3')]
        if len(terminal_messages) != 1 or unexpected_standalone.is_set() or music_orders:
            raise VoiceFailure('unexpected_or_duplicate_voice_playback_command')
        report.update(voice_audio_sequence_verified=True, terminal_count=len(terminal_messages),
                      speech_segments=total,
                      download_user_agent='Python-urllib (not ESP32)',
                      native_tool_receipts_audited=False)

        state = None
        state_deadline = min(deadline, time.monotonic() + 5)
        while time.monotonic() < state_deadline:
            state = state_for_device(config.DEERFLOW_STATE_PATH, dkey)
            if state and state['status'] == 'completed' and state['session_present']:
                break
            time.sleep(min(0.1, remaining(deadline)))
        report['conversation_state'] = state['status'] if state else 'missing'
        if not state or state['status'] != 'completed' or not state['session_present']:
            raise VoiceFailure('deerflow_turn_not_completed_fallback_not_accepted')
        report['native_cursor'] = state['native_cursor']
        report['voice_request_id'] = state['request_id']
        report['waku_session_id'] = state['session_id']
        report.update({'status': 'voice_chain_completed_evidence_pending', 'passed':False,
                       'pipeline_completed':True, 'full_voice_pipeline_tested':True,
                       'voice_pipeline_provider_receipt_verified':False, 'audio_segments': total,
                       'downloaded_audio_bytes': sum(downloads.values())})
        stage('asr_ws_llm_tts_chain_completed', conversation_state='completed')
    finally:
        if udp is not None:
            udp.close()
        if client is not None:
            try:
                if connected.is_set() and not rejected.is_set():
                    current = state_for_device(config.DEERFLOW_STATE_PATH, dkey)
                    if not current or current.get('status') != 'completed':
                        # Cancel only the synthetic voice turn before closing
                        # its actor; a real device's topic is never published.
                        result = client.publish(events_topic, json.dumps(
                            {'identifier': 'press_small_btn', 'inputParams': {}}), qos=1)
                        result.wait_for_publish(timeout=2)
                        cleanup['synthetic_cancel_published'] = result.is_published()
                client.disconnect()
                cleanup['mqtt_disconnected'] = True
            except Exception:
                cleanup['mqtt_disconnected'] = False
            finally:
                if loop_started:
                    client.loop_stop()
        if source_created and source_file is not None:
            try:
                source_file.unlink(missing_ok=True)
                cleanup['source_tts_file_removed'] = True
            except OSError:
                cleanup['source_tts_file_removed'] = False
        # Close just the recorded synthetic actor; never clear shared state.
        # Keep correlation even when the earlier audio/queue assertion failed;
        # cleanup will remove this test identity's durable mapping afterwards.
        try:
            observed_state = state_for_device(config.DEERFLOW_STATE_PATH, dkey)
            if observed_state:
                report.setdefault('native_cursor', observed_state.get('native_cursor'))
                report.setdefault('voice_request_id', observed_state.get('request_id'))
                report.setdefault('waku_session_id', observed_state.get('session_id'))
                report.setdefault('conversation_state', observed_state.get('status'))
        except Exception:
            cleanup['correlation_capture_failed'] = True
        try:
            if args.role==1:
                cleanup_identity(config, dkey, cleanup,args.role)
            else:
                cleanup['no_device_or_actor_created']=True
        except Exception as error:
            cleanup['identity_cleanup_error_type'] = type(error).__name__
            report['status'] = 'failed'


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--run', action='store_true', help='Make real TTS/ASR/model calls.')
    parser.add_argument('--role', type=int,choices=range(1,8),default=1)
    parser.add_argument('--sample-rate',type=int,choices=SAMPLE_RATES,default=32000,
                        help='Device PCM rate; 32000 reproduces the actual toy protocol.')
    parser.add_argument('--text', default='请只回答测试通过，不要调用工具。')
    parser.add_argument('--audit-main-log',action='store_true',help='Offline audit of the existing synthetic voice report and filtered main-process log.')
    parser.add_argument('--log-file',type=Path)
    parser.add_argument('--namespace', default='folotoy', help='The running service PROJECT_NAME.')
    parser.add_argument('--udp-host', help='Defaults to SPEECH_UDP_SERVER_HOST.')
    parser.add_argument('--udp-port', type=int, help='Defaults to SPEECH_UDP_SERVER_PORT.')
    parser.add_argument('--timeout', type=float, default=180, help='Overall deadline, at most 240 seconds.')
    parser.add_argument('--report', type=Path)
    args = parser.parse_args(argv)
    if args.audit_main_log:
        if args.run or not args.report or not args.log_file:
            parser.error('--audit-main-log requires --report and --log-file and cannot use --run')
        try:
            report=audit_main_log(json.loads(args.report.read_text(encoding='utf-8')),
                                  args.log_file.read_text(encoding='utf-8'))
        except VoiceFailure as error:
            report={'passed':False,'status':'failed','role':1,'provider':PROVIDER,'failure_code':str(error)}
        args.report.write_text(json.dumps(report,ensure_ascii=False,indent=2)+'\n',encoding='utf-8')
        emit(report)
        return 0 if report['passed'] else 1
    if not args.run:
        emit({'status': 'not_run','passed':False,'role':args.role,'provider':PROVIDER,
              'input_sample_rate':args.sample_rate,
              'reason': 'Supply --run to make real voice-provider requests.'})
        return 0
    if not 0 < len(args.text.strip()) <= 200:
        parser.error('--text must contain between 1 and 200 characters')
    if not re.fullmatch(r'[A-Za-z0-9._-]{1,64}', args.namespace):
        parser.error('--namespace must be one MQTT topic segment')
    if not math.isfinite(args.timeout) or not 0 < args.timeout <= 240:
        parser.error('--timeout must be greater than zero and at most 240 seconds')
    if args.udp_port is not None and not 1 <= args.udp_port <= 65535:
        parser.error('--udp-port must be between 1 and 65535')
    report = {'status': 'running','passed':False,'role':args.role,'provider':PROVIDER,
              'input_sample_rate':args.sample_rate,'stages': []}
    started = time.monotonic()
    try:
        run(args, report)
    except VoiceFailure as error:
        report.update({'status': 'failed','passed':False,'failure_code': str(error)})
    except KeyboardInterrupt:
        report.update({'status': 'cancelled','passed':False})
    except Exception as error:
        report.update({'status': 'failed','passed':False,'error_type': type(error).__name__})
    finally:
        if report['status']=='failed':report['passed']=False
        report['elapsed_ms'] = round((time.monotonic() - started) * 1000)
        if args.report:
            try:
                args.report.parent.mkdir(parents=True, exist_ok=True)
                args.report.write_text(json.dumps(report, ensure_ascii=False, indent=2) + '\n',
                                       encoding='utf-8')
            except OSError:
                report.update({'status': 'failed','passed':False,'failure_code': 'report_write_failed'})
        emit(report)
    # Role1 exits successfully when its audio chain completed, but the report
    # remains passed=false until the host attaches the exact main ASR receipt.
    return 0 if report.get('passed') or report.get('pipeline_completed') else 1


if __name__ == '__main__':
    raise SystemExit(main())
