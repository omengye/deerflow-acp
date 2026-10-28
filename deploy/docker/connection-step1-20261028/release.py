"""Scoped Gentoo release; only replace the image, never restore business data."""
import argparse
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import time
import uuid
import websocket

ROOT = Path('/home/gen/deerflow-deploy')
TASK = ROOT / 'connection-step1-20261028'
IMAGE = 'deerflow-server:connection-step1-20261028'
BASE_ID = 'sha256:d89395c9629b359a3c8a8688f9a0f0e2145ff32a4c67f30a99b5226c045ab26f'
CONTAINER = 'deerflow-deploy-deerflow-1'
SMOKE = 'deerflow-connection-step1-test'
NIL = str(uuid.UUID(int=0))


def require(value, reason):
    if not value:
        raise RuntimeError(reason)


def run(args, *, payload=None, env=None, timeout=120):
    result = subprocess.run(args, input=payload, text=True, capture_output=True, env=env, timeout=timeout)
    require(result.returncode == 0, 'command_failed:' + args[0])
    return result.stdout


def save(name, value):
    (TASK / name).write_text(json.dumps(value, indent=2) + '\n')


def inspect():
    return json.loads(run(['docker', 'inspect', CONTAINER]))[0]


def image_id(image):
    value = run(['docker', 'image', 'inspect', '--format', '{{.Id}}', image]).strip()
    require(re.fullmatch(r'sha256:[a-f0-9]{64}', value), 'invalid_image_identity')
    return value


def token():
    return next(item.split('=', 1)[1] for item in inspect()['Config']['Env'] if item.startswith('WAKU_DAEMON_TOKEN='))


def handshake(url, capability, delay=0, expected='hello', rpc=False):
    started = time.monotonic()
    ws = websocket.create_connection(url, timeout=35, suppress_origin=True, http_no_proxy=['*'])
    try:
        time.sleep(delay)
        try:
            ws.send(json.dumps({'type': 'hello', 'protocolVersion': 10, 'token': capability,
                                'clientId': str(uuid.uuid4()), 'resumeFrom': []}))
            raw = ws.recv()
            kind = json.loads(raw)['type'] if raw else 'closed'
        except (websocket.WebSocketConnectionClosedException, BrokenPipeError, ConnectionResetError):
            kind = 'closed'
        require(kind == expected, 'unexpected_handshake_result')
        checks = {}
        if rpc:
            for command, response in [('getSettings', 'settings'), ('loadTaskState', 'taskState')]:
                request_id = str(uuid.uuid4())
                ws.send(json.dumps({'type': 'request', 'requestId': request_id, 'sessionId': NIL,
                                    'runtimeId': NIL, 'command': {'type': command}}))
                deadline = time.monotonic() + 35
                while True:
                    remaining = deadline - time.monotonic()
                    require(remaining > 0, 'rpc_timeout')
                    ws.settimeout(remaining)
                    message = json.loads(ws.recv())
                    if message.get('type') == 'response' and message.get('requestId') == request_id:
                        outcome = message.get('outcome', {})
                        require(outcome.get('status') == 'ok', 'rpc_failed')
                        require(outcome.get('payload', {}).get('type') == response, 'rpc_contract_changed')
                        checks[command] = True
                        break
        return {'delay_seconds': delay, 'result': kind, 'elapsed_ms': round((time.monotonic() - started) * 1000), 'rpc': checks}
    finally:
        ws.close()


def smoke():
    capability = secrets.token_urlsafe(32)
    candidate = image_id(IMAGE)
    env = dict(os.environ, WAKU_DAEMON_TOKEN=capability)
    run(['docker', 'run', '-d', '--rm', '--name', SMOKE, '--no-healthcheck',
         '-p', '127.0.0.1:18788:18787', '-e', 'WAKU_DAEMON_TOKEN',
         '--entrypoint', '/opt/deerflow/bin/waku-daemon', candidate,
         '--bind', '0.0.0.0:18787', '--allow-non-loopback'], env=env)
    try:
        url = 'ws://127.0.0.1:18788/v1'
        for attempt in range(30):
            try:
                normal = handshake(url, capability, rpc=True)
                break
            except (ConnectionRefusedError, websocket.WebSocketConnectionClosedException):
                if attempt == 29:
                    raise
                time.sleep(0.2)
        results = [normal]
        for delay in (6, 10):
            results.append(handshake(url, capability, delay, rpc=True))
        results.append(handshake(url, 'invalid-capability', expected='rejected'))
        results.append(handshake(url, capability, 21, expected='closed'))
        report = {'passed': True, 'image_id': candidate, 'protocol': 10, 'results': results}
        save('smoke.json', report)
        print(json.dumps(report), flush=True)
    finally:
        run(['docker', 'stop', SMOKE])


def compose(*args):
    return ['docker', 'compose', '--env-file', str(ROOT / '.env'), '--project-directory', str(ROOT),
            '-p', 'deerflow-deploy', '-f', str(ROOT / 'compose.yaml'), *args]


def control(operation):
    raw = run(['docker', 'exec', '-i', CONTAINER, '/var/lib/deerflow/deerflow-control-go',
               '--runtime-dir', '/var/lib/deerflow/user-data/runtime/acp-go'], payload=json.dumps({'operation': operation}))
    result = json.loads(raw)
    require(result.get('ok'), 'native_control_failed')
    return result['data']


def atomic_env(data):
    path = ROOT / '.env.connection-step1-tmp'
    # Remove only the temporary file this attempt created; an existing file
    # belongs to another attempt and must not be overwritten or removed.
    created = False
    try:
        with path.open('xb') as handle:
            created = True
            path.chmod(0o600)
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(path, ROOT / '.env')
    finally:
        if created:
            path.unlink(missing_ok=True)


def healthy():
    for _ in range(40):
        state = inspect()['State']
        if state.get('Health', {}).get('Status') == 'healthy':
            return
        require(state['Status'] not in ('dead', 'exited'), 'container_exited')
        time.sleep(2)
    raise RuntimeError('container_not_healthy')


def verify():
    healthy()
    capability = token()
    results = [handshake('ws://100.80.205.120:18787/v1', capability, rpc=True),
               handshake('wss://deerflow.sprwhisp.cc/v1', capability, rpc=True)]
    for delay in (6, 10):
        results.append(handshake('wss://deerflow.sprwhisp.cc/v1', capability, delay, rpc=True))
    native = control('daemon.status')
    report = {'passed': True, 'image': inspect()['Config']['Image'], 'protocol': 10,
              'health': 'healthy', 'results': results,
              'active_runs': native.get('active_runs'), 'queued_runs': native.get('queued_runs')}
    save('production.json', report)
    print(json.dumps(report), flush=True)


def apply():
    evidence = json.loads((TASK / 'smoke.json').read_text())
    require(evidence.get('passed') is True and evidence.get('protocol') == 10, 'smoke_required')
    candidate = image_id(IMAGE)
    require(evidence.get('image_id') == candidate, 'smoke_image_changed')
    require(inspect()['Image'] == BASE_ID, 'deployment_baseline_changed')
    old_env = (ROOT / '.env').read_bytes()
    # Deploy the tested immutable image, even if its convenience tag changes.
    text, count = re.subn(r'(?m)^DEERFLOW_IMAGE=.*$', 'DEERFLOW_IMAGE=' + candidate, old_env.decode())
    require(count == 1, 'image_setting_ambiguous')
    new_env = text.encode()
    state = control('daemon.status')
    require(all(state.get(key) == 0 for key in ('active_operations', 'active_runs', 'queued_runs')),
            'active_work_wait_required')
    require(state.get('draining') is False, 'deployment_already_drained')
    changed = False
    rollback_allowed = True
    try:
        # The request can reach the daemon even if its response is lost, so
        # resume belongs in finally before we attempt the drain operation.
        drained = control('daemon.drain')
        require(drained.get('draining') is True and
                all(drained.get(key) == 0 for key in ('active_operations', 'active_runs', 'queued_runs')),
                'active_work_started')
        require((ROOT / '.env').read_bytes() == old_env, 'configuration_changed')
        backup = TASK / 'previous.env'
        try:
            with backup.open('xb') as handle:
                backup.chmod(0o600)
                handle.write(old_env)
                handle.flush()
                os.fsync(handle.fileno())
        except FileExistsError:
            # A failed release may have rolled back already. Reuse its exact
            # original configuration without replacing the recovery evidence.
            require(backup.is_file() and not backup.is_symlink() and backup.read_bytes() == old_env,
                    'previous_configuration_backup_changed')
        atomic_env(new_env)
        changed = True
        run(compose('up', '-d', '--no-deps', 'deerflow'))
        healthy()
        require(inspect()['Image'] == candidate, 'deployed_image_changed')
        # The inherited entrypoint starts a fresh native daemon; its drain
        # state is in memory, so reapply it before the read-only verification.
        drained = control('daemon.drain')
        rollback_allowed = all(drained.get(key) == 0
                               for key in ('active_operations', 'active_runs', 'queued_runs'))
        require(drained.get('draining') is True and rollback_allowed,
                'active_work_started_after_restart')
        verify()
    except Exception:
        if changed and rollback_allowed:
            atomic_env(old_env)
            run(compose('up', '-d', '--no-deps', 'deerflow'))
            healthy()
        elif changed:
            # A client may submit during the brief fresh-daemon startup gap.
            # Preserve that run rather than restarting it for a rollback.
            save('deployment.json', {'passed': False, 'verification_pending': True,
                                     'image_id': candidate, 'business_data_restored': False,
                                     'reason': 'active_work_started_after_restart'})
        raise
    finally:
        control('daemon.resume')
    save('deployment.json', {'passed': True, 'image': IMAGE, 'image_id': candidate, 'previous_image_id': BASE_ID,
                             'business_data_restored': False, 'protocol': 10})


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('action', choices=['smoke', 'apply', 'verify'])
    args = parser.parse_args()
    try:
        globals()[args.action]()
    except Exception as error:
        print(json.dumps({'passed': False, 'error': str(error) if isinstance(error, RuntimeError) else type(error).__name__}), flush=True)
        raise SystemExit(1)
