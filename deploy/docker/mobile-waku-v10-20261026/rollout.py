"""Backed-up Gentoo rollout for Waku v10 and its FoloToy protocol adapter.

Run --check, then --apply. --rollback restores configuration and old images only.
Business data archives are deliberately never restored automatically.
No credentials, Docker environment values, model calls or prompts are printed.
"""

import argparse
import copy
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile
import time
import uuid


ROOT = Path('/home/gen/deerflow-deploy/mobile-waku-v10-20261026')
BACKUP = ROOT / 'backup'
DEER_ROOT = Path('/home/gen/deerflow-deploy')
FOLO_ROOT = Path('/home/gen/Documents/folotoy/deerflow-ws/src')
DEER = 'deerflow-deploy-deerflow-1'
FOLO = 'folotoy-ws-folotoy-1'
DEER_IMAGE = 'deerflow-server:mobile-waku-v10-20261026'
FOLO_IMAGE = 'folotoy-deerflow-ws:mobile-waku-v10-20261026'
SOURCE_SHA256 = 'f2f95f58d7a2a89f208052cdac8b51977d0f00c00f4a5300149d1787bab8fe2d'
LAN = '192.168.1.190'
TAIL = '100.80.205.120'
BACKUP_FILES = {
    DEER_ROOT / 'compose.yaml': 'deerflow-compose.yaml',
    DEER_ROOT / '.env': 'deerflow.env',
    DEER_ROOT / 'runtime/deerflow-acpd-wrapper.sh': 'deerflow-acpd-wrapper.sh',
    FOLO_ROOT / 'compose.ws.yaml': 'folotoy-compose.yaml',
    FOLO_ROOT / '.env.ws': 'folotoy.env',
    FOLO_ROOT / 'sdk/deerflow_ws_client.py': 'deerflow_ws_client.py',
}
CHANGED_PATHS = (
    DEER_ROOT / '.env', DEER_ROOT / 'compose.yaml',
    FOLO_ROOT / 'compose.ws.yaml', FOLO_ROOT / 'sdk/deerflow_ws_client.py',
)
BUSY = {'connecting', 'working', 'waiting', 'background', 'running'}


class RolloutError(Exception):
    pass


def require(condition, message):
    if not condition:
        raise RolloutError(message)


def digest(data):
    return hashlib.sha256(data).hexdigest()


def read_file(path):
    require(path.is_file() and not path.is_symlink(), 'Missing or symlinked deployment file: ' + path.name)
    return path.read_bytes()


def command(argv, *, payload=None, timeout=120, environment=None):
    env = dict(os.environ)
    for key in ('DEERFLOW_IMAGE', 'FOLOTOY_WS_IMAGE', 'COMPOSE_FILE', 'COMPOSE_PROJECT_NAME', 'COMPOSE_PROFILES'):
        env.pop(key, None)
    env.update(environment or {})
    try:
        result = subprocess.run(argv, input=payload, text=True, capture_output=True,
                                timeout=timeout, env=env)
    except (subprocess.TimeoutExpired, OSError):
        raise RolloutError('Deployment command failed or timed out: ' + argv[0]) from None
    require(result.returncode == 0, 'Deployment command failed: ' + argv[0] + ' (exit ' + str(result.returncode) + ')')
    return result.stdout


def atomic_write(path, data, mode):
    require(not path.is_symlink(), 'Refusing a symlinked deployment target')
    temporary = path.with_name('.' + path.name + '.' + uuid.uuid4().hex + '.tmp')
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, mode)
    try:
        with os.fdopen(fd, 'wb') as output:
            output.write(data)
            output.flush()
            os.fsync(output.fileno())
        temporary.chmod(mode)
        os.replace(temporary, path)
        directory = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        temporary.unlink(missing_ok=True)


def compose(service, *, candidate=False):
    if service == 'deerflow':
        directory, env, project, filename = DEER_ROOT, '.env', 'deerflow-deploy', 'compose.yaml'
    else:
        directory, env, project, filename = FOLO_ROOT, '.env.ws', 'folotoy-ws', 'compose.ws.yaml'
    return ['docker', 'compose', '--env-file', str(directory / env), '-p', project,
            '--project-directory', str(directory), '-f', '-' if candidate else str(directory / filename)]


def compose_config(service, raw=None, image=None):
    value = command(compose(service, candidate=raw is not None) + ['config', '--format', 'json'],
                    payload=raw.decode('utf-8') if raw is not None else None,
                    environment={'DEERFLOW_IMAGE': image} if image else None)
    return json.loads(value)


def service_range(lines, name):
    matches = [i for i, line in enumerate(lines)
               if re.fullmatch(r' +'+re.escape(name)+r':\s*(?:#.*)?', line.rstrip('\r\n'))]
    require(len(matches) == 1, 'Expected exactly one service block')
    start = matches[0]
    indent = len(lines[start]) - len(lines[start].lstrip(' '))
    end = len(lines)
    for i in range(start + 1, len(lines)):
        stripped = lines[i].strip()
        if stripped and not stripped.startswith('#') and len(lines[i]) - len(lines[i].lstrip(' ')) <= indent:
            end = i
            break
    return start, end


def add_tail_port(raw):
    lines = raw.decode('utf-8').splitlines(keepends=True)
    start, end = service_range(lines, 'deerflow')
    keys = [i for i in range(start + 1, end) if lines[i].strip() == 'ports:']
    require(len(keys) == 1, 'Expected a single block-style ports field')
    key = keys[0]
    item = key + 1
    require(item < end and lines[item].lstrip().startswith('- '), 'Expected short-form port entries')
    indent = lines[item][:len(lines[item]) - len(lines[item].lstrip(' '))]
    finish = item
    while finish < end and lines[finish].startswith(indent + '- '):
        require('target:' not in lines[finish], 'Long-form port entries require review')
        require(TAIL not in lines[finish], 'Tailscale mapping is already present')
        finish += 1
    newline = '\r\n' if lines[item].endswith('\r\n') else '\n'
    require(lines[finish - 1].endswith('\n'), 'Port list must end with a newline')
    lines.insert(finish, indent + '- "' + TAIL + ':18787:18787"' + newline)
    return ''.join(lines).encode('utf-8')


def update_image_env(raw, image):
    pattern = re.compile(rb'(?m)^([ \t]*(?:export[ \t]+)?DEERFLOW_IMAGE[ \t]*=[ \t]*)("[^"\r\n]*"|\'[^\'\r\n]*\'|[^ \t#\r\n]+)([ \t]*(?:#[^\r\n]*)?)(\r?)$')
    matches = list(pattern.finditer(raw))
    require(len(matches) == 1, 'Expected exactly one DEERFLOW_IMAGE assignment')
    def replacement(match):
        quote = match[2][:1] if match[2][:1] in (b'"', b"'") else b''
        return match[1] + quote + image.encode('ascii') + quote + match[3] + match[4]
    return pattern.sub(replacement, raw)


def update_literal_image(raw, old, new):
    lines = raw.decode('utf-8').splitlines(keepends=True)
    start, end = service_range(lines, 'folotoy')
    matches = [i for i in range(start + 1, end) if re.match(r'^ +image:', lines[i])]
    require(len(matches) == 1, 'Expected one literal FoloToy image field')
    i = matches[0]
    pattern = re.compile(r'^( +image:\s*)(["\']?)' + re.escape(old) + r'\2([ \t]*(?:#[^\r\n]*)?)(\r?\n)?$')
    match = pattern.fullmatch(lines[i])
    require(match is not None, 'FoloToy image differs from the reviewed literal')
    lines[i] = match[1] + match[2] + new + match[2] + match[3] + (match[4] or '')
    return ''.join(lines).encode('utf-8')


def inspect_container(name):
    return json.loads(command(['docker', 'inspect', name]))[0]


def safe_container(name):
    item = inspect_container(name)
    return {'id': item['Id'], 'image': item['Config']['Image'], 'imageId': item['Image'],
            'status': item['State']['Status'], 'health': item['State'].get('Health', {}).get('Status'),
            'startedAt': item['State'].get('StartedAt')}


def verify_images(preflight):
    image_ids = {}
    for name, target in ((DEER, DEER_IMAGE), (FOLO, FOLO_IMAGE)):
        old = json.loads(command(['docker', 'image', 'inspect', preflight[name]['imageId']]))[0]
        new = json.loads(command(['docker', 'image', 'inspect', target]))[0]
        require(old['Id'] == preflight[name]['imageId'], 'Original image identity changed')
        require(new['Id'] != old['Id'], 'Target image is still the original image')
        require((old['Os'], old['Architecture']) == (new['Os'], new['Architecture']), 'Target platform differs')
        for key in ('User', 'Entrypoint', 'Cmd', 'WorkingDir', 'Env', 'Healthcheck'):
            require(old['Config'].get(key) == new['Config'].get(key), 'Target image changed inherited runtime configuration: ' + key)
        image_ids[name] = new['Id']
    return image_ids


def load_preflight(*, current=True):
    preflight = json.loads(read_file(ROOT / 'preflight.json'))
    require(preflight.get('backup') == str(BACKUP), 'Preflight backup location differs')
    hashes = preflight.get('configHashes', {})
    require(set(map(str, BACKUP_FILES)) <= set(hashes), 'Preflight lacks required configuration hashes')
    for path, backup_name in BACKUP_FILES.items():
        expected = hashes[str(path)]
        require(digest(read_file(BACKUP / backup_name)) == expected, 'Original configuration backup checksum differs')
    if current:
        for name, expected in hashes.items():
            require(digest(read_file(Path(name))) == expected, 'A preflight file changed; refresh the deployment review')
        for name in (DEER, FOLO):
            item = inspect_container(name)
            require(item['Id'] == preflight[name]['id'] and item['Image'] == preflight[name]['imageId'], 'Container changed since preflight')
            require(item['State']['Running'] and item['State'].get('Health', {}).get('Status') == 'healthy', 'Original container is not healthy')
            tagged = json.loads(command(['docker', 'image', 'inspect', preflight[name]['image']]))[0]
            require(tagged['Id'] == preflight[name]['imageId'], 'Rollback image tag changed')
    return preflight


def prepare_changes(preflight):
    originals = {path: read_file(path) for path in CHANGED_PATHS}
    source = read_file(ROOT / 'folotoy/sdk/deerflow_ws_client.py')
    require(digest(source) == SOURCE_SHA256, 'Reviewed FoloToy payload checksum differs')
    candidates = {
        DEER_ROOT / '.env': update_image_env(originals[DEER_ROOT / '.env'], DEER_IMAGE),
        DEER_ROOT / 'compose.yaml': add_tail_port(originals[DEER_ROOT / 'compose.yaml']),
        FOLO_ROOT / 'compose.ws.yaml': update_literal_image(originals[FOLO_ROOT / 'compose.ws.yaml'], preflight[FOLO]['image'], FOLO_IMAGE),
        FOLO_ROOT / 'sdk/deerflow_ws_client.py': source,
    }
    for service, image, filename in (('deerflow', DEER_IMAGE, DEER_ROOT / 'compose.yaml'),
                                     ('folotoy', FOLO_IMAGE, FOLO_ROOT / 'compose.ws.yaml')):
        before = compose_config(service)
        after = compose_config(service, candidates[filename], DEER_IMAGE if service == 'deerflow' else None)
        expected = copy.deepcopy(before)
        expected['services'][service]['image'] = image
        if service == 'deerflow':
            ports = expected['services'][service]['ports']
            require(any(p.get('host_ip') == LAN and str(p.get('published')) == '18787' and p.get('target') == 18787 for p in ports), 'Existing LAN port is missing')
            require(not any(p.get('host_ip') == TAIL for p in ports), 'Tailscale port already present')
            new_ports = after['services'][service]['ports']
            require(len(new_ports) == len(ports) + 1 and new_ports[:-1] == ports, 'Candidate changed existing ports')
            added = new_ports[-1]
            require(added.get('host_ip') == TAIL and added.get('target') == 18787 and str(added.get('published')) == '18787' and added.get('protocol') == 'tcp', 'Candidate Tailscale port is invalid')
            expected['services'][service]['ports'] = new_ports
        require(after == expected, 'Candidate changed fields beyond the reviewed image and port additions')
    return originals, candidates


def probe_waku(protocol, host=LAN, *, require_idle=False):
    import websocket
    container = inspect_container(DEER)
    token = next((value.partition('=')[2] for value in container['Config'].get('Env', []) if value.startswith('WAKU_DAEMON_TOKEN=')), '')
    require(bool(token), 'DeerFlow token is not configured')
    socket = None
    try:
        socket = websocket.create_connection('ws://' + host + ':18787/v1', timeout=10,
                    suppress_origin=True, http_no_proxy=[host])
        socket.send(json.dumps({'type': 'hello', 'protocolVersion': protocol, 'token': token,
                               'clientId': str(uuid.uuid4()), 'resumeFrom': []}))
        hello = json.loads(socket.recv())
        require(hello.get('type') == 'hello' and hello.get('protocolVersion') == protocol, 'Waku authentication or protocol check failed')
        replies = {}
        for kind in ('getSettings', 'loadTaskState'):
            request_id = str(uuid.uuid4())
            socket.send(json.dumps({'type': 'request', 'requestId': request_id,
                'sessionId': str(uuid.UUID(int=0)), 'runtimeId': str(uuid.UUID(int=0)), 'command': {'type': kind}}))
            deadline = time.monotonic() + 10
            while time.monotonic() < deadline:
                socket.settimeout(max(0.1, deadline - time.monotonic()))
                message = json.loads(socket.recv())
                if message.get('requestId') == request_id:
                    outcome = message.get('outcome', {})
                    require(outcome.get('status') == 'ok', 'Waku read-only RPC failed')
                    replies[kind] = outcome.get('payload', {})
                    break
            require(kind in replies, 'Waku read-only RPC timed out')
        require(replies['getSettings'].get('type') == 'settings' and replies['loadTaskState'].get('type') == 'taskState', 'Unexpected Waku read-only response')
        sessions = replies['loadTaskState'].get('sessions', [])
        if require_idle:
            require(not any(session.get('status') in BUSY for session in sessions), 'A Waku task is active; deployment was not started')
        return {'protocol': protocol, 'settings': True, 'taskState': True, 'sessionCount': len(sessions)}
    except RolloutError:
        raise
    except Exception:
        raise RolloutError('Waku connection probe failed; remote details withheld') from None
    finally:
        token = None
        if socket is not None:
            socket.close(timeout=1)


def native_active_operations(timeout=15):
    """Query the existing daemon only; the control CLI does not launch it."""
    output = command(['docker', 'exec', '-i', DEER, '/var/lib/deerflow/deerflow-control-go',
                      '--runtime-dir', '/var/lib/deerflow/user-data/runtime/acp-go'],
                     payload=json.dumps({'operation': 'daemon.status'}), timeout=timeout)
    try:
        response = json.loads(output)
    except (ValueError, TypeError):
        raise RolloutError('Native daemon status could not be read; refusing to assume it is idle') from None
    require(isinstance(response, dict) and response.get('ok') is True,
            'Native daemon status was not confirmed; refusing to assume it is idle')
    data = response.get('data')
    active = data.get('active_operations') if isinstance(data, dict) else None
    require(type(active) is int and active >= 0, 'Native daemon activity count is unavailable')
    return active


def require_native_idle():
    require(native_active_operations() == 0, 'Native daemon work is active; deployment was not started')


def wait_native_idle(timeout=60):
    deadline = time.monotonic() + timeout
    while True:
        remaining = deadline - time.monotonic()
        require(remaining > 0, 'Native daemon stayed busy; DeerFlow was not stopped')
        if native_active_operations(timeout=min(15, remaining)) == 0:
            return
        remaining = deadline - time.monotonic()
        require(remaining > 0, 'Native daemon stayed busy; DeerFlow was not stopped')
        time.sleep(min(2, remaining))


def archive_data(source, name):
    require(source.is_dir() and not source.is_symlink(), 'Expected a real data directory')
    target = BACKUP / name
    require(not target.exists(), 'A data archive already exists; do not overwrite the rollback baseline')
    temporary = BACKUP / (name + '.tmp')
    require(not temporary.exists(), 'An incomplete backup needs review')
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        with os.fdopen(fd, 'wb') as output:
            with tarfile.open(fileobj=output, mode='w:gz', compresslevel=1) as archive:
                archive.add(source, arcname=source.name, recursive=True)
            output.flush()
            os.fsync(output.fileno())
        with tarfile.open(temporary, 'r:gz') as archive:
            require(bool(archive.getmembers()), 'Data backup is empty')
        os.replace(temporary, target)
    except Exception:
        # Keep an incomplete archive for diagnosis; never replace an existing backup.
        raise RolloutError('Data backup failed: ' + name) from None
    return {'path': str(target), 'bytes': target.stat().st_size, 'sha256': digest(target.read_bytes())}


def up(service):
    command(compose(service) + ['config', '--quiet'])
    command(compose(service) + ['up', '-d', '--no-deps', '--wait', '--wait-timeout', '90', service], timeout=120)


def stop(service):
    command(compose(service) + ['stop', '-t', '45', service], timeout=70)


def verify_health(name, image_id):
    status = safe_container(name)
    require(status['imageId'] == image_id and status['status'] == 'running' and status['health'] == 'healthy', 'Container health or image verification failed')
    return status


def report(value):
    value['updatedAt'] = datetime.now(timezone.utc).isoformat()
    value['dataRestorePolicy'] = 'Manual review required; rollout and rollback never restore business data automatically.'
    atomic_write(ROOT / 'deployment.json', json.dumps(value, indent=2).encode() + b'\n', 0o600)


def restore_configs(preflight, candidates):
    for path in CHANGED_PATHS:
        original = read_file(BACKUP / BACKUP_FILES[path])
        current = read_file(path)
        require(current == original or current == candidates[path], 'Concurrent configuration change prevents automatic rollback')
    for path in reversed(CHANGED_PATHS):
        original = read_file(BACKUP / BACKUP_FILES[path])
        mode = path.stat().st_mode & 0o777
        atomic_write(path, original, mode)
    for name in (DEER, FOLO):
        image = json.loads(command(['docker', 'image', 'inspect', preflight[name]['image']]))[0]
        require(image['Id'] == preflight[name]['imageId'], 'Original image tag changed; rollback requires review')


def rollback(preflight, candidates):
    stop('folotoy')
    stop('deerflow')
    restore_configs(preflight, candidates)
    up('deerflow')
    verify_health(DEER, preflight[DEER]['imageId'])
    probe_waku(9)
    up('folotoy')
    verify_health(FOLO, preflight[FOLO]['imageId'])


def apply():
    preflight = load_preflight()
    images = verify_images(preflight)
    originals, candidates = prepare_changes(preflight)
    probe_waku(9, require_idle=True)
    require_native_idle()
    result = {'status': 'prepared', 'backup': str(BACKUP), 'previousContainers': {name: preflight[name] for name in (DEER, FOLO)},
              'targetImageIds': images, 'appliedConfigHashes': {str(path): digest(data) for path, data in candidates.items()}}
    report(result)
    stopped = False
    deerflow_stop_attempted = False
    try:
        # Recheck immediately before the first service interruption.
        load_preflight()
        stopped = True
        stop('folotoy')
        wait_native_idle(timeout=60)
        probe_waku(9, require_idle=True)
        deerflow_stop_attempted = True
        stop('deerflow')
        result['archives'] = [archive_data(DEER_ROOT / 'data', 'data.tar.gz'),
                              archive_data(FOLO_ROOT / 'data', 'folotoy-data.tar.gz'),
                              archive_data(DEER_ROOT / 'skills', 'skills.tar.gz')]
        for path in CHANGED_PATHS:
            require(read_file(path) == originals[path], 'Configuration changed during the maintenance window')
            atomic_write(path, candidates[path], path.stat().st_mode & 0o777)
        up('deerflow')
        verify_health(DEER, images[DEER])
        result['waku'] = probe_waku(10, TAIL)
        up('folotoy')
        result['containers'] = {name: verify_health(name, images[name]) for name in (DEER, FOLO)}
        result['status'] = 'deployed'
        report(result)
        print(json.dumps({'status': 'deployed', 'report': str(ROOT / 'deployment.json')}))
    except Exception as error:
        result['status'] = 'failed'
        result['failureType'] = type(error).__name__
        if stopped:
            try:
                if deerflow_stop_attempted:
                    rollback(preflight, candidates)
                else:
                    # An unreadable/busy native daemon must keep running. Only FoloToy
                    # was stopped, and no deployment file has been changed yet.
                    require(all(read_file(path) == originals[path] for path in CHANGED_PATHS),
                            'Configuration changed while waiting for native work to finish')
                    up('folotoy')
                    verify_health(FOLO, preflight[FOLO]['imageId'])
                result['status'] = 'rolled_back'
            except Exception as recovery_error:
                result['status'] = 'rollback_needs_attention'
                result['rollbackFailureType'] = type(recovery_error).__name__
        report(result)
        raise RolloutError('Rollout failed; status=' + result['status'] + '; see the deployment report') from None


def manual_rollback():
    preflight = load_preflight(current=False)
    existing = json.loads(read_file(ROOT / 'deployment.json'))
    expected = existing.get('appliedConfigHashes', {})
    require(set(map(str, CHANGED_PATHS)) <= set(expected), 'Rollback report lacks reviewed configuration hashes')
    candidates = {}
    for path in CHANGED_PATHS:
        raw = read_file(path)
        original = read_file(BACKUP / BACKUP_FILES[path])
        require(raw == original or digest(raw) == expected[str(path)], 'Deployment configuration changed after rollout; manual review required')
        candidates[path] = raw
    for name in (DEER, FOLO):
        current = inspect_container(name)
        require(current['Image'] in {preflight[name]['imageId'], existing.get('targetImageIds', {}).get(name)}, 'A container was replaced after rollout; manual review required')
    rollback(preflight, candidates)
    existing['status'] = 'rolled_back'
    existing['containers'] = {name: safe_container(name) for name in (DEER, FOLO)}
    report(existing)
    print(json.dumps({'status': 'rolled_back', 'report': str(ROOT / 'deployment.json'), 'businessDataRestored': False}))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument('--check', action='store_true')
    mode.add_argument('--apply', action='store_true')
    mode.add_argument('--rollback', action='store_true')
    args = parser.parse_args()
    require(ROOT.is_dir() and BACKUP.is_dir() and not ROOT.is_symlink() and not BACKUP.is_symlink(), 'Reviewed deployment directory is unavailable')
    import fcntl
    with open(ROOT / '.rollout.lock', 'a') as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise RolloutError('Another rollout is active') from None
        if args.rollback:
            manual_rollback()
        elif args.apply:
            apply()
        else:
            baseline = load_preflight()
            images = verify_images(baseline)
            prepare_changes(baseline)
            probe_waku(9, require_idle=True)
            require_native_idle()
            print(json.dumps({'status': 'preflight_passed', 'targetImageIds': images, 'servicesChanged': False}))


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        # Do not expose raw subprocess, WebSocket or Docker errors containing private data.
        print(json.dumps({'status': 'error', 'message': str(error) if isinstance(error, RolloutError) else type(error).__name__}))
        raise SystemExit(1)
