"""Check a full voice turn using a synthetic device token issued before restart.

--phase seed --run performs only MQTT login. It writes the token to a private
0600 file and never prints it. Recreate the FoloToy container, then run
--phase check --run: the original 32 kHz voice verifier is reused, with only
its synthetic identity and login block replaced. No login or token refresh is
published during check. Main-process ASR and native-run audits remain required.

SOURCE_FILES may inject verify-rate-voice.py and verify-voice-model.py. The
production token store and this test's private seed file are separate files.
"""
import argparse
from datetime import datetime, timezone
import json
import logging
import os
from pathlib import Path
import queue
import re
import secrets
import socket
import stat
import sys
import threading
import time


DEFAULT_MODEL = 'gemini-3.8-flash-high'
DEFAULT_SEED = Path('/data/private/voice-restart-test.json')
BASE_NAME = 'verify-rate-voice.py'
MODEL_NAME = 'verify-voice-model.py'
DEVICE_PATTERN = r'ASR2TEST[0-9A-F]{16}'


class RestartFailure(Exception):
    """Only fixed diagnostics are safe to emit."""


def emit(value):
    print(json.dumps(value, ensure_ascii=False), flush=True)


def source_text(name):
    injected = globals().get('SOURCE_FILES')
    if injected is not None:
        if not isinstance(injected, dict) or not isinstance(injected.get(name), str):
            raise RestartFailure('reviewed_verifier_source_missing')
        return injected[name]
    directory = Path(globals().get('__file__', str(Path.cwd() / 'verify-restart-voice.py'))).resolve().parent.parent
    folder = 'asr2-audiofix-20261025' if name == BASE_NAME else 'llm-switch-20261025'
    return (directory / folder / name).read_text(encoding='utf-8-sig')


def container_marker():
    """Both container identity and PID 1 start time survive diagnostic execs."""
    try:
        tail = Path('/proc/1/stat').read_text().rsplit(')', 1)[1].split()
        ticks = tail[19]  # /proc stat field 22; tail starts with field 3.
        boot_id = Path('/proc/sys/kernel/random/boot_id').read_text().strip()
    except (OSError, IndexError):
        raise RestartFailure('linux_container_start_marker_unavailable') from None
    if not ticks.isdigit() or not re.fullmatch(r'[0-9a-f-]{36}', boot_id):
        raise RestartFailure('linux_container_start_marker_invalid')
    return {'hostname': socket.gethostname(), 'boot_id': boot_id, 'pid1_start_ticks': ticks}


def private_seed(path):
    if path.is_symlink():
        raise RestartFailure('seed_file_must_not_be_symlink')
    info = path.stat()
    if info.st_uid != os.geteuid() or stat.S_IMODE(info.st_mode) & 0o077:
        raise RestartFailure('seed_file_owner_or_permissions_invalid')
    data = json.loads(path.read_text(encoding='utf-8'))
    if (not isinstance(data, dict) or data.get('kind') != 'folotoy-synthetic-restart-seed'
            or data.get('version') != 1
            or not re.fullmatch(DEVICE_PATTERN, str(data.get('device', '')))
            or not re.fullmatch(r'[0-9a-f]{32}', str(data.get('token', '')))
            or not isinstance(data.get('container_start_marker'), dict)
            or not isinstance(data.get('issued_at'), str)):
        raise RestartFailure('private_seed_format_invalid')
    return data


def install_source_path():
    for source in (Path.cwd(), Path('/opt/folotoy')):
        if (source / 'config.py').is_file():
            sys.path.insert(0, str(source))
            return
    raise RestartFailure('folotoy_source_not_found')


def seed(args):
    logging.disable(logging.CRITICAL)
    if args.seed_file.exists() or args.seed_file.is_symlink():
        raise RestartFailure('private_seed_already_exists_use_another_test_file')
    marker = container_marker()
    install_source_path()
    from config import Config
    import paho.mqtt.client as mqtt
    config = Config()
    broker = config.MQTT_CONFIG
    dkey = 'ASR2TEST' + secrets.token_hex(8).upper()
    topic = '/user/%s/%s/thing/command/call' % (args.namespace, dkey)
    event_topic = '/user/%s/%s/thing/event/post' % (args.namespace, dkey)
    connected = threading.Event()
    subscribed = threading.Event()
    rejected = threading.Event()
    messages = queue.Queue(maxsize=8)
    client = None
    loop_started = False
    deadline = time.monotonic() + min(args.timeout, 45)

    def remaining(maximum=10):
        value = deadline - time.monotonic()
        if value <= 0:
            raise RestartFailure('seed_mqtt_deadline_exceeded')
        return min(value, maximum)

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
            if isinstance(value, dict) and value.get('identifier') == 'updatetoken':
                messages.put_nowait(value)
        except (ValueError, UnicodeError, queue.Full):
            pass

    try:
        client = mqtt.Client(callback_api_version=mqtt.CallbackAPIVersion.VERSION2,
                             client_id='folotoy-restart-seed-' + secrets.token_hex(8))
        client.suppress_exceptions = True
        client.on_connect = on_connect
        client.on_subscribe = on_subscribe
        client.on_message = on_message
        client.username_pw_set(broker['username'], broker['password'])
        client.connect_timeout = remaining()
        client.connect(host=broker['ip'], port=int(broker['port']), keepalive=30)
        client.loop_start()
        loop_started = True
        if not connected.wait(remaining()) or rejected.is_set():
            raise RestartFailure('seed_mqtt_connection_failed')
        if not subscribed.wait(remaining()) or rejected.is_set():
            raise RestartFailure('seed_mqtt_subscription_failed')
        published = client.publish(event_topic, json.dumps({'identifier': 'login', 'inputParams': {}}), qos=1)
        published.wait_for_publish(timeout=remaining(5))
        if not published.is_published():
            raise RestartFailure('seed_login_publish_unconfirmed')
        try:
            login = messages.get(timeout=remaining())
        except queue.Empty:
            raise RestartFailure('seed_token_not_received') from None
        token_hex = str((login.get('inputParams') or {}).get('token', '')).lower()
        if not re.fullmatch(r'[0-9a-f]{32}', token_hex):
            raise RestartFailure('seed_token_invalid')
        record = {'kind': 'folotoy-synthetic-restart-seed', 'version': 1,
                  'device': dkey, 'token': token_hex, 'namespace': args.namespace,
                  'issued_at': datetime.now(timezone.utc).isoformat(),
                  'container_start_marker': marker}
        args.seed_file.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
        if args.seed_file.parent.is_symlink():
            raise RestartFailure('seed_private_directory_must_not_be_symlink')
        args.seed_file.parent.chmod(0o700)
        descriptor = os.open(args.seed_file, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(descriptor, 'w', encoding='utf-8') as stream:
            json.dump(record, stream)
            stream.write('\n')
            stream.flush()
            os.fsync(stream.fileno())
        return {'status': 'seeded', 'passed': True, 'phase': 'seed', 'test_device': dkey,
                'issued_at': record['issued_at'], 'container_start_marker': marker,
                'seed_file': str(args.seed_file), 'seed_file_mode': '0600',
                'login_published': True, 'token_refresh_published': False,
                'llm_request_submitted': False, 'full_voice_pipeline_tested': False,
                'physical_toy_audio_published': False}
    finally:
        if client is not None:
            try:
                client.disconnect()
            finally:
                if loop_started:
                    client.loop_stop()


def patched_voice_source():
    source = source_text(BASE_NAME)
    identity = "    dkey = 'ASR2TEST' + secrets.token_hex(8).upper()\n"
    login = """        publish('login')
        login = wait_message(messages, min(deadline, time.monotonic() + 10),
                             lambda message: message.get('identifier') == 'updatetoken')
        token_hex = str((login.get('inputParams') or {}).get('token', ''))
        if not re.fullmatch(r'[0-9a-fA-F]{32}', token_hex):
            raise VoiceFailure('device_login_token_invalid')
        token = bytes.fromhex(token_hex)
        stage('mqtt_device_logged_in')
"""
    guard = '    def publish(identifier):\n'
    if source.count(identity) != 1 or source.count(login) != 1 or source.count(guard) != 1:
        raise RestartFailure('original_voice_source_anchor_changed')
    source = source.replace(identity, "    dkey = RESTART_SEED['device']\n")
    source = source.replace(login, """        token = bytes.fromhex(RESTART_SEED['token'])
        report.update(device_token_reused_after_restart=True, login_published=False,
                      token_refresh_published=False)
        stage('mqtt_subscribed_without_login_stored_token_reused')
""")
    source = source.replace(guard, guard + "        if identifier in ('login', 'updatetoken'):\n            raise VoiceFailure('restart_check_must_not_publish_login_or_token_refresh')\n")
    if "publish('login')" in source:
        raise RestartFailure('restart_source_still_contains_login_publish')
    return source


def restart_evidence_valid(report):
    return bool(report.get('phase') == 'check'
                and report.get('container_restart_verified') is True
                and report.get('device_token_reused_after_restart') is True
                and report.get('login_published') is False
                and report.get('token_refresh_published') is False
                and report.get('input_sample_rate') == 32000
                and report.get('role') == 1
                and report.get('seed_test_device') == report.get('test_device')
                and re.fullmatch(DEVICE_PATTERN, str(report.get('test_device', ''))))


def load_verifier(expected_model=DEFAULT_MODEL, seed_record=None):
    """Return the original verifier with both model and restart evidence gates.

    Parent-side offline auditing may call load_verifier()['audit_main_log'].
    It does not read the private seed file or open MQTT/network connections.
    """
    modified_source = patched_voice_source()
    wrapper = {'__name__': '_voice_model_for_restart',
               '__file__': str(Path(globals().get('__file__', '/tmp/verify-restart-voice.py'))),
               'SOURCE_FILES': {BASE_NAME: modified_source}}
    exec(compile(source_text(MODEL_NAME), MODEL_NAME, 'exec'), wrapper)
    base = wrapper['add_model_checks'](wrapper['load_base'](), expected_model)
    base['RESTART_SEED'] = seed_record
    original_run = base['run']
    original_audit = base['audit_main_log']

    def run_with_restart(args, report):
        if not seed_record:
            raise base['VoiceFailure']('private_restart_seed_not_loaded')
        if args.role != 1 or args.sample_rate != 32000:
            raise base['VoiceFailure']('restart_check_requires_role1_32000hz')
        current = container_marker()
        restarted = current != seed_record['container_start_marker']
        report.update(phase='check', seed_test_device=seed_record['device'],
                      seed_issued_at=seed_record['issued_at'],
                      seed_container_start_marker=seed_record['container_start_marker'],
                      check_container_start_marker=current,
                      container_restart_verified=restarted, login_published=False,
                      token_refresh_published=False, device_token_reused_after_restart=False,
                      requires_native_audit=True)
        if not restarted:
            raise base['VoiceFailure']('container_was_not_restarted_after_seed')
        original_run(args, report)
        if not restart_evidence_valid(report):
            raise base['VoiceFailure']('restart_token_reuse_evidence_incomplete')
        if 'cleanup' in report:
            report['cleanup']['transient_device_note'] = ('Synthetic device login is retained in the server session store; '
                                                         'no real device was changed.')

    def audit_with_restart(report, log_text):
        result = original_audit(report, log_text)
        if not restart_evidence_valid(result):
            result.update(status='failed', passed=False,
                          failure_code='restart_token_reuse_evidence_incomplete')
        return result

    base.update(run=run_with_restart, audit_main_log=audit_with_restart)
    return base


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--phase', choices=('seed', 'check'), default='check')
    parser.add_argument('--seed-file', type=Path, default=DEFAULT_SEED)
    parser.add_argument('--expected-model', default=DEFAULT_MODEL)
    parser.add_argument('--run', action='store_true')
    parser.add_argument('--audit-main-log', action='store_true')
    parser.add_argument('--log-file', type=Path)
    parser.add_argument('--report', type=Path)
    parser.add_argument('--timeout', type=float, default=230)
    parser.add_argument('--namespace', default='folotoy')
    parser.add_argument('--role', type=int, default=1)
    parser.add_argument('--sample-rate', type=int, default=32000)
    parser.add_argument('--text', default='帮我查询明天北京的天气')
    args, forwarded = parser.parse_known_args(argv)
    if not re.fullmatch(r'[A-Za-z0-9_.:/-]{1,160}', args.expected_model):
        parser.error('expected-model must be a public model identifier')
    if not re.fullmatch(r'[A-Za-z0-9._-]{1,64}', args.namespace):
        parser.error('namespace must be one MQTT topic segment')
    if not 0 < args.timeout <= 240 or args.role != 1 or args.sample_rate != 32000:
        parser.error('requires role1, sample-rate32000, and timeout1..240 seconds')
    if args.run and args.audit_main_log:
        parser.error('--run and --audit-main-log are mutually exclusive')
    if not args.run and not args.audit_main_log:
        emit({'status': 'not_run', 'passed': False, 'phase': args.phase,
              'reason': 'Supply --run; use seed before recreation and check afterwards.'})
        return 0
    try:
        if args.phase == 'seed':
            if args.audit_main_log or forwarded:
                parser.error('seed accepts only its documented arguments')
            report = seed(args)
            if args.report:
                args.report.parent.mkdir(parents=True, exist_ok=True)
                args.report.write_text(json.dumps(report, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
            emit(report)
            return 0
        record = private_seed(args.seed_file) if args.run else None
        if record is not None and record.get('namespace') != args.namespace:
            raise RestartFailure('seed_namespace_mismatch')
        base = load_verifier(args.expected_model, record)
        if args.audit_main_log:
            if not args.report or not args.log_file:
                parser.error('--audit-main-log requires --report and --log-file')
            result = base['audit_main_log'](json.loads(args.report.read_text(encoding='utf-8')),
                                            args.log_file.read_text(encoding='utf-8'))
            args.report.write_text(json.dumps(result, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
            emit(result)
            return 0 if result.get('passed') else 1
        cli = ['--run', '--role', '1', '--sample-rate', '32000', '--text', args.text,
               '--timeout', str(args.timeout), '--namespace', args.namespace, *forwarded]
        if args.report:
            cli += ['--report', str(args.report)]
        final = [None]
        original_emit = base['emit']

        def capture(value):
            if isinstance(value, dict) and 'status' in value:
                final[0] = value
            original_emit(value)

        base['emit'] = capture
        code = base['main'](cli)
        return 1 if final[0] is not None and final[0].get('status') == 'failed' else code
    except RestartFailure as error:
        failure = {'status': 'failed', 'passed': False, 'phase': args.phase, 'failure_code': str(error)}
    except Exception as error:
        failure = {'status': 'failed', 'passed': False, 'phase': args.phase, 'error_type': type(error).__name__}
    if args.report:
        args.report.parent.mkdir(parents=True, exist_ok=True)
        args.report.write_text(json.dumps(failure, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
    emit(failure)
    return 1


if __name__ == '__main__':
    raise SystemExit(main())
