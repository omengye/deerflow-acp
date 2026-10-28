"""Private API configuration and bounded, atomic shared-music downloads."""
import hashlib
import json
import os
from pathlib import Path
import re
import tempfile
import time
import urllib.parse
import uuid

DEFAULT_CONFIG = Path('/var/lib/deerflow/user-data/private/skills/music/api.json')
DEFAULT_ROOT = Path('/workspaces/folotoy/music')
MAX_API_BODY = 2 * 1024 * 1024
MAX_DOWNLOAD = 128 * 1024 * 1024
# Keep the existing client's identifier: this API can return HTTP 200 with an
# empty list for an unrecognized bot User-Agent, despite matching query/key.
USER_AGENT = 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36'
FORMATS = {'.mp3', '.m4a', '.aac', '.flac', '.wav', '.ogg', '.opus', '.ape'}


class MusicError(Exception):
    def __init__(self, code, **details):
        super().__init__(code)
        self.code, self.details = code, details


def config(path=DEFAULT_CONFIG):
    try:
        path = Path(path)
        if path.stat().st_size > 65536:
            raise MusicError('private_config_too_large')
        result = json.loads(path.read_text(encoding='utf-8'))
    except (OSError, ValueError):
        raise MusicError('private_api_config_unavailable') from None
    if not isinstance(result, dict):
        raise MusicError('invalid_private_api_config')
    return result


def api_key(configuration, name):
    value = configuration.get(name)
    if not isinstance(value, str) or not value.strip() or len(value) > 4096:
        raise MusicError('music_api_key_not_configured')
    return value.strip()


def redact(value, secret):
    if isinstance(value, dict):
        return {k: redact(v, secret) for k, v in value.items()
                if str(k).lower() not in ('apikey', 'api_key', 'token', 'authorization', 'password')}
    if isinstance(value, list):
        return [redact(v, secret) for v in value]
    if isinstance(value, str):
        return value.replace(secret, '[REDACTED]') if secret else value
    return value


def requests_module():
    try:
        import requests
        return requests
    except ImportError:
        raise MusicError('requests_dependency_missing') from None


def get_api(endpoint, params, *, session=None):
    requests = requests_module()
    owned = session is None
    session = session or requests.Session()
    started = time.monotonic()
    try:
        with session.get(endpoint, params=params,
                         headers={'User-Agent': USER_AGENT, 'Accept': 'application/json,text/plain,*/*'},
                         stream=True, timeout=(8, 25), allow_redirects=False) as response:
            if response.status_code != 200:
                raise MusicError('music_api_http_error', http_status=response.status_code)
            body = bytearray()
            for chunk in response.iter_content(65536):
                if time.monotonic() - started > 35:
                    raise MusicError('music_api_deadline_exceeded')
                body.extend(chunk)
                if len(body) > MAX_API_BODY:
                    raise MusicError('music_api_response_too_large')
        text = body.decode('utf-8-sig', 'replace').strip()
        if text.lower().startswith(('<!doctype html', '<html')):
            raise MusicError('music_api_returned_html')
        if re.search(r'(?:apikey|api.?key).{0,25}(?:无效|过期|错误|invalid)|(?:无效|过期|错误).{0,25}(?:apikey|api.?key)', text, re.I):
            raise MusicError('music_api_credential_rejected')
        try:
            value = json.loads(text)
        except ValueError:
            value = None
        if isinstance(value, dict):
            code = value.get('code', value.get('status'))
            if code not in (None, 0, '0', 200, '200'):
                raise MusicError('music_api_rejected_request')
        return value, text
    except requests.RequestException:
        # requests errors include the full API query URL; never emit them.
        raise MusicError('music_api_network_error') from None
    finally:
        if owned:
            session.close()


def safe_stem(value):
    value = re.sub(r'[<>:"/\\|?*\x00-\x1f\x7f]+', '_', str(value))
    value = re.sub(r'\s+', ' ', value).strip(' .')
    return (value or 'music')[:120]


def audio_url(value):
    if not isinstance(value, str):
        raise MusicError('invalid_audio_url')
    parsed = urllib.parse.urlsplit(value)
    if parsed.scheme not in ('http', 'https') or not parsed.hostname or parsed.username or parsed.password:
        raise MusicError('invalid_audio_url')
    return value


def shared_path(configuration, stem, extension, output='', output_dir=''):
    root = Path(configuration.get('music_root', DEFAULT_ROOT)).expanduser().resolve()
    if extension not in FORMATS:
        extension = '.mp3'
    if output:
        path = Path(output).expanduser()
        path = path if path.is_absolute() else root / path
        path = path.resolve()
    else:
        directory = Path(output_dir).expanduser() if output_dir else root
        directory = directory if directory.is_absolute() else root / directory
        directory = directory.resolve()
        path = directory / (safe_stem(stem) + ' - ' + uuid.uuid4().hex[:8] + extension)
    if not path.is_relative_to(root):
        raise MusicError('output_path_outside_shared_root')
    if path.suffix.lower() not in FORMATS:
        raise MusicError('unsupported_output_extension')
    if path.exists():
        raise MusicError('output_already_exists')
    path.parent.mkdir(parents=True, exist_ok=True)
    return path, root


def audio_header(data):
    return (data.startswith((b'ID3', b'fLaC', b'OggS', b'RIFF', b'MAC '))
            or len(data) >= 2 and data[0] == 255 and data[1] & 0xe0 == 0xe0
            or len(data) >= 12 and data[4:8] == b'ftyp')


def download(url, target, *, session=None, deadline_seconds=90, max_bytes=MAX_DOWNLOAD):
    audio_url(url)
    requests = requests_module()
    owned = session is None
    session = session or requests.Session()
    # Preserve QQ CDN compatibility: final audio fetches do not use host proxies.
    session.trust_env = False
    temporary = None
    try:
        started = time.monotonic()
        with session.get(url, headers={'User-Agent': USER_AGENT, 'Referer': 'https://y.qq.com/'},
                         stream=True, timeout=(8, 20), allow_redirects=True) as response:
            if response.status_code != 200:
                raise MusicError('audio_download_http_error', http_status=response.status_code)
            audio_url(getattr(response, 'url', url))
            try:
                declared = int(response.headers.get('content-length', 0))
            except (TypeError, ValueError):
                declared = 0
            if declared > max_bytes:
                raise MusicError('audio_download_too_large')
            content_type = response.headers.get('content-type', '').lower()
            fd, temporary = tempfile.mkstemp(prefix='.music-download-', suffix='.part', dir=target.parent)
            size = 0
            digest = hashlib.sha256()
            head = bytearray()
            with os.fdopen(fd, 'wb') as output:
                for chunk in response.iter_content(65536):
                    if time.monotonic() - started > deadline_seconds:
                        raise MusicError('audio_download_deadline_exceeded')
                    if not chunk:
                        continue
                    size += len(chunk)
                    if size > max_bytes:
                        raise MusicError('audio_download_too_large')
                    if len(head) < 512:
                        head.extend(chunk[:512 - len(head)])
                    output.write(chunk)
                    digest.update(chunk)
                output.flush()
                os.fsync(output.fileno())
            if size == 0:
                raise MusicError('audio_download_empty')
            if declared and size != declared:
                raise MusicError('audio_download_incomplete')
            prefix = bytes(head).lstrip().lower()
            if (prefix.startswith((b'<!doctype html', b'<html', b'{', b'['))
                    or ('text/' in content_type or 'json' in content_type) and not audio_header(head)):
                raise MusicError('download_is_not_audio')
            # Do not replace an existing local file or another concurrent writer.
            if target.exists():
                raise MusicError('output_already_exists')
            os.link(temporary, target)
            os.unlink(temporary)
            temporary = None
            return {'path': str(target), 'filename': target.name, 'bytes': size,
                    'sha256': digest.hexdigest(), 'audio_header_detected': audio_header(head),
                    'content_type': content_type.split(';')[0], 'transcode_required_for_toy': True}
    except requests.RequestException:
        raise MusicError('audio_download_network_error') from None
    except OSError:
        raise MusicError('audio_output_io_error') from None
    finally:
        if temporary:
            Path(temporary).unlink(missing_ok=True)
        if owned:
            session.close()
