"""QQ response parsers retained from the existing local skill (no credentials)."""
from __future__ import annotations
from dataclasses import dataclass
from pathlib import Path
import re
from typing import Any
from urllib.parse import urlparse

@dataclass
class SearchResult:
    number: int
    title: str
    artist: str = ''
    album: str = ''
    mid: str = ''
    raw: Any = None

@dataclass
class DownloadCandidate:
    number: int
    url: str
    label: str = ''
    fmt: str = ''
    bitrate: str = ''
    size: str = ''
    source: str = ''

class ApiError(RuntimeError):
    pass

def parse_search_results(data: Any | None, text: str) -> list[SearchResult]:
    if data is not None:
        items = find_song_list(data)
        if items:
            parsed = [normalize_search_item(i, item) for i, item in enumerate(items, start=1)]
            return [item for item in parsed if item.title or item.mid]
        embedded_text = find_first_text_block(data)
        if embedded_text:
            parsed = parse_text_search_results(embedded_text)
            if parsed:
                return parsed
    return parse_text_search_results(text)

def find_song_list(data: Any) -> list[Any]:
    if isinstance(data, list):
        return data
    if not isinstance(data, dict):
        return []
    preferred_keys = ('data', 'list', 'songs', 'songlist', 'song_list', 'result', 'results', 'music', 'items')
    for key in preferred_keys:
        value = get_case_insensitive(data, key)
        found = find_song_list(value)
        if looks_like_song_list(found):
            return found
    for value in data.values():
        found = find_song_list(value)
        if looks_like_song_list(found):
            return found
    return []

def looks_like_song_list(items: list[Any]) -> bool:
    if not items:
        return False
    sample = items[:5]
    for item in sample:
        if isinstance(item, str):
            return True
        if not isinstance(item, dict):
            continue
        keys = {str(k).lower() for k in item.keys()}
        if keys & {'name', 'title', 'song_title', 'song', 'songname', 'song_name', 'mid', 'songmid', 'song_mid', 'singer', 'singer_name', 'artist'}:
            return True
    return False

def normalize_search_item(number: int, item: Any) -> SearchResult:
    if isinstance(item, str):
        parsed = parse_search_line(item)
        if parsed:
            parsed.number = number
            return parsed
        return SearchResult(number=number, title=item.strip(), raw=item)
    if not isinstance(item, dict):
        return SearchResult(number=number, title=str(item), raw=item)
    explicit_number = first_value(item, ('n', 'number', 'index', 'idx', 'no', 'num', 'id'))
    try:
        number = int(str(explicit_number))
    except (TypeError, ValueError):
        pass
    title = stringify(first_value(item, ('name', 'title', 'song_title', 'songTitle', 'song', 'songname', 'song_name', 'music', 'musicname')))
    artist = stringify(first_value(item, ('singer', 'artist', 'author', 'singername', 'singer_name', 'artistname')))
    album = stringify(first_value(item, ('album', 'albumname', 'album_name')))
    mid = stringify(first_value(item, ('mid', 'songmid', 'song_mid', 'media_mid', 'mediaMid', 'rid')))
    return SearchResult(number=number, title=title, artist=artist, album=album, mid=mid, raw=item)

def parse_text_search_results(text: str) -> list[SearchResult]:
    results: list[SearchResult] = []
    for line in text.splitlines():
        parsed = parse_search_line(line)
        if parsed:
            results.append(parsed)
    return results

def parse_search_line(line: str) -> SearchResult | None:
    line = line.strip()
    if not line:
        return None
    patterns = ('^\\s*\\[?(\\d+)\\]?[\\.\\)\\]、:\\s-]+(.+?)\\s*[-|/]\\s*(.+?)\\s*$', '^\\s*(\\d+)\\s+(.+?)\\s{2,}(.+?)\\s*$', '^\\s*(\\d+)[\\.\\)\\]、:\\s-]+(.+?)\\s*$')
    for pattern in patterns:
        match = re.match(pattern, line)
        if not match:
            continue
        number = int(match.group(1))
        title = match.group(2).strip()
        artist = match.group(3).strip() if len(match.groups()) >= 3 else ''
        return SearchResult(number=number, title=title, artist=artist, raw=line)
    return None

def parse_download_candidates(data: Any | None, text: str) -> list[DownloadCandidate]:
    candidates: list[DownloadCandidate] = []
    if data is not None:
        seen: set[str] = set()
        for path, key, value, parent in walk_values(data):
            if not isinstance(value, str) or not is_probable_audio_url(value):
                continue
            if should_skip_url_key(key, path):
                continue
            if value in seen:
                continue
            seen.add(value)
            candidates.append(candidate_from_parent(len(candidates) + 1, value, key, path, parent))
    if not candidates:
        urls = re.findall('https?://[^\\s\\"\'<>]+', text)
        for url in urls:
            if is_probable_audio_url(url):
                candidates.append(DownloadCandidate(number=len(candidates) + 1, url=url, label='audio', fmt=infer_extension(url).lstrip('.'), source='text'))
    return candidates

def find_first_text_block(data: Any) -> str:
    if isinstance(data, str) and '\n' in data:
        return data
    if isinstance(data, dict):
        for key in ('data', 'msg', 'message', 'text', 'result'):
            value = get_case_insensitive(data, key)
            found = find_first_text_block(value)
            if found:
                return found
        for value in data.values():
            found = find_first_text_block(value)
            if found:
                return found
    if isinstance(data, list):
        for value in data:
            found = find_first_text_block(value)
            if found:
                return found
    return ''

def walk_values(value: Any, path: str='', parent: Any=None):
    if isinstance(value, dict):
        for key, child in value.items():
            key_text = str(key)
            child_path = f'{path}.{key_text}' if path else key_text
            yield (child_path, key_text, child, value)
            yield from walk_values(child, child_path, value)
    elif isinstance(value, list):
        for idx, child in enumerate(value):
            child_path = f'{path}[{idx}]'
            yield (child_path, str(idx), child, value)
            yield from walk_values(child, child_path, value)

def candidate_from_parent(number: int, url: str, key: str, path: str, parent: Any) -> DownloadCandidate:
    parent_dict = parent if isinstance(parent, dict) else {}
    suffix = quality_suffix_from_key(key)
    fmt = stringify(first_value(parent_dict, ('format', 'fmt', 'type', 'ext', 'filetype', 'quality')))
    bitrate = stringify(first_value(parent_dict, quality_keys('kbps', suffix) + ('bitrate', 'br', 'rate')))
    size = stringify(first_value(parent_dict, quality_keys('song_size', suffix) + ('size', 'filesize', 'file_size')))
    filename = stringify(first_value(parent_dict, quality_keys('song_filename', suffix) + ('filename', 'file', 'song_filename')))
    label = quality_label_from_suffix(suffix) or stringify(first_value(parent_dict, ('name', 'label', 'quality', 'type', 'format'))) or key
    if not fmt and filename:
        fmt = Path(filename).suffix.lstrip('.')
    if not fmt:
        fmt = infer_extension(url).lstrip('.')
    return DownloadCandidate(number=number, url=url, label=label, fmt=fmt, bitrate=bitrate, size=size, source=path)

def quality_suffix_from_key(key: str) -> str:
    if key == 'song_play_url':
        return ''
    prefix = 'song_play_url_'
    if key.startswith(prefix):
        return key[len(prefix):]
    return ''

def quality_keys(base: str, suffix: str) -> tuple[str, ...]:
    if not suffix:
        return (base,)
    return (f'{base}_{suffix}', base)

def quality_label_from_suffix(suffix: str) -> str:
    labels = {'': 'default', 'sq': 'SQ/lossless', 'hq': 'HQ', 'standard': 'standard', 'fq': 'low', 'pq': 'premium', 'accom': 'accompaniment'}
    return labels.get(suffix, suffix.upper() if suffix else '')

def is_probable_audio_url(value: str) -> bool:
    if not value.startswith(('http://', 'https://')):
        return False
    lowered = value.lower()
    if re.search('\\.(jpg|jpeg|png|gif|webp|bmp)(\\?|$)', lowered):
        return False
    if re.search('\\.(lrc|txt)(\\?|$)', lowered):
        return False
    if any((marker in lowered for marker in ('cover', 'albumcover', 'lyric', 'lrc'))):
        return False
    strong_audio_markers = ('.mp3', '.flac', '.m4a', '.aac', '.wav', '.ogg', '.ape', 'm4a?', 'mp3?', 'flac?')
    if any((marker in lowered for marker in strong_audio_markers)):
        return True
    return any((token in lowered for token in ('/amobile.music.tc.qq.com/', '/dl.stream.qqmusic.qq.com/', '/isure.stream.qqmusic.qq.com/', '/music/', '/song/')))

def should_skip_url_key(key: str, path: str) -> bool:
    lowered = f'{key} {path}'.lower()
    return any((token in lowered for token in ('pic', 'image', 'cover', 'lyric', 'lrc', 'avatar')))

def select_candidate(candidates: list[DownloadCandidate], quality: str='') -> DownloadCandidate:
    if not candidates:
        raise ApiError('no downloadable audio URL found in API response')
    if quality:
        q = quality.strip().lower()
        if q.isdigit():
            requested = int(q)
            for candidate in candidates:
                if candidate.number == requested:
                    return candidate
        for candidate in candidates:
            haystack = ' '.join([candidate.label, candidate.fmt, candidate.bitrate, candidate.size, candidate.source]).lower()
            if q in haystack:
                return candidate
        raise ApiError(f'quality not found: {quality}')
    ranked = sorted(candidates, key=candidate_rank, reverse=True)
    return ranked[0]

def candidate_rank(candidate: DownloadCandidate) -> tuple[int, int, int]:
    text = ' '.join([candidate.label, candidate.fmt, candidate.bitrate]).lower()
    bitrate = parse_first_int(candidate.bitrate)
    format_score = 0
    if 'flac' in text or 'lossless' in text:
        format_score = 5
    elif '320' in text:
        format_score = 4
    elif 'm4a' in text or 'aac' in text:
        format_score = 3
    elif 'mp3' in text:
        format_score = 2
    return (format_score, bitrate, -candidate.number)

def infer_extension(url: str) -> str:
    path = urlparse(url).path.lower()
    suffix = Path(path).suffix
    if suffix and re.fullmatch('\\.[a-z0-9]{2,5}', suffix):
        return suffix
    for ext in ('.flac', '.mp3', '.m4a', '.aac', '.wav', '.ogg', '.ape'):
        if ext.lstrip('.') in url.lower():
            return ext
    return '.mp3'

def first_value(data: dict[str, Any], keys: tuple[str, ...]) -> Any:
    for key in keys:
        value = get_case_insensitive(data, key)
        if value not in (None, ''):
            return value
    return None

def get_case_insensitive(data: Any, key: str) -> Any:
    if not isinstance(data, dict):
        return None
    if key in data:
        return data[key]
    lowered = key.lower()
    for actual_key, value in data.items():
        if str(actual_key).lower() == lowered:
            return value
    return None

def stringify(value: Any) -> str:
    if value is None:
        return ''
    if isinstance(value, (dict, list)):
        return ''
    return str(value).strip()

def parse_first_int(value: str) -> int:
    match = re.search('\\d+', value or '')
    return int(match.group(0)) if match else 0
