"""Authenticated music preparation and playback owned by the voice process.

The HTTP thread starts after the legacy API process has forked. It shares the
actual device/turn coordinator; audio GET observations cross the process boundary
through a narrow multiprocessing queue. MQTT submission is not a playback ACK.
"""
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
import hashlib
import hmac
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path, PurePosixPath
import queue
import re
import shutil
import subprocess
import threading
import time
from urllib.parse import urlsplit
import uuid

from core.conversation_control import get_conversation_control


class MusicError(Exception):
    def __init__(self, code, status=400):
        self.code, self.status = code, status
        super().__init__(code)


@dataclass
class MusicJob:
    id: str
    path: Path
    relative: str
    device_key: str
    request_id: str | None
    generation: int
    dry_run: bool
    turn: object = None
    state: str = 'queued'
    filename: str | None = None
    url: str | None = None
    error: str | None = None
    created_at: float = field(default_factory=time.time)
    ready: threading.Event = field(default_factory=threading.Event)
    audio_requested: bool = False
    device_audio_requested: bool = False

    def public(self):
        result = {key: getattr(self, key) for key in
                  ('id', 'state', 'device_key', 'request_id', 'filename', 'url',
                   'error', 'audio_requested', 'device_audio_requested')}
        result['delivery_confirmed'] = False
        return result


class MusicPlaybackService:
    EXTENSIONS = {'.mp3', '.wav', '.flac', '.m4a', '.ogg', '.aac', '.opus', '.webm'}
    TERMINAL = {'published', 'dry_run', 'failed', 'cancelled'}

    @classmethod
    def from_environment(cls, app, fetch_events=None):
        path = os.environ.get('MUSIC_CONTROL_CONFIG')
        if not path:
            return None
        raw = Path(path).read_bytes()
        if len(raw) > 65536:
            raise MusicError('invalid_music_configuration', 500)
        return cls(app, json.loads(raw), fetch_events=fetch_events)

    def __init__(self, app, config, *, fetch_events=None, prepare=None):
        self.app = app
        self.control = get_conversation_control(app)
        self.token = config.get('token')
        if not isinstance(self.token, str) or len(self.token) < 24:
            raise MusicError('invalid_music_configuration', 500)
        self.media_root = Path(config.get('media_root', '/music')).resolve(strict=True)
        self.audio_root = Path(app.config.AUDIO_SAVE_PATH).resolve(strict=True)
        if not self.media_root.is_dir() or not self.audio_root.is_dir():
            raise MusicError('invalid_music_configuration', 500)
        self.public_audio_base = str(config.get('public_audio_base', app.config.AUDIO_DOWNLOAD_URL)).rstrip('/')
        parsed = urlsplit(self.public_audio_base)
        if parsed.scheme not in ('http', 'https') or not parsed.hostname or parsed.username or parsed.password or parsed.query or parsed.fragment:
            raise MusicError('invalid_music_configuration', 500)
        self.default_device = config.get('default_device_key')
        self.host = str(config.get('listen_host', '127.0.0.1'))
        self.port = int(config.get('listen_port', 8090))
        self.prepare_timeout = min(180, max(5, float(config.get('prepare_timeout', 90))))
        self.max_bytes = min(512 * 1024 * 1024, max(1024, int(config.get('max_input_bytes', 200 * 1024 * 1024))))
        self.key_code = int(config.get('key_code', 7))
        self.fetch_events = fetch_events
        self._prepare_override = prepare
        self._lock = threading.RLock()
        self._jobs = {}
        self._idempotency = {}
        self._executor = ThreadPoolExecutor(max_workers=2, thread_name_prefix='music-prepare')
        self._stop = threading.Event()
        self._server = None
        self._http_thread = None

    def start(self):
        service = self

        class Handler(BaseHTTPRequestHandler):
            def setup(self):
                super().setup()
                self.connection.settimeout(10)

            def log_message(self, *args):
                return  # Never log bearer headers, request bodies or media paths.

            def respond(self, status, body):
                raw = json.dumps(body, ensure_ascii=False).encode()
                self.send_response(status)
                self.send_header('Content-Type', 'application/json; charset=utf-8')
                self.send_header('Content-Length', str(len(raw)))
                self.send_header('Cache-Control', 'no-store')
                self.end_headers()
                self.wfile.write(raw)

            def dispatch(self):
                header = self.headers.get('Authorization', '')
                if not hmac.compare_digest(header.encode(), ('Bearer ' + service.token).encode()):
                    raise MusicError('unauthorized', 401)
                path = urlsplit(self.path).path
                if self.command == 'GET' and path == '/v1/music/health':
                    return 200, {'ok': True, 'available': bool(service.app.connected_flag),
                                 'media_ready': True, 'playback_confirmation': 'audio_get_observation'}
                if self.command == 'GET' and re.fullmatch('/v1/music/jobs/[0-9a-f]{32}', path):
                    return 200, {'ok': True, 'job': service.get(path.rsplit('/', 1)[1])}
                if self.command == 'POST' and path == '/v1/music/play':
                    try:
                        length = int(self.headers.get('Content-Length', '0'))
                    except ValueError:
                        raise MusicError('invalid_body', 400)
                    if not 1 <= length <= 8192:
                        raise MusicError('invalid_body_size', 413)
                    if self.headers.get_content_type() != 'application/json':
                        raise MusicError('json_required', 415)
                    try:
                        body = json.loads(self.rfile.read(length))
                    except (ValueError, UnicodeError):
                        raise MusicError('invalid_json', 400)
                    job, repeated = service.submit(body)
                    return 200 if repeated else 202, {'ok': True, 'job': job, 'idempotent': repeated}
                raise MusicError('not_found', 404)

            def handle_request(self):
                try:
                    status, result = self.dispatch()
                except MusicError as error:
                    status, result = error.status, {'ok': False, 'error': error.code}
                except Exception:
                    status, result = 500, {'ok': False, 'error': 'music_control_failure'}
                self.respond(status, result)

            do_GET = do_POST = handle_request

        self._server = ThreadingHTTPServer((self.host, self.port), Handler)
        self._server.daemon_threads = True
        self._http_thread = threading.Thread(target=self._server.serve_forever,
                                             name='music-control-http', daemon=True)
        self._http_thread.start()

    def stop(self):
        self._stop.set()
        if self._server is not None:
            self._server.shutdown()
            self._server.server_close()
        self._executor.shutdown(wait=True, cancel_futures=True)

    def _path(self, value):
        if not isinstance(value, str) or not value or len(value) > 4096 or '\\' in value or '\x00' in value:
            raise MusicError('invalid_media_path')
        relative = PurePosixPath(value)
        if relative.is_absolute() or '..' in relative.parts:
            raise MusicError('invalid_media_path')
        try:
            path = (self.media_root / value).resolve(strict=True)
        except OSError:
            raise MusicError('media_not_found', 404)
        if not path.is_relative_to(self.media_root) or not path.is_file() or path.suffix.lower() not in self.EXTENSIONS:
            raise MusicError('invalid_media_path')
        if not 0 < path.stat().st_size <= self.max_bytes:
            raise MusicError('invalid_media_size')
        return path

    def submit(self, body):
        if not isinstance(body, dict) or set(body) - {'path', 'request_id', 'device_key', 'idempotency_key', 'dry_run'}:
            raise MusicError('invalid_request')
        key = body.get('idempotency_key')
        if not isinstance(key, str) or not re.fullmatch(r'[A-Za-z0-9_.:-]{8,128}', key):
            raise MusicError('idempotency_key_required')
        if not isinstance(body.get('dry_run', False), bool):
            raise MusicError('invalid_dry_run')
        device = body.get('device_key')
        request = body.get('request_id')
        if device is not None and (not isinstance(device, str) or not re.fullmatch(r'[A-Za-z0-9_-]{1,128}', device)):
            raise MusicError('invalid_device_key')
        if request is not None and (not isinstance(request, str) or not 1 <= len(request) <= 128):
            raise MusicError('invalid_request_id')
        with self._lock:
            previous = self._idempotency.get(key)
            existing = self._jobs.get(previous[1]) if previous else None
        if existing is not None:
            replay_device = device or (existing.device_key if request else self.default_device)
            if previous[0] != (body.get('path'), replay_device, request, body.get('dry_run', False)):
                raise MusicError('idempotency_conflict', 409)
            return self.get(existing.id), True
        path = self._path(body.get('path'))
        turn = self.control.current(device, request) if request else None
        if request and turn is None:
            raise MusicError('voice_request_not_current', 409)
        if turn is not None:
            device = turn.dkey
        device = device or self.default_device
        if not isinstance(device, str) or not device:
            raise MusicError('device_key_required')
        if device not in self.app.devices and device != self.default_device:
            raise MusicError('device_not_registered', 409)
        if not self.app.connected_flag and not body.get('dry_run', False):
            raise MusicError('mqtt_unavailable', 503)
        signature = (body['path'], device, request, body.get('dry_run', False))
        generation = self.control.generation(device)
        result = []

        def register():
            with self._lock:
                previous = self._idempotency.get(key)
                if previous:
                    if previous[0] != signature:
                        raise MusicError('idempotency_conflict', 409)
                    result.append((self._jobs[previous[1]], True))
                    return
                if turn is not None and (turn.terminal_sent or getattr(turn, 'music_closed', False)):
                    raise MusicError('voice_request_already_finished', 409)
                if len(self._jobs) >= 256:
                    removable = next((j for j in self._jobs.values() if j.state in self.TERMINAL), None)
                    if removable is None:
                        raise MusicError('music_queue_full', 429)
                    self._jobs.pop(removable.id)
                    self._idempotency = {k: v for k, v in self._idempotency.items() if v[1] != removable.id}
                job = MusicJob(uuid.uuid4().hex, path, body['path'], device, request,
                               generation, signature[3], turn)
                self._jobs[job.id] = job
                self._idempotency[key] = (signature, job.id)
                result.append((job, False))

        if turn is not None:
            if not self.control.apply(turn, register):
                raise MusicError('voice_request_not_current', 409)
        else:
            register()
        job, repeated = result[0]
        if not repeated:
            self._executor.submit(self._run, job)
        return job.public(), repeated

    def _cancelled(self, job):
        return (self._stop.is_set() or (job.turn is not None and not self.control.is_current(job.turn))
                or self.control.generation(job.device_key) != job.generation)

    def _prepare(self, job):
        if self._prepare_override:
            return self._prepare_override(job)
        ffmpeg = shutil.which('ffmpeg')
        if not ffmpeg:
            raise MusicError('ffmpeg_unavailable', 503)
        name = 'music-' + job.id + '.mp3'
        output = self.audio_root / name
        temporary = self.audio_root / ('.music-' + job.id + '.tmp.mp3')
        argv = [ffmpeg, '-hide_banner', '-loglevel', 'error', '-nostdin', '-y',
                '-i', str(job.path), '-vn', '-ar', '24000', '-ac', '1',
                '-c:a', 'libmp3lame', '-b:a', '96k', str(temporary)]
        process = subprocess.Popen(argv, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        deadline = time.monotonic() + self.prepare_timeout
        try:
            while process.poll() is None:
                if self._cancelled(job):
                    raise MusicError('cancelled', 409)
                if time.monotonic() >= deadline:
                    raise MusicError('transcode_timeout', 504)
                time.sleep(0.05)
            if process.returncode != 0 or not temporary.is_file() or temporary.stat().st_size == 0:
                raise MusicError('transcode_failed', 422)
            os.replace(temporary, output)
            return name, self.public_audio_base + '/' + name
        finally:
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=2)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=2)
            temporary.unlink(missing_ok=True)

    def _run(self, job):
        try:
            with self._lock:
                job.state = 'preparing'
            if self._cancelled(job):
                raise MusicError('cancelled', 409)
            filename, url = self._prepare(job)
            if self._cancelled(job):
                raise MusicError('cancelled', 409)
            with self._lock:
                job.filename, job.url = filename, url
                job.state = 'dry_run' if job.dry_run else ('ready' if job.turn is not None else 'waiting_turn')
            job.ready.set()
            if job.dry_run or job.turn is not None:
                return
            deadline = time.monotonic() + self.prepare_timeout
            while True:
                if self._cancelled(job):
                    raise MusicError('cancelled', 409)
                if self.control.apply_idle_generation(job.device_key, job.generation, lambda: self._publish_standalone(job)):
                    return
                if time.monotonic() >= deadline:
                    raise MusicError('active_voice_turn_timeout', 409)
                time.sleep(0.05)
        except MusicError as error:
            with self._lock:
                job.state = 'cancelled' if error.code == 'cancelled' else 'failed'
                job.error = error.code
        except Exception:
            with self._lock:
                job.state, job.error = 'failed', 'music_prepare_failure'
        finally:
            job.ready.set()

    def _publish_standalone(self, job):
        if not self.app.connected_flag:
            raise MusicError('mqtt_unavailable', 503)
        audio = self.audio_root / job.filename
        etag = hashlib.md5(audio.read_bytes()).hexdigest()
        message = {'msgId': int(time.time() * 1000), 'identifier': 'updatestartvoice',
                   'inputParams': {'url': job.url, 'keyCode': self.key_code, 'etag': etag}}
        topic = '/user/%s/%s/thing/command/call' % (self.app.product_key, job.device_key)
        sent = self.app.mqtt_client.publish(topic, json.dumps(message), qos=self.app.qos)
        if getattr(sent, 'rc', None) != 0:
            raise MusicError('mqtt_publish_failed', 503)
        with self._lock:
            job.state = 'published'

    def finish_turn(self, turn, send_url):
        """Freeze acceptance, finish accepted preparations, append before total."""
        self.control.apply(turn, lambda: setattr(turn, 'music_closed', True))
        with self._lock:
            jobs = [job for job in self._jobs.values() if job.turn is turn]
        deadline = time.monotonic() + self.prepare_timeout
        for job in jobs:
            while not job.ready.wait(0.05):
                if self._cancelled(job) or time.monotonic() >= deadline:
                    break
            def append():
                with self._lock:
                    if self._cancelled(job):
                        job.state, job.error = 'cancelled', 'cancelled'
                    elif job.state == 'ready' and not turn.terminal_sent:
                        send_url(turn.dkey, turn.recording_id, turn.sent_count + 1, job.url)
                        turn.sent_count += 1
                        job.state = 'published'
                    elif job.state not in self.TERMINAL:
                        job.state, job.error = 'failed', 'transcode_timeout'
            if not self.control.apply(turn, append):
                with self._lock:
                    job.state, job.error = 'cancelled', 'cancelled'

    def _observe_fetches(self):
        if self.fetch_events is None:
            return
        while True:
            try:
                event = self.fetch_events.get_nowait()
            except queue.Empty:
                return
            if not isinstance(event, dict):
                continue
            with self._lock:
                job = next((job for job in self._jobs.values() if job.filename == event.get('filename')), None)
                if job is not None:
                    job.audio_requested = True
                    job.device_audio_requested |= bool(event.get('esp32'))

    def get(self, job_id):
        self._observe_fetches()
        with self._lock:
            job = self._jobs.get(job_id)
            if job is None:
                raise MusicError('job_not_found', 404)
        cancelled = self._cancelled(job)
        with self._lock:
            if cancelled and job.state not in self.TERMINAL:
                job.state, job.error = 'cancelled', 'cancelled'
            return job.public()
