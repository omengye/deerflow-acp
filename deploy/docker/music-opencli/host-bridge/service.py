#!/usr/bin/env python3
"""Start/stop the gen-user bridge without assuming systemd or requiring root."""
import argparse
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time
import urllib.request


def private_write(path, value):
    temporary = path.with_name('.' + path.name + '.tmp')
    fd = os.open(temporary, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    with os.fdopen(fd, 'w') as output:
        json.dump(value, output)
        output.flush()
        os.fsync(output.fileno())
    os.replace(temporary, path)


def identity(pid, script, config):
    if not isinstance(pid, int) or pid <= 1:
        return False
    path = Path('/proc') / str(pid) / 'cmdline'
    try:
        argv = path.read_bytes().decode().split('\0')
        return str(script) in argv and '--config' in argv and str(config) in argv
    except (OSError, UnicodeError):
        return False


def health(config):
    host = config.get('bind', '172.27.0.1')
    if host in ('0.0.0.0', '::'):
        host = '127.0.0.1'
    req = urllib.request.Request('http://' + host + ':' + str(config.get('port', 18990)) + '/health',
                                 headers={'Authorization': 'Bearer ' + config['token']})
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(req, timeout=2) as response:
            value = json.load(response)
        return value.get('ok') is True and value.get('service') == 'opencli-host-bridge'
    except Exception:
        return False


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('operation', choices=('start', 'stop', 'status'))
    parser.add_argument('--config', required=True, type=Path)
    parser.add_argument('--pid-file', type=Path)
    parser.add_argument('--log-file', type=Path)
    args = parser.parse_args()
    os.umask(0o077)
    config_path = args.config.resolve(strict=True)
    config = json.loads(config_path.read_text())
    script = Path(__file__).with_name('host_opencli_bridge.py').resolve(strict=True)
    pid_file = (args.pid_file or config_path.with_name('opencli-bridge.pid.json')).resolve()
    log_file = (args.log_file or config_path.with_name('opencli-bridge.log')).resolve()
    try:
        recorded = json.loads(pid_file.read_text())
    except (OSError, ValueError):
        recorded = {}
    pid = recorded.get('pid')
    own = identity(pid, script, config_path)
    ready = health(config)
    if args.operation == 'status':
        print(json.dumps({'process_identity_confirmed': own, 'healthy': ready, 'pid': pid if own else None}))
        return 0 if own and ready else 1
    if args.operation == 'stop':
        if not own:
            # Never signal a reused PID or a service started by another launcher.
            print(json.dumps({'status': 'not_running_or_unowned', 'signalled': False}))
            return 0 if not ready else 1
        os.kill(pid, signal.SIGTERM)
        deadline = time.monotonic() + 12
        while identity(pid, script, config_path) and time.monotonic() < deadline:
            time.sleep(0.1)
        if identity(pid, script, config_path):
            print(json.dumps({'status': 'stop_unconfirmed', 'pid': pid}))
            return 1
        pid_file.unlink(missing_ok=True)
        print(json.dumps({'status': 'stopped', 'pid': pid}))
        return 0
    if own and ready:
        print(json.dumps({'status': 'already_running', 'pid': pid}))
        return 0
    if own or ready:
        print(json.dumps({'status': 'conflicting_or_unhealthy_service', 'started': False}))
        return 1
    pid_file.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    fd = os.open(log_file, os.O_CREAT | os.O_APPEND | os.O_WRONLY, 0o600)
    try:
        process = subprocess.Popen([sys.executable, str(script), '--config', str(config_path)],
                                   stdin=subprocess.DEVNULL, stdout=fd, stderr=fd,
                                   start_new_session=True, close_fds=True)
    finally:
        os.close(fd)
    private_write(pid_file, {'pid': process.pid, 'script': str(script), 'config': str(config_path)})
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        if health(config):
            print(json.dumps({'status': 'started', 'pid': process.pid, 'healthy': True}))
            return 0
        if process.poll() is not None:
            break
        time.sleep(0.1)
    print(json.dumps({'status': 'startup_failed', 'pid': process.pid, 'log': str(log_file)}))
    return 1


if __name__ == '__main__':
    raise SystemExit(main())
