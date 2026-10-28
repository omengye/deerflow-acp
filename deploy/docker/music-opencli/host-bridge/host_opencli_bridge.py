#!/usr/bin/env python3
"""Authenticated, bounded OpenCLI job execution in the existing host user session."""
import argparse
from collections import deque
import hashlib
import hmac
import ipaddress
import json
import os
from pathlib import Path
import re
import signal
import sqlite3
import subprocess
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit

TERMINAL = {'completed', 'failed', 'cancelled', 'timed_out', 'output_limit', 'unknown'}
DEFAULT_SITES = ['web', 'twitter', 'xiaohongshu', 'xiaoyuzhou', 'weixin', 'douyin', 'bilibili', 'github']
WORD = re.compile(r'[A-Za-z0-9_][A-Za-z0-9_-]{0,63}\Z')


class RequestError(Exception):
    def __init__(self, code, status=400):
        self.code, self.status = code, status


def read_config(path):
    path = Path(path).resolve(strict=True)
    if os.name != 'nt' and path.stat().st_mode & 0o077:
        raise ValueError('Host bridge configuration must be private (0600)')
    value = json.loads(path.read_text())
    if not isinstance(value.get('token'), str) or len(value['token']) < 32:
        raise ValueError('A private bridge token of at least 32 characters is required')
    executable = Path(value['executable']).expanduser().resolve(strict=True)
    if not executable.is_file() or not os.access(executable, os.X_OK):
        raise ValueError('The fixed OpenCLI executable is not executable')
    home = Path(value.get('home', str(Path.home()))).expanduser().resolve(strict=True)
    if not home.is_dir():
        raise ValueError('The host home directory does not exist')
    value.update(executable=str(executable), home=str(home))
    value.setdefault('bind', '172.27.0.1')
    value.setdefault('port', 18990)
    value.setdefault('allowed_sites', DEFAULT_SITES)
    value.setdefault('allowed_cidrs', ['127.0.0.0/8', '172.27.0.0/16'])
    value.setdefault('max_timeout_seconds', 70)
    value.setdefault('output_limit_bytes', 262144)
    value.setdefault('max_jobs', 4096)
    value.setdefault('queue_size', 8)
    value.setdefault('workers', 1)
    value.setdefault('journal', str(path.parent / 'opencli-jobs.sqlite3'))
    if (not isinstance(value['allowed_sites'], list) or len(value['allowed_sites']) > 64
            or any(not isinstance(s, str) or not WORD.fullmatch(s) for s in value['allowed_sites'])):
        raise ValueError('Invalid host OpenCLI site allowlist')
    if not 1 <= value['workers'] <= 2 or not 1 <= value['queue_size'] <= 32:
        raise ValueError('Invalid worker/queue bounds')
    if not 1 <= value['max_timeout_seconds'] <= 180 or not 1024 <= value['output_limit_bytes'] <= 1048576:
        raise ValueError('Invalid process bounds')
    if not 16 <= value['max_jobs'] <= 4096:
        raise ValueError('Invalid journal bound')
    value['_networks'] = [ipaddress.ip_network(n) for n in value['allowed_cidrs']]
    prefix = value.get('prefix', [])
    if not isinstance(prefix, list) or any(not isinstance(s, str) or '\0' in s for s in prefix):
        raise ValueError('Invalid trusted launcher prefix')
    return value


def validate_argv(argv, sites):
    if (not isinstance(argv, list) or not 1 <= len(argv) <= 132
            or any(not isinstance(a, str) or '\0' in a or len(a.encode()) > 16384 for a in argv)):
        raise RequestError('invalid_argv')
    first = argv[0]
    if first in ('--version', '-V') and len(argv) == 1:
        return
    formats = {'table', 'yaml', 'yml', 'json', 'plain', 'md', 'csv'}
    if first in ('--help', '-h') and len(argv) == 1:
        return
    if first == 'list':
        i = 1
        while i < len(argv):
            arg = argv[i]
            if arg in ('--help', '-h'):
                i += 1
                continue
            if arg in ('--format', '-f') and i + 1 < len(argv) and argv[i + 1] in formats:
                i += 2
                continue
            if arg.startswith(('--format=', '-f=')) and arg.split('=', 1)[1] in formats:
                i += 1
                continue
            raise RequestError('invalid_list_arguments')
        return
    if first == 'doctor' and all(a in ('-v', '--verbose', '--help', '-h') for a in argv[1:]):
        return
    if first not in sites or not WORD.fullmatch(first) or len(argv) < 2:
        raise RequestError('site_or_surface_not_allowed', 403)
    if argv[1] in ('--help', '-h') or WORD.fullmatch(argv[1]):
        return
    raise RequestError('invalid_adapter_command')


class Jobs:
    """A private durable receipt journal; interrupted jobs are never replayed."""
    def __init__(self, config):
        self.config = config
        self.lock = threading.RLock()
        self.condition = threading.Condition(self.lock)
        self.queue = deque()
        self.stopping = False
        self.controls = {}
        journal = Path(config['journal'])
        journal.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        self.db = sqlite3.connect(str(journal), check_same_thread=False)
        self.db.row_factory = sqlite3.Row
        self.db.execute('PRAGMA journal_mode=WAL')
        self.db.execute('CREATE TABLE IF NOT EXISTS jobs (id TEXT PRIMARY KEY, idempotency_key TEXT UNIQUE NOT NULL, digest TEXT NOT NULL, argv TEXT NOT NULL, timeout REAL NOT NULL, created REAL NOT NULL, updated REAL NOT NULL, state TEXT NOT NULL, receipt TEXT NOT NULL)')
        for row in self.db.execute("SELECT id,receipt FROM jobs WHERE state NOT IN ('completed','failed','cancelled','timed_out','output_limit','unknown')").fetchall():
            result = json.loads(row['receipt'])
            result.update(state='unknown', termination_confirmed=False, error='bridge_restarted_no_replay', updated_at=time.time())
            self.db.execute('UPDATE jobs SET state=?,updated=?,receipt=? WHERE id=?', ('unknown', time.time(), json.dumps(result), row['id']))
        self.db.commit()
        if os.name != 'nt':
            journal.chmod(0o600)
        self.threads = [threading.Thread(target=self._worker, name='opencli-worker-' + str(i), daemon=True)
                        for i in range(config['workers'])]
        for thread in self.threads:
            thread.start()

    def _save(self, receipt):
        receipt['updated_at'] = time.time()
        self.db.execute('UPDATE jobs SET state=?,updated=?,receipt=? WHERE id=?',
                        (receipt['state'], receipt['updated_at'], json.dumps(receipt, ensure_ascii=False), receipt['id']))
        self.db.commit()

    def get(self, job_id):
        with self.lock:
            row = self.db.execute('SELECT receipt FROM jobs WHERE id=?', (job_id,)).fetchone()
            if row is None:
                raise RequestError('job_not_found', 404)
            return json.loads(row['receipt'])

    def submit(self, payload):
        if not isinstance(payload, dict) or set(payload) - {'idempotency_key', 'argv', 'timeout_seconds'}:
            raise RequestError('invalid_job_fields')
        key, argv = payload.get('idempotency_key'), payload.get('argv')
        if not isinstance(key, str) or not re.fullmatch(r'[A-Za-z0-9_-]{16,128}', key):
            raise RequestError('invalid_idempotency_key')
        validate_argv(argv, self.config['allowed_sites'])
        timeout = payload.get('timeout_seconds', self.config['max_timeout_seconds'])
        if isinstance(timeout, bool) or not isinstance(timeout, (int, float)) or not 1 <= timeout <= self.config['max_timeout_seconds']:
            raise RequestError('invalid_timeout')
        digest = hashlib.sha256(json.dumps([argv, timeout], ensure_ascii=False).encode()).hexdigest()
        with self.condition:
            row = self.db.execute('SELECT digest,receipt FROM jobs WHERE idempotency_key=?', (key,)).fetchone()
            if row:
                if row['digest'] != digest:
                    raise RequestError('idempotency_conflict', 409)
                return json.loads(row['receipt']), False
            if self.stopping:
                raise RequestError('bridge_stopping', 503)
            if len(self.queue) >= self.config['queue_size']:
                raise RequestError('queue_full', 429)
            count = self.db.execute('SELECT count(*) FROM jobs').fetchone()[0]
            if count >= self.config['max_jobs']:
                # Preserve every retained idempotency key; explicit journal rotation
                # is safer than silently forgetting an old mutating request.
                raise RequestError('journal_full_requires_rotation', 507)
            now, job_id = time.time(), uuid.uuid4().hex
            receipt = {'id': job_id, 'state': 'queued', 'created_at': now, 'updated_at': now,
                       'exit_code': None, 'termination_confirmed': False,
                       'stdout': '', 'stderr': '', 'stdout_truncated': False, 'stderr_truncated': False}
            self.db.execute('INSERT INTO jobs VALUES(?,?,?,?,?,?,?,?,?)',
                            (job_id, key, digest, json.dumps(argv), timeout, now, now, 'queued', json.dumps(receipt)))
            self.db.commit()
            self.controls[job_id] = threading.Event()
            self.queue.append(job_id)
            self.condition.notify()
            return receipt, True

    def cancel(self, job_id):
        with self.condition:
            receipt = self.get(job_id)
            if receipt['state'] in TERMINAL:
                return receipt
            event = self.controls.get(job_id)
            if event:
                event.set()
            if receipt['state'] == 'queued':
                receipt.update(state='cancelled', termination_confirmed=True, error='cancelled_before_execution')
                self._save(receipt)
            else:
                receipt['cancel_requested'] = True
                self._save(receipt)
            self.condition.notify_all()
            return receipt

    def _worker(self):
        while True:
            with self.condition:
                while not self.queue and not self.stopping:
                    self.condition.wait()
                if self.stopping and not self.queue:
                    return
                job_id = self.queue.popleft()
                row = self.db.execute('SELECT * FROM jobs WHERE id=?', (job_id,)).fetchone()
                receipt = json.loads(row['receipt'])
                if receipt['state'] != 'queued':
                    self.controls.pop(job_id, None)
                    continue
                receipt['state'] = 'running'
                self._save(receipt)
                cancelled = self.controls[job_id]
            try:
                outcome = self._execute(json.loads(row['argv']), row['timeout'], cancelled)
            except Exception:
                # If execution raised after spawn, _execute owns process cleanup.
                outcome = {'state': 'unknown', 'termination_confirmed': False, 'error': 'execution_receipt_unavailable'}
            with self.condition:
                receipt.update(outcome)
                self._save(receipt)
                self.controls.pop(job_id, None)

    def _execute(self, argv, timeout, cancelled):
        environment = dict(os.environ)
        environment.update(HOME=self.config['home'],
                           PATH=self.config.get('path', '/home/gen/.npm-global/bin:/usr/local/bin:/usr/bin:/bin'),
                           OPENCLI_BROWSER_CONNECT_TIMEOUT='30', OPENCLI_BROWSER_COMMAND_TIMEOUT='55')
        # This deprecated override is incompatible with the installed extension.
        environment.pop('OPENCLI_DAEMON_PORT', None)
        output = {'stdout': bytearray(), 'stderr': bytearray()}
        truncated = {'stdout': False, 'stderr': False}
        total = 0
        output_lock = threading.Lock()
        exceeded = threading.Event()
        process = None
        readers = []
        reason = None
        try:
            process = subprocess.Popen([self.config['executable'], *self.config.get('prefix', []), *argv],
                                       cwd=self.config['home'], env=environment, stdin=subprocess.DEVNULL,
                                       stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                       start_new_session=(os.name != 'nt'))
            def collect(name, pipe):
                nonlocal total
                try:
                    while True:
                        chunk = pipe.read(4096)
                        if not chunk:
                            return
                        with output_lock:
                            remaining = max(0, self.config['output_limit_bytes'] - total)
                            output[name].extend(chunk[:remaining])
                            total += min(remaining, len(chunk))
                            if len(chunk) > remaining:
                                truncated[name] = True
                                exceeded.set()
                finally:
                    pipe.close()
            for name, pipe in (('stdout', process.stdout), ('stderr', process.stderr)):
                thread = threading.Thread(target=collect, args=(name, pipe), daemon=True)
                readers.append(thread)
                thread.start()
            deadline = time.monotonic() + timeout
            while process.poll() is None:
                if cancelled.is_set():
                    reason = 'cancelled'
                elif exceeded.is_set():
                    reason = 'output_limit'
                elif time.monotonic() >= deadline:
                    reason = 'timed_out'
                if reason:
                    break
                time.sleep(0.025)
            if reason:
                self._terminate(process)
            code = process.wait(timeout=4)
            for reader in readers:
                reader.join(timeout=2)
            if any(reader.is_alive() for reader in readers) or self._group_alive(process.pid):
                self._terminate(process)
                for reader in readers:
                    reader.join(timeout=1)
            confirmed = (not any(reader.is_alive() for reader in readers)
                         and not self._group_alive(process.pid))
            if exceeded.is_set() and reason is None:
                reason = 'output_limit'
            state = reason or ('completed' if code == 0 else 'failed')
            if not confirmed:
                state = 'unknown'
            with output_lock:
                result = {'state': state, 'exit_code': code, 'termination_confirmed': confirmed,
                          'stdout': output['stdout'].decode('utf-8', 'replace'),
                          'stderr': output['stderr'].decode('utf-8', 'replace'),
                          'stdout_truncated': truncated['stdout'], 'stderr_truncated': truncated['stderr']}
            if reason:
                result['error'] = reason
            return result
        except OSError:
            if process is None:
                return {'state': 'failed', 'exit_code': None, 'termination_confirmed': True,
                        'error': 'launcher_unavailable', 'stdout': '', 'stderr': ''}
            raise
        finally:
            if process is not None and process.poll() is None:
                self._terminate(process)

    @staticmethod
    def _group_alive(pid):
        if os.name == 'nt':
            return False
        try:
            os.killpg(pid, 0)
            return True
        except ProcessLookupError:
            return False

    @classmethod
    def _terminate(cls, process):
        try:
            if os.name != 'nt':
                os.killpg(process.pid, signal.SIGTERM)
            else:
                process.terminate()
            process.wait(timeout=1)
        except subprocess.TimeoutExpired:
            try:
                if os.name != 'nt':
                    os.killpg(process.pid, signal.SIGKILL)
                else:
                    process.kill()
                process.wait(timeout=3)
            except (ProcessLookupError, subprocess.TimeoutExpired):
                pass
        except ProcessLookupError:
            pass
        if os.name != 'nt' and cls._group_alive(process.pid):
            until = time.monotonic() + 0.4
            while cls._group_alive(process.pid) and time.monotonic() < until:
                time.sleep(0.025)
            if cls._group_alive(process.pid):
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass

    def close(self):
        with self.condition:
            self.stopping = True
            for event in self.controls.values():
                event.set()
            self.condition.notify_all()
        for thread in self.threads:
            thread.join(timeout=8)
        if not any(thread.is_alive() for thread in self.threads):
            with self.lock:
                self.db.close()


class Server(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True

    def __init__(self, address, config, jobs):
        self.config, self.jobs = config, jobs
        super().__init__(address, Handler)


class Handler(BaseHTTPRequestHandler):
    server_version = 'DeerFlowOpenCLIBridge/1'

    def log_message(self, *_):
        pass

    def send_json(self, status, value):
        body = json.dumps(value, ensure_ascii=False).encode()
        self.send_response(status)
        self.send_header('Content-Type', 'application/json; charset=utf-8')
        self.send_header('Cache-Control', 'no-store')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        try:
            self.wfile.write(body)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def authenticate(self):
        address = ipaddress.ip_address(self.client_address[0])
        if not any(address in network for network in self.server.config['_networks']):
            raise RequestError('client_network_not_allowed', 403)
        expected = 'Bearer ' + self.server.config['token']
        if not hmac.compare_digest(self.headers.get('Authorization', ''), expected):
            raise RequestError('unauthorized', 401)

    def handle_request(self, method):
        try:
            self.authenticate()
            path = urlsplit(self.path).path
            if method == 'GET' and path == '/health':
                with self.server.jobs.lock:
                    queued = len(self.server.jobs.queue)
                self.send_json(200, {'ok': True, 'service': 'opencli-host-bridge', 'queued': queued})
                return
            if method == 'POST' and path == '/v1/opencli/jobs':
                try:
                    length = int(self.headers.get('Content-Length', '0'))
                except ValueError:
                    raise RequestError('invalid_content_length')
                if not 0 < length <= 65536:
                    raise RequestError('request_size_invalid', 413)
                try:
                    payload = json.loads(self.rfile.read(length))
                except (ValueError, UnicodeError):
                    raise RequestError('invalid_json')
                receipt, created = self.server.jobs.submit(payload)
                self.send_json(202 if created else 200, receipt)
                return
            match = re.fullmatch(r'/v1/opencli/jobs/([0-9a-f]{32})', path)
            if match and method in ('GET', 'DELETE'):
                receipt = self.server.jobs.get(match[1]) if method == 'GET' else self.server.jobs.cancel(match[1])
                self.send_json(200, receipt)
                return
            raise RequestError('not_found', 404)
        except RequestError as error:
            self.send_json(error.status, {'error': error.code})
        except Exception:
            self.send_json(500, {'error': 'bridge_internal_error'})

    def do_GET(self):
        self.handle_request('GET')

    def do_POST(self):
        self.handle_request('POST')

    def do_DELETE(self):
        self.handle_request('DELETE')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--config', required=True, type=Path)
    args = parser.parse_args()
    os.umask(0o077)
    config = read_config(args.config)
    jobs = Jobs(config)
    server = Server((config['bind'], config['port']), config, jobs)
    def shutdown(*_):
        threading.Thread(target=server.shutdown, daemon=True).start()
    signal.signal(signal.SIGTERM, shutdown)
    signal.signal(signal.SIGINT, shutdown)
    try:
        server.serve_forever(poll_interval=0.2)
    finally:
        server.server_close()
        jobs.close()


if __name__ == '__main__':
    main()
