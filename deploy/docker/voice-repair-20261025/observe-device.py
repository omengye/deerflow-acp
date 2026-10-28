"""Observe one genuine device question without injecting input or commands."""
import argparse
import datetime
import json
import logging
from pathlib import Path
import secrets
import sqlite3
import sys
import threading
import time
from urllib.parse import urlsplit


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--timeout', type=int, default=210)
    parser.add_argument('--report', type=Path, required=True)
    args = parser.parse_args()
    logging.disable(logging.CRITICAL)
    sys.path.insert(0, '/opt/folotoy')
    from config import Config
    import paho.mqtt.client as mqtt
    config = Config()
    device = '48ca439d3b78'
    topic = '/user/%s/%s/thing/command/call' % (config.PROJECT_NAME, device)
    report = {'status': 'observing', 'device_suffix': device[-4:],
              'started_at': datetime.datetime.now(datetime.timezone.utc).isoformat(),
              'observer_published_commands': False, 'injected_input': False,
              'audio': [], 'terminal': [], 'token_updates_seen': 0}
    ready = threading.Event()
    lock = threading.Lock()
    completed_at = [None]
    def emit(value):
        print(json.dumps(value, ensure_ascii=False), flush=True)
    def save():
        args.report.parent.mkdir(parents=True, exist_ok=True)
        args.report.write_text(json.dumps(report, ensure_ascii=False, indent=2) + '\n')
        args.report.chmod(0o600)
    def connect(active, ud, flags, rc, properties):
        if rc.value == 0:
            active.subscribe(topic, qos=1)
    def subscribed(*unused):
        ready.set()
    def message(active, ud, packet):
        if packet.topic != topic or len(packet.payload) > 65536:
            return
        try:
            value = json.loads(packet.payload)
            params = value.get('inputParams') or {}
            with lock:
                if value.get('identifier') == 'updatetoken':
                    report['token_updates_seen'] += 1
                elif value.get('identifier') == 'audioplay':
                    path = urlsplit(params.get('url', '')).path
                    item = {'recording_id': params.get('recordingId'), 'order': params.get('order'),
                            'filename': Path(path).name, 'message_id': value.get('msgId')}
                    if item not in report['audio']:
                        report['audio'].append(item)
                        emit({'stage': 'real_device_audio_command', 'recording_id': item['recording_id'], 'order': item['order']})
                elif value.get('identifier') == 'audioplay_cmd':
                    report['terminal'].append({'recording_id': params.get('recordingId'),
                                               'total': params.get('total'), 'message_id': value.get('msgId')})
                    completed_at[0] = time.monotonic()
                    emit({'stage': 'real_device_audio_completion', 'total': params.get('total')})
                save()
        except Exception:
            report['observer_message_error'] = True
    broker = config.MQTT_CONFIG
    client = mqtt.Client(callback_api_version=mqtt.CallbackAPIVersion.VERSION2,
                         client_id='real-device-observer-' + secrets.token_hex(6))
    client.on_connect = connect
    client.on_subscribe = subscribed
    client.on_message = message
    client.username_pw_set(broker['username'], broker['password'])
    client.connect(broker['ip'], int(broker['port']), keepalive=30)
    client.loop_start()
    try:
        if not ready.wait(10):
            raise RuntimeError('observer MQTT subscription not ready')
        save()
        emit({'stage': 'real_device_observer_ready', 'timeout': args.timeout})
        deadline = time.monotonic() + args.timeout
        while time.monotonic() < deadline:
            if completed_at[0] and time.monotonic() - completed_at[0] > 12:
                break
            time.sleep(.2)
        location = Path(config.DEERFLOW_STATE_PATH).resolve()
        with sqlite3.connect(location.as_uri() + '?mode=ro', uri=True) as db:
            db.row_factory = sqlite3.Row
            row = db.execute('SELECT role,epoch,session_id,status,request_id,updated_at FROM deerflow_sessions WHERE device=? ORDER BY updated_at DESC LIMIT 1', (device,)).fetchone()
            report['conversation'] = dict(row) if row else None
            if row and row['session_id']:
                ws = db.execute('SELECT cursor,model,unconfirmed FROM deerflow_ws_sessions WHERE session_id=?', (row['session_id'],)).fetchone()
                report['ws'] = dict(ws) if ws else None
                if report['ws']:
                    report['ws']['cursor'] = json.loads(report['ws']['cursor']) if report['ws']['cursor'] else None
        report['status'] = 'observed_voice_reply' if report['audio'] and report['terminal'] else 'no_voice_reply_observed'
        report['requires_host_log_and_native_audit'] = True
        report['speaker_audibility_verified'] = False
        save()
        emit(report)
    finally:
        client.disconnect()
        client.loop_stop()


if __name__ == '__main__':
    main()
