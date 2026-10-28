#!/usr/bin/env python3
"""Queue a shared local audio file on the authenticated FoloToy music service."""
import argparse
import hashlib
import json
from pathlib import Path
import random
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

DEFAULT_CONFIG = Path('/var/lib/deerflow/user-data/private/skills/music/playback.json')
DEFAULT_ROOT = Path('/workspaces/folotoy/music')
EXTENSIONS = {'.mp3', '.wav', '.flac', '.m4a', '.ogg', '.aac', '.opus'}
TERMINAL = {'published', 'dry_run', 'failed', 'cancelled'}


class PlaybackError(Exception):
    def __init__(self, code, **details):
        super().__init__(code)
        self.code, self.details = code, details


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def private_config(path):
    try:
        if path.stat().st_size > 65536:
            raise PlaybackError('private_config_too_large')
        value = json.loads(path.read_text(encoding='utf-8'))
    except (OSError, ValueError):
        raise PlaybackError('private_config_unavailable') from None
    if not isinstance(value, dict):
        raise PlaybackError('invalid_private_config')
    url, token = value.get('url'), value.get('token')
    if not isinstance(url, str) or not isinstance(token, str) or not 16 <= len(token) <= 4096:
        raise PlaybackError('invalid_private_config')
    parsed = urllib.parse.urlsplit(url)
    if (parsed.scheme not in ('http', 'https') or not parsed.hostname
            or parsed.username or parsed.password or parsed.query or parsed.fragment):
        raise PlaybackError('invalid_playback_base_url')
    if 'music_root' in value and not isinstance(value['music_root'], str):
        raise PlaybackError('invalid_private_config')
    return {**value, 'url': url.rstrip('/'), 'token': token}


def public_result(value, token):
    if isinstance(value, dict):
        return {key: public_result(item, token) for key, item in value.items()
                if str(key).lower() not in ('token', 'authorization', 'api_key', 'password', 'cookie')}
    if isinstance(value, list):
        return [public_result(item, token) for item in value]
    if isinstance(value, str):
        return value.replace(token, '[REDACTED]')
    return value


def request(config, method, path, payload=None, timeout=12, *, opener=None):
    opener = opener or urllib.request.build_opener(NoRedirect())
    body = json.dumps(payload, ensure_ascii=False).encode() if payload is not None else None
    req = urllib.request.Request(config['url'] + path, data=body, method=method,
                                 headers={'Authorization': 'Bearer ' + config['token'],
                                          'Content-Type': 'application/json',
                                          'Accept': 'application/json'})
    try:
        with opener.open(req, timeout=timeout) as response:
            raw = response.read(128 * 1024 + 1)
            if len(raw) > 128 * 1024:
                raise PlaybackError('playback_response_too_large')
            value = json.loads(raw)
    except urllib.error.HTTPError as error:
        # No automatic retry: a lost POST response may have queued the music.
        code = 'playback_http_error'
        try:
            value = json.loads(error.read(16384))
            candidate = value.get('error') if isinstance(value, dict) else None
            if isinstance(candidate, str) and re.fullmatch(r'[a-z_]{1,80}', candidate):
                code = candidate
        except (OSError, ValueError):
            pass
        raise PlaybackError(code, http_status=error.code) from None
    except (urllib.error.URLError, TimeoutError, OSError):
        raise PlaybackError('playback_connection_unconfirmed', method=method,
                            retry_automatically=False) from None
    except ValueError:
        raise PlaybackError('invalid_playback_response') from None
    if not isinstance(value, dict) or not isinstance(value.get('ok'), bool):
        raise PlaybackError('invalid_playback_response')
    return public_result(value, config['token'])


def select_audio(raw_path, root, match=None, recursive=False, randomized=False):
    root = root.expanduser().resolve()
    candidate = Path(raw_path).expanduser()
    candidate = (root / candidate) if not candidate.is_absolute() else candidate
    candidate = candidate.resolve()
    if not candidate.is_relative_to(root):
        raise PlaybackError('audio_path_outside_shared_root')
    if candidate.is_dir():
        options = candidate.rglob('*') if recursive else candidate.iterdir()
        files = []
        for path in options:
            if not path.is_file() or path.suffix.lower() not in EXTENSIONS:
                continue
            resolved = path.resolve()
            if resolved.is_relative_to(root) and (not match or match.casefold() in path.name.casefold()):
                files.append(resolved)
        files.sort(key=lambda path: str(path).casefold())
        if not files:
            raise PlaybackError('no_matching_audio_files')
        candidate = random.choice(files) if randomized else files[0]
    if not candidate.is_file():
        raise PlaybackError('audio_file_not_found')
    if candidate.suffix.lower() not in EXTENSIONS:
        raise PlaybackError('unsupported_audio_extension')
    if candidate.stat().st_size == 0:
        raise PlaybackError('audio_file_empty')
    return candidate.relative_to(root).as_posix()


def identifier(value, field):
    if value is not None and not re.fullmatch(r'[A-Za-z0-9_.:-]{1,160}', value):
        raise PlaybackError('invalid_' + field)
    return value


def list_audio(raw_path, root, match=None, recursive=True, limit=20):
    root = root.expanduser().resolve()
    candidate = Path(raw_path).expanduser()
    candidate = (root / candidate) if not candidate.is_absolute() else candidate
    candidate = candidate.resolve()
    if not candidate.is_relative_to(root):
        raise PlaybackError('audio_path_outside_shared_root')
    if not candidate.is_dir():
        raise PlaybackError('music_directory_not_found')
    options = candidate.rglob('*') if recursive else candidate.iterdir()
    found, scanned = [], 0
    for path in options:
        scanned += 1
        if scanned > 10000:
            break
        if not path.is_file() or path.suffix.lower() not in EXTENSIONS:
            continue
        resolved = path.resolve()
        if not resolved.is_relative_to(root) or match and match.casefold() not in path.name.casefold():
            continue
        found.append({'path': resolved.relative_to(root).as_posix(),
                      'filename': resolved.name, 'bytes': resolved.stat().st_size})
        if len(found) >= 1000:
            break
    found.sort(key=lambda item: item['path'].casefold())
    return {'ok': True, 'operation': 'list', 'music_root': str(root),
            'count': min(len(found), limit), 'files': found[:limit],
            'truncated': len(found) > limit or scanned > 10000 or len(found) >= 1000}


def play(args, config):
    relative = select_audio(args.path, Path(config.get('music_root', DEFAULT_ROOT)),
                            args.match, args.recursive, args.random)
    request_id = identifier(args.request_id, 'request_id')
    device = identifier(args.device_key, 'device_key')
    if args.default_device and (request_id or device):
        raise PlaybackError('default_device_cannot_mix_with_turn_context')
    if not args.default_device and not request_id and not device:
        raise PlaybackError('device_or_request_required')
    key = identifier(args.idempotency_key, 'idempotency_key')
    if not key:
        key = (hashlib.sha256((request_id + '\0' + (device or '') + '\0' + relative).encode()).hexdigest()
               if request_id else uuid.uuid4().hex)
    payload = {'path': relative, 'idempotency_key': key, 'dry_run': args.dry_run}
    if request_id:
        payload['request_id'] = request_id
    if device:
        payload['device_key'] = device
    result = request(config, 'POST', '/v1/music/play', payload, args.timeout)
    result['idempotency_key'] = key
    job_id = (result.get('job') or {}).get('id')
    if args.wait and result.get('ok') and job_id:
        deadline = time.monotonic() + args.wait
        while (result.get('job') or {}).get('state') not in TERMINAL:
            if time.monotonic() >= deadline:
                return {'ok': False, 'error': 'job_wait_timeout', 'job_id': job_id,
                        'idempotency_key': key, 'last_state': (result.get('job') or {}).get('state'),
                        'retry_automatically': False}
            time.sleep(min(1.0, max(0, deadline - time.monotonic())))
            result = request(config, 'GET', '/v1/music/jobs/' + identifier(job_id, 'job_id'), timeout=args.timeout)
            result['idempotency_key'] = key
    if (result.get('job') or {}).get('state') in ('failed', 'cancelled'):
        result['ok'] = False
    # Queue acceptance and MQTT publication are not speaker/track-completion ACKs.
    result['speaker_playback_confirmed'] = False
    return result


def parser():
    result = argparse.ArgumentParser(description=__doc__)
    result.add_argument('--config', type=Path, default=DEFAULT_CONFIG)
    result.add_argument('--timeout', type=int, choices=range(3, 61), default=12)
    sub = result.add_subparsers(dest='operation', required=True)
    sub.add_parser('health')
    listing = sub.add_parser('list')
    listing.add_argument('path', nargs='?', default='.')
    listing.add_argument('--match')
    listing.add_argument('--no-recursive', action='store_true')
    listing.add_argument('--limit', type=int, choices=range(1, 101), default=20)
    status = sub.add_parser('status')
    status.add_argument('job_id')
    command = sub.add_parser('play')
    command.add_argument('path', help='Audio filename/path inside the shared music root')
    command.add_argument('--request-id')
    command.add_argument('--device-key')
    command.add_argument('--default-device', action='store_true', help='Explicit administrative choice of the configured default device')
    command.add_argument('--idempotency-key')
    command.add_argument('--dry-run', action='store_true')
    command.add_argument('--wait', type=int, choices=range(0, 121), default=0)
    command.add_argument('--match')
    command.add_argument('--recursive', action='store_true')
    command.add_argument('--random', action='store_true')
    return result


def main(argv=None):
    args = parser().parse_args(argv)
    try:
        config = private_config(args.config)
        if args.operation == 'play':
            result = play(args, config)
        elif args.operation == 'list':
            result = list_audio(args.path, Path(config.get('music_root', DEFAULT_ROOT)),
                                args.match, not args.no_recursive, args.limit)
        elif args.operation == 'status':
            result = request(config, 'GET', '/v1/music/jobs/' + identifier(args.job_id, 'job_id'), timeout=args.timeout)
        else:
            result = request(config, 'GET', '/v1/music/health', timeout=args.timeout)
    except PlaybackError as error:
        result = {'ok': False, 'error': error.code, **error.details}
    except (OSError, ValueError, TypeError):
        result = {'ok': False, 'error': 'playback_configuration_or_media_error'}
    print(json.dumps(result, ensure_ascii=False))
    return 0 if result.get('ok') else 1


if __name__ == '__main__':
    raise SystemExit(main())
