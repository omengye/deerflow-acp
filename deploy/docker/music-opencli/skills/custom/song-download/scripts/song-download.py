#!/usr/bin/env python3
"""Noninteractive Kuwo search/resolve/download through the configured music API."""
import argparse
import json
from pathlib import Path
import re
import sys
from urllib.parse import urlsplit

from music_common import (DEFAULT_CONFIG, MusicError, api_key, audio_url, config,
                          download, get_api, redact, shared_path)

API_URL = 'https://api.nki.pw/API/kw.php'
QUALITY_PREF = [('mp3', '128'), ('mp3', '320'), ('mp3', '192'), ('mp3', '48'),
                ('ogg', '100'), ('ogg', '192'), ('ogg', '300'), ('aac', '48')]


def parse_search(text, data=None):
    songs = []
    if isinstance(data, list):
        for number, item in enumerate(data, 1):
            if isinstance(item, dict):
                title = item.get('name') or item.get('title') or item.get('song_title')
                if title:
                    songs.append({'index': number, 'title': str(title),
                                  'artist': str(item.get('singer') or item.get('artist') or '')})
        return songs
    for line in text.strip().splitlines():
        match = re.match(r'^(\d+)\.(.+?)-(.+)$', line.strip())
        if match:
            songs.append({'index': int(match[1]), 'title': match[2].strip(),
                          'artist': match[3].strip()})
    return songs


def search(query, key, limit=20):
    data, text = get_api(API_URL, {'msg': query, 'type': 'json', 'apikey': key})
    return parse_search(text, data)[:limit]


def tracks_from_detail(detail):
    tracks = detail.get('data', {}).get('Data', []) if isinstance(detail, dict) else []
    if not isinstance(tracks, list):
        raise MusicError('invalid_audio_candidates')
    result = []
    for index, track in enumerate(tracks):
        if not isinstance(track, dict) or not track.get('url'):
            continue
        try:
            audio_url(track['url'])
        except MusicError:
            continue
        result.append({**track, 'index': index, 'format': str(track.get('format', '')).lower(),
                       'bitrate': str(track.get('bitrate', ''))})
    return result


def pick_track(tracks, quality='auto'):
    if not tracks:
        raise MusicError('no_downloadable_audio_candidates')
    if quality not in ('', 'auto'):
        if not str(quality).isdigit():
            raise MusicError('kuwo_quality_must_be_zero_based_index')
        requested = int(quality)
        selected = next((track for track in tracks if track['index'] == requested), None)
        if selected is None:
            raise MusicError('requested_quality_unavailable')
        return selected
    for fmt, bitrate in QUALITY_PREF:
        selected = next((track for track in tracks
                         if track['format'] == fmt and track['bitrate'] == bitrate), None)
        if selected:
            return selected
    return tracks[0]


def selected_download_args(song):
    return ['download', '--song-title', song['title'], '--artist', song['artist']]


def resolve(query, key, song_index=1, *, song_title=None, artist=None):
    # A result already selected by the user/model must not depend on a second,
    # potentially changing generic search. Resolve its explicit title/artist.
    if song_title is not None or artist is not None:
        if not isinstance(song_title, str) or not isinstance(artist, str) or not song_title.strip() or not artist.strip():
            raise MusicError('explicit_selection_requires_title_and_artist')
        if (len(song_title) > 200 or len(artist) > 200
                or re.search(r'[\x00-\x1f\x7f]', song_title + artist)):
            raise MusicError('invalid_explicit_song_selection')
        selected = {'index': None, 'title': song_title.strip(), 'artist': artist.strip(),
                    'selection_source': 'explicit'}
    else:
        if not isinstance(query, str) or not query.strip():
            raise MusicError('query_or_explicit_selection_required')
        songs = search(query, key)
        selected = next((song for song in songs if song['index'] == song_index), None)
        if not selected:
            raise MusicError('selected_song_not_found')
    detail, _ = get_api(API_URL, {'msg': selected['title'] + '-' + selected['artist'],
                                 'n': '1', 'type': 'json', 'apikey': key})
    return selected, tracks_from_detail(detail)


def candidate_public(track):
    return {'index': track['index'], 'format': track['format'], 'bitrate': track['bitrate'],
            'size': str(track.get('size', '')), 'available': True}


def parser():
    value = argparse.ArgumentParser(description=__doc__)
    value.add_argument('--config', type=Path, default=DEFAULT_CONFIG)
    commands = value.add_subparsers(dest='operation', required=True)
    search_command = commands.add_parser('search')
    search_command.add_argument('query')
    search_command.add_argument('--limit', type=int, choices=range(1, 21), default=10)
    for operation in ('resolve', 'download'):
        command = commands.add_parser(operation)
        command.add_argument('query', nargs='?', default='')
        command.add_argument('--song-index', '-i', type=int, default=1)
        command.add_argument('--song-title', help='Exact title from an already selected search result')
        command.add_argument('--artist', help='Exact artist from the same selected result')
        if operation == 'download':
            command.add_argument('--quality', '-q', default='auto')
            command.add_argument('--output', '-o', default='')
            command.add_argument('--download-dir', '-d', default='')
            command.add_argument('--auto-first', action='store_true', help='Compatibility: selection is noninteractive')
            command.add_argument('--transcode', action='store_true', help='Compatibility: FoloToy handles final transcoding')
    return value


def main(argv=None):
    args = parser().parse_args(argv)
    key = ''
    try:
        configuration = config(args.config)
        key = api_key(configuration, 'song_download_api_key')
        if len(args.query) > 200 or (args.query and not args.query.strip()) or (args.operation == 'search' and not args.query.strip()):
            raise MusicError('invalid_search_query')
        if args.operation == 'search':
            songs = search(args.query, key, args.limit)
            result = {'ok': True, 'provider': 'kuwo', 'operation': 'search',
                      'query': args.query, 'count': len(songs),
                      'results': [{**item, 'download_args': selected_download_args(item)} for item in songs]}
        else:
            selected, tracks = resolve(args.query, key, args.song_index,
                                       song_title=args.song_title, artist=args.artist)
            if not tracks:
                raise MusicError('no_downloadable_audio_candidates')
            result = {'ok': True, 'provider': 'kuwo', 'operation': args.operation,
                      'selected': selected, 'candidates': [candidate_public(t) for t in tracks]}
            if args.operation == 'download':
                track = pick_track(tracks, args.quality)
                extension = '.' + track['format'] if track['format'] else Path(urlsplit(track['url']).path).suffix.lower()
                path, root = shared_path(configuration, selected['title'] + ' - ' + selected['artist'],
                                         extension, args.output, args.download_dir)
                saved = download(track['url'], path)
                result.update(saved)
                result.update(relative_path=path.relative_to(root).as_posix(),
                              selected_quality=candidate_public(track),
                              next_skill='toy-playmusic', playback_started=False)
    except MusicError as error:
        result = {'ok': False, 'error': error.code, **error.details}
    except (TypeError, ValueError, RecursionError):
        result = {'ok': False, 'error': 'invalid_music_api_response'}
    print(json.dumps(redact(result, key), ensure_ascii=False))
    return 0 if result.get('ok') else 1


if __name__ == '__main__':
    raise SystemExit(main())
