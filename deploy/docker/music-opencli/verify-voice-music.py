"""Real ASR -> WS tools -> TTS/music queue validation on a synthetic device.

Run inside its Linux/Python 3.11 container with the service's environment.
--run makes paid TTS/ASR/model requests; without it no configuration or network
is accessed. Only a fresh synthetic device's exact MQTT topic is observed.
"""

import argparse
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
from urllib.parse import urlsplit
from urllib.request import ProxyHandler, Request, build_opener


PROJECT = Path(__file__).resolve().parents[1]


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


def cleanup_identity(config, dkey, cleanup):
    """Close only the recorded synthetic actor, using a separate local WS DB."""
    if not re.fullmatch(r'MUSICVOICE[0-9A-F]{16}', dkey):
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
    selected = dict(get_role_llm_config('deerflow', config.ROLES['1'], config))
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


def music_job_observation(filename, dkey, request_id, deadline):
    if not re.fullmatch(r'music-[0-9a-f]{32}\.mp3', filename):
        raise VoiceFailure('music_filename_invalid')
    config = json.loads(Path(os.environ['MUSIC_CONTROL_CONFIG']).read_text())
    host = config.get('listen_host', '127.0.0.1')
    if host == '0.0.0.0':
        host = '127.0.0.1'
    endpoint = 'http://%s:%s/v1/music/jobs/%s' % (
        host, config.get('listen_port',8090), filename[6:-4])
    opener = build_opener(ProxyHandler({}))
    end = min(deadline, time.monotonic()+3)
    while time.monotonic() < end:
        req = Request(endpoint, headers={'Authorization':'Bearer '+config['token']})
        with opener.open(req, timeout=remaining(deadline,3)) as response:
            value = json.load(response)
        job = value.get('job') or {}
        if job.get('device_key') != dkey or job.get('request_id') != request_id:
            raise VoiceFailure('music_job_does_not_belong_to_synthetic_voice_turn')
        if job.get('state')!='published':
            raise VoiceFailure('music_job_not_published')
        if job.get('device_audio_requested'):
            raise VoiceFailure('synthetic_music_was_requested_by_an_esp32_client')
        if job.get('audio_requested'):
            return {'id':job['id'],'state':job['state'],'device_key':dkey,
                    'request_id':request_id,'audio_requested':True,
                    'device_audio_requested':False,'delivery_confirmed':False}
        time.sleep(min(.1, remaining(deadline)))
    raise VoiceFailure('music_http_get_observation_missing')


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
    role = config.ROLES.get('1', {})
    if role.get('llm_type', config.LLM_TYPE) != 'deerflow':
        raise VoiceFailure('device_default_role_is_not_deerflow')
    if getattr(config, 'DEERFLOW_TRANSPORT', '') != 'ws':
        raise VoiceFailure('service_configuration_is_not_websocket')
    if config.STORAGE_TYPE != 'file':
        raise VoiceFailure('live_verifier_requires_local_audio_storage')
    broker = config.MQTT_CONFIG
    dkey = 'MUSICVOICE' + secrets.token_hex(8).upper()
    report['test_device'] = dkey
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
        conversion = subprocess.run([
            'ffmpeg', '-v', 'error', '-nostdin', '-i', str(source_file),
            '-ac', '1', '-ar', '16000', '-acodec', 'pcm_s16le', '-f', 's16le', 'pipe:1',
        ], capture_output=True, timeout=remaining(deadline, 20))
        pcm = conversion.stdout
        if conversion.returncode != 0 or not 3200 <= len(pcm) <= 640000 or len(pcm) % 2:
            raise VoiceFailure('source_pcm_conversion_failed')
        stage('source_pcm_ready', pcm_bytes=len(pcm), sample_rate=16000)

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
        header = token + recording_id.to_bytes(2, 'big')
        udp.sendto(header + b'\x02\x01', destination)
        time.sleep(min(0.1, remaining(deadline)))
        packets = 0
        for offset in range(0, len(pcm), 1280):
            packet = pcm[offset:offset + 1280]
            udp.sendto(header + b'\x04' + packet, destination)
            packets += 1
            time.sleep(min(len(packet) / 32000, remaining(deadline)))
        udp.sendto(header + b'\x02\x02', destination)
        uploaded_at = time.monotonic()
        stage('udp_recording_uploaded', audio_packets=packets, pcm_bytes=len(pcm))

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
        # completion, and the music must be the final URL in that same batch.
        time.sleep(min(.3, remaining(deadline)))
        music_orders = [order for order, url in observed_audio.items()
                        if Path(urlsplit(url).path).name.startswith('music-')
                        and Path(urlsplit(url).path).name.endswith('.mp3')]
        if (len(terminal_messages) != 1 or unexpected_standalone.is_set()
                or not music_orders or max(music_orders) != total or total < 2):
            raise VoiceFailure('music_was_not_appended_after_tts_in_one_recording_batch')
        report.update(music_queue_verified=True, terminal_count=len(terminal_messages),
                      music_orders=music_orders,
                      speech_segments=total-len(music_orders),
                      music_filename=Path(urlsplit(observed_audio[total]).path).name,
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
        report['music_job'] = music_job_observation(
            report['music_filename'], dkey, state['request_id'], deadline)
        report.update({'status': 'passed', 'audio_segments': total,
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
            cleanup_identity(config, dkey, cleanup)
        except Exception as error:
            cleanup['identity_cleanup_error_type'] = type(error).__name__
            report['status'] = 'failed'


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--run', action='store_true', help='Make real TTS/ASR/model calls.')
    parser.add_argument('--text', default='请播放本地验证音乐，只在本轮设备播放，不用默认设备，也不要下载新歌曲。')
    parser.add_argument('--namespace', default='folotoy', help='The running service PROJECT_NAME.')
    parser.add_argument('--udp-host', help='Defaults to SPEECH_UDP_SERVER_HOST.')
    parser.add_argument('--udp-port', type=int, help='Defaults to SPEECH_UDP_SERVER_PORT.')
    parser.add_argument('--timeout', type=float, default=180, help='Overall deadline, at most 240 seconds.')
    parser.add_argument('--report', type=Path)
    args = parser.parse_args(argv)
    if not args.run:
        emit({'status': 'not_run', 'reason': 'Supply --run to make real voice-provider requests.'})
        return 0
    if not 0 < len(args.text.strip()) <= 200:
        parser.error('--text must contain between 1 and 200 characters')
    if not re.fullmatch(r'[A-Za-z0-9._-]{1,64}', args.namespace):
        parser.error('--namespace must be one MQTT topic segment')
    if not math.isfinite(args.timeout) or not 0 < args.timeout <= 240:
        parser.error('--timeout must be greater than zero and at most 240 seconds')
    if args.udp_port is not None and not 1 <= args.udp_port <= 65535:
        parser.error('--udp-port must be between 1 and 65535')
    report = {'status': 'running', 'stages': []}
    started = time.monotonic()
    try:
        run(args, report)
    except VoiceFailure as error:
        report.update({'status': 'failed', 'failure_code': str(error)})
    except KeyboardInterrupt:
        report.update({'status': 'cancelled'})
    except Exception as error:
        report.update({'status': 'failed', 'error_type': type(error).__name__})
    finally:
        report['elapsed_ms'] = round((time.monotonic() - started) * 1000)
        if args.report:
            try:
                args.report.parent.mkdir(parents=True, exist_ok=True)
                args.report.write_text(json.dumps(report, ensure_ascii=False, indent=2) + '\n',
                                       encoding='utf-8')
            except OSError:
                report.update({'status': 'failed', 'failure_code': 'report_write_failed'})
        emit(report)
    return 0 if report['status'] == 'passed' else 1


if __name__ == '__main__':
    raise SystemExit(main())
