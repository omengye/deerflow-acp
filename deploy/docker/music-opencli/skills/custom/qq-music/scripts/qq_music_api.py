#!/usr/bin/env python3
"""JSON search/resolve/download client for the configured QQ Music API."""
import argparse
import json
from pathlib import Path

from music_common import (DEFAULT_CONFIG, MusicError, api_key, config, download,
                          get_api, redact, shared_path)
from qq_parsers import (ApiError, candidate_rank, infer_extension, parse_download_candidates,
                        parse_first_int, parse_search_results, select_candidate)

API_URL = 'https://api.nki.pw/API/music_open_api.php'


def search(keyword, key, limit=20):
    data, text = get_api(API_URL, {'msg': keyword, 'line': limit, 'json': '1', 'apikey': key})
    return parse_search_results(data, text)[:limit]


def public_song(item):
    return {'index': item.number, 'title': item.title, 'artist': item.artist,
            'album': item.album, 'mid': item.mid,
            'access_label': str(item.raw.get('pay', '')) if isinstance(item.raw, dict) else ''}


def public_candidate(item):
    return {'index': item.number, 'quality': item.label, 'format': item.fmt,
            'bitrate': item.bitrate, 'size': item.size, 'available': True}


def resolve(key, keyword='', song_index=None, mid='', limit=20):
    params = {'json': '1', 'apikey': key, 'line': limit}
    if keyword:
        params['msg'] = keyword
    if song_index is not None:
        params['n'] = song_index
    if mid:
        params.update(mid=mid, msg=keyword or mid)
    if not keyword and not mid:
        raise MusicError('keyword_or_mid_required')
    data, text = get_api(API_URL, params)
    return parse_download_candidates(data, text)


def choose_quality(candidates, quality='auto'):
    if not candidates:
        raise MusicError('no_downloadable_audio_candidates')
    if quality not in ('auto', ''):
        try:
            return select_candidate(candidates, quality)
        except ApiError:
            raise MusicError('requested_quality_unavailable') from None
    # Voice music defaults to a compact playable candidate, not a large FLAC.
    standard = next((item for item in candidates
                     if 'standard' in (item.label + ' ' + item.source).lower()), None)
    if standard:
        return standard
    def rank(item):
        text = (item.label + ' ' + item.fmt).lower()
        format_rank = 0 if 'mp3' in text else 1 if any(x in text for x in ('aac', 'm4a', 'low')) else 2
        bitrate = parse_first_int(item.bitrate)
        return format_rank, bitrate if bitrate else 10000, item.number
    return min(candidates, key=rank)


def selected_song(query, key, index, limit):
    items = search(query, key, limit)
    result = next((item for item in items if item.number == index), None)
    if result is None:
        raise MusicError('selected_song_not_found')
    return result


def parser():
    value = argparse.ArgumentParser(description=__doc__)
    value.add_argument('--config', type=Path, default=DEFAULT_CONFIG)
    commands = value.add_subparsers(dest='operation', required=True)
    command = commands.add_parser('search')
    command.add_argument('keyword')
    command.add_argument('--limit', '-l', type=int, choices=range(1, 21), default=10)
    for operation in ('resolve', 'download'):
        command = commands.add_parser(operation)
        command.add_argument('keyword', nargs='?', default='')
        command.add_argument('--mid', default='')
        command.add_argument('--song-index', '-i', type=int, default=1)
        command.add_argument('--limit', '-l', type=int, choices=range(1, 21), default=20)
        if operation == 'download':
            command.add_argument('--quality', '-q', default='auto')
            command.add_argument('--output', '-o', default='')
            command.add_argument('--output-dir', '-d', default='')
            command.add_argument('--auto-first', '-a', action='store_true', help='Compatibility: selection is always noninteractive')
    return value


def main(argv=None):
    args = parser().parse_args(argv)
    key = ''
    try:
        configuration = config(args.config)
        key = api_key(configuration, 'qq_music_api_key')
        if len(args.keyword) > 200 or len(getattr(args, 'mid', '')) > 100:
            raise MusicError('invalid_search_query')
        if args.operation == 'search':
            if not args.keyword.strip():
                raise MusicError('invalid_search_query')
            results = search(args.keyword, key, args.limit)
            result = {'ok': True, 'provider': 'qq-music', 'operation': 'search',
                      'query': args.keyword, 'count': len(results),
                      'results': [public_song(item) for item in results]}
        else:
            selected = None
            if args.mid:
                candidates = resolve(key, mid=args.mid, limit=args.limit)
            else:
                if not args.keyword.strip():
                    raise MusicError('keyword_or_mid_required')
                selected = selected_song(args.keyword, key, args.song_index, args.limit)
                candidates = resolve(key, args.keyword, selected.number, selected.mid, args.limit)
            if not candidates:
                raise MusicError('no_downloadable_audio_candidates')
            result = {'ok': True, 'provider': 'qq-music', 'operation': args.operation,
                      'selected': public_song(selected) if selected else {'mid': args.mid},
                      'candidates': [public_candidate(item) for item in candidates]}
            if args.operation == 'download':
                candidate = choose_quality(candidates, args.quality)
                stem = (selected.title + ' - ' + selected.artist) if selected else 'QQ音乐 - ' + args.mid
                extension = '.' + candidate.fmt.lower().strip('.') if candidate.fmt else infer_extension(candidate.url)
                path, root = shared_path(configuration, stem, extension, args.output, args.output_dir)
                result.update(download(candidate.url, path))
                result.update(relative_path=path.relative_to(root).as_posix(),
                              selected_quality=public_candidate(candidate),
                              next_skill='toy-playmusic', playback_started=False)
    except MusicError as error:
        result = {'ok': False, 'error': error.code, **error.details}
    except (ApiError, ValueError, TypeError, RecursionError):
        result = {'ok': False, 'error': 'invalid_music_api_response'}
    print(json.dumps(redact(result, key), ensure_ascii=False))
    return 0 if result.get('ok') else 1


if __name__ == '__main__':
    raise SystemExit(main())
