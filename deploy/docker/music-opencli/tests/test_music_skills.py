"""Offline behavioral regressions for migrated music clients; no network or playback."""
from contextlib import redirect_stdout
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import sys
import tempfile
import types
import unittest
from unittest.mock import patch
import urllib.error

BASE = Path(__file__).resolve().parents[1] / 'skills/custom'
TEST_TEMP = Path(__file__).resolve().parents[4] / '.tmp/music-integration/music-skill-tests'
TEST_TEMP.mkdir(parents=True, exist_ok=True)
tempfile.tempdir = str(TEST_TEMP)
SONG = BASE / 'song-download/scripts'
QQ = BASE / 'qq-music/scripts'
sys.path[:0] = [str(SONG), str(QQ)]


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


common = load('music_common', SONG / 'music_common.py')
song = load('kuwo_client_test', SONG / 'song-download.py')
qq = load('qq_client_test', QQ / 'qq_music_api.py')
toy = load('toy_client_test', BASE / 'toy-playmusic/scripts/toy-playmusic.py')


class RequestFailure(Exception):
    pass


class Response:
    def __init__(self, body=b'ID3audio-content', status=200, headers=None):
        self.body, self.status_code = body, status
        self.headers = headers or {'content-type': 'audio/mpeg', 'content-length': str(len(body))}
        self.url = 'https://cdn.example/music/test.mp3'
    def __enter__(self): return self
    def __exit__(self, *unused): return False
    def iter_content(self, *args, **kwargs):
        yield self.body


class Session:
    def __init__(self, response=None, error=None):
        self.response, self.error, self.calls = response or Response(), error, []
    def get(self, *args, **kwargs):
        self.calls.append((args, kwargs))
        if self.error: raise self.error
        return self.response
    def close(self): pass


FAKE_REQUESTS = types.SimpleNamespace(RequestException=RequestFailure)


class MusicTransportTests(unittest.TestCase):
    def test_config_has_no_embedded_or_environment_fallback(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'api.json'
            with self.assertRaises(common.MusicError):common.config(path)
            path.write_text(json.dumps({'song_download_api_key': 'fixture-key'}))
            self.assertEqual(common.api_key(common.config(path), 'song_download_api_key'), 'fixture-key')
            with self.assertRaises(common.MusicError):common.api_key({}, 'qq_music_api_key')

    def test_request_error_never_contains_secret_or_full_query(self):
        session = Session(error=RequestFailure('https://api.example/?apikey=fixture-sensitive-value'))
        with patch.object(common, 'requests_module', return_value=FAKE_REQUESTS):
            with self.assertRaises(common.MusicError) as caught:
                common.get_api('https://api.example/', {'apikey': 'fixture-sensitive-value'}, session=session)
        self.assertEqual(caught.exception.code, 'music_api_network_error')
        self.assertNotIn('fixture-sensitive-value', str(caught.exception))
        self.assertEqual(len(session.calls), 1)

    def test_api_html_or_credential_errors_are_not_empty_searches(self):
        for body, code in [(b'<html>challenge</html>', 'music_api_returned_html'),
                           ('apikey 已过期'.encode(), 'music_api_credential_rejected')]:
            with patch.object(common, 'requests_module', return_value=FAKE_REQUESTS):
                with self.assertRaises(common.MusicError) as caught:
                    common.get_api('https://api.example/', {}, session=Session(Response(body)))
            self.assertEqual(caught.exception.code, code)

    def test_output_is_confined_and_title_is_sanitized(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / 'music'
            path, actual = common.shared_path({'music_root': str(root)}, '../../bad/title:*?', '.mp3')
            self.assertEqual(actual, root.resolve())
            self.assertEqual(path.parent, root.resolve())
            self.assertNotIn('/', path.name)
            with self.assertRaises(common.MusicError):
                common.shared_path({'music_root': str(root)}, 'song', '.mp3', output='../escape.mp3')

    def test_atomic_download_hash_and_proxy_policy(self):
        body = b'ID3' + b'audio' * 100
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / 'song.mp3';session = Session(Response(body))
            with patch.object(common, 'requests_module', return_value=FAKE_REQUESTS):
                result = common.download('https://cdn.example/song.mp3?temporary=redacted', target, session=session)
            self.assertEqual(target.read_bytes(), body)
            self.assertEqual(result['sha256'], hashlib.sha256(body).hexdigest())
            self.assertEqual(result['bytes'], len(body))
            self.assertTrue(result['audio_header_detected'])
            self.assertFalse(session.trust_env)
            self.assertNotIn('url', result)
            self.assertEqual(list(Path(directory).glob('*.part')), [])

    def test_download_error_retains_no_partial_file(self):
        cases = [(Response(b'<html>Access denied</html>', headers={'content-type':'text/html'}), 'download_is_not_audio'),
                 (Response(b'ID3', headers={'content-length':'99'}), 'audio_download_incomplete'),
                 (Response(b''), 'audio_download_empty'),
                 (Response(b'ID3content'), 'audio_download_too_large')]
        for response, expected in cases:
            with tempfile.TemporaryDirectory() as directory:
                target = Path(directory) / 'audio.mp3'
                limit = 4 if expected == 'audio_download_too_large' else 4096
                with patch.object(common, 'requests_module', return_value=FAKE_REQUESTS):
                    with self.assertRaises(common.MusicError) as caught:
                        common.download('https://cdn.example/song.mp3', target, session=Session(response), max_bytes=limit)
                self.assertEqual(caught.exception.code, expected)
                self.assertFalse(target.exists())
                self.assertEqual(list(Path(directory).iterdir()), [])

    def test_existing_download_is_not_overwritten(self):
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / 'song.mp3';target.write_bytes(b'existing')
            with patch.object(common, 'requests_module', return_value=FAKE_REQUESTS):
                with self.assertRaises(common.MusicError):
                    common.download('https://cdn.example/song.mp3', target, session=Session())
            self.assertEqual(target.read_bytes(), b'existing')

    def test_api_body_limit_and_no_redirect(self):
        session = Session(Response(b'123456'))
        with patch.object(common, 'requests_module', return_value=FAKE_REQUESTS), patch.object(common, 'MAX_API_BODY', 4):
            with self.assertRaises(common.MusicError):common.get_api('https://api.example/', {}, session=session)
        self.assertFalse(session.calls[0][1]['allow_redirects'])
        self.assertIn('Chrome/126.0',session.calls[0][1]['headers']['User-Agent'])
        self.assertNotIn('DeerFlow-Music',session.calls[0][1]['headers']['User-Agent'])


class MusicParserTests(unittest.TestCase):
    def test_kuwo_search_and_zero_based_quality(self):
        result = song.parse_search('1.晴天-周杰伦\n2.晴天（翻唱）-歌手\n共2首')
        self.assertEqual(result[0], {'index':1,'title':'晴天','artist':'周杰伦'})
        tracks = song.tracks_from_detail({'data':{'Data':[{'url':'https://cdn.example/a.flac','format':'flac','bitrate':1000},
                                                               {'url':'https://cdn.example/a.mp3','format':'mp3','bitrate':128}]}})
        self.assertEqual(song.pick_track(tracks)['index'], 1)
        self.assertEqual(song.pick_track(tracks, '0')['format'], 'flac')
        self.assertNotIn('url', song.candidate_public(tracks[0]))

    def test_kuwo_explicit_selection_resolves_without_second_search(self):
        detail={'data':{'Data':[{'url':'https://cdn.example/song.mp3','format':'mp3','bitrate':128}]}}
        with patch.object(song,'search',side_effect=AssertionError('must not repeat search')),patch.object(song,'get_api',return_value=(detail,'')) as request:
            selected,tracks=song.resolve('', 'fixture-key', song_title='晴天', artist='周杰伦')
        self.assertEqual((selected['title'],selected['artist']),('晴天','周杰伦'))
        self.assertEqual(selected['selection_source'],'explicit')
        self.assertEqual(len(tracks),1)
        self.assertEqual(request.call_args.args[1],{'msg':'晴天-周杰伦','n':'1','type':'json','apikey':'fixture-key'})

    def test_kuwo_incomplete_selection_rejected_without_api_or_search(self):
        cases=[{'song_title':'晴天'}, {'artist':'周杰伦'}, {'song_title':'晴天','artist':'  '},
               {'song_title':'x'*201,'artist':'artist'}, {'song_title':'song\nother','artist':'artist'}]
        for selected in cases:
            with patch.object(song,'search',side_effect=AssertionError('no fallback search')),patch.object(song,'get_api',side_effect=AssertionError('no API')):
                with self.assertRaises(common.MusicError):song.resolve('晴天','fixture-key',**selected)

    def test_kuwo_search_provides_reusable_selected_download_arguments(self):
        with tempfile.TemporaryDirectory() as directory:
            configuration=Path(directory)/'api.json';configuration.write_text(json.dumps({'song_download_api_key':'fixture-private-key'}))
            output=io.StringIO()
            with patch.object(song,'search',return_value=[{'index':4,'title':'晴天（现场）','artist':'周杰伦'}]),redirect_stdout(output):
                code=song.main(['--config',str(configuration),'search','晴天'])
            self.assertEqual(code,0)
            argv=json.loads(output.getvalue())['results'][0]['download_args']
            self.assertEqual(argv,['download','--song-title','晴天（现场）','--artist','周杰伦'])
            args=song.parser().parse_args(argv)
            self.assertEqual(args.query,'')
            self.assertEqual((args.song_title,args.artist),('晴天（现场）','周杰伦'))

    def test_qq_nested_known_field_names_and_plain_text(self):
        nested={'data':{'songs':[{'song_title':'晴天','singer_name':'周杰伦','song_mid':'mid123'}]}}
        result=qq.parse_search_results(nested,'')
        self.assertEqual((result[0].title,result[0].artist,result[0].mid),('晴天','周杰伦','mid123'))
        result=qq.parse_search_results(None,'1.晴天-周杰伦\n2.歌曲-歌手')
        self.assertEqual(len(result),2)

    def test_qq_top_level_live_shape_survives_search(self):
        data=[{'song_title':'晴天','singer_name':'周杰伦','song_mid':'mid1','pay':'付费'},
              {'song_title':'晴天 (交响改编版)','singer_name':'维也纳碎片','song_mid':'mid2','pay':'免费'},
              {'song_title':'晴天 (Live)','singer_name':'周杰伦','song_mid':'mid3','pay':'付费'}]
        with patch.object(qq,'get_api',return_value=(data,json.dumps(data,ensure_ascii=False))):
            result=qq.search('晴天','fixture-key',3)
        self.assertEqual(len(result),3)
        self.assertEqual(qq.public_song(result[1])['access_label'],'免费')

    def test_qq_candidates_ignore_cover_and_select_standard(self):
        payload={'data':{'song_play_url':'https://cdn.example/music/main.flac','kbps':1000,
                         'song_play_url_standard':'https://cdn.example/music/standard.mp3','kbps_standard':128,
                         'cover':'https://cdn.example/cover.jpg','lyric':'https://cdn.example/song.lrc'}}
        candidates=qq.parse_download_candidates(payload,'')
        self.assertEqual(len(candidates),2)
        selected=qq.choose_quality(candidates)
        self.assertIn('standard',selected.label)
        self.assertNotIn('url',qq.public_candidate(selected))
        with self.assertRaises(common.MusicError):qq.choose_quality(candidates,'unavailable')

    def test_cli_json_output_redacts_secret_returned_by_upstream(self):
        with tempfile.TemporaryDirectory() as directory:
            configuration=Path(directory)/'api.json';configuration.write_text(json.dumps({'song_download_api_key':'fixture-private-key'}))
            output=io.StringIO()
            with patch.object(song,'search',return_value=[{'index':1,'title':'fixture-private-key','artist':'artist'}]),redirect_stdout(output):
                code=song.main(['--config',str(configuration),'search','song'])
            self.assertEqual(code,0);self.assertTrue(json.loads(output.getvalue())['ok'])
            self.assertNotIn('fixture-private-key',output.getvalue())

    def test_no_credentials_or_legacy_home_paths_in_skill_payload(self):
        for name in ('song-download','qq-music','toy-playmusic'):
            folder=BASE/name;manifest=json.loads((folder/'skill-runtime.json').read_text())
            self.assertEqual(manifest['version'],1)
            for path in folder.rglob('*'):
                if path.is_file() and '__pycache__' not in path.parts:
                    text=path.read_text(encoding='utf-8')
                    self.assertNotIn('[PRIVATE_API_KEY]',text)
                    self.assertNotIn('[PRIVATE_RUNTIME_CONFIG]',text)
                    self.assertNotIn('BEGIN OPENSSH PRIVATE KEY',text)


class ToyClientTests(unittest.TestCase):
    def test_private_token_is_required_and_not_in_url(self):
        with tempfile.TemporaryDirectory() as directory:
            p=Path(directory)/'playback.json';p.write_text(json.dumps({'url':'http://host:8090','token':'fixture-token-123456789'}))
            result=toy.private_config(p);self.assertEqual(result['url'],'http://host:8090')
            p.write_text(json.dumps({'url':'http://user:password@host:8090','token':'fixture-token-123456789'}))
            with self.assertRaises(toy.PlaybackError):toy.private_config(p)

    def test_shared_file_selection_and_traversal(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory)/'music';root.mkdir();(root/'a.mp3').write_bytes(b'ID3x');(root/'b.flac').write_bytes(b'fLaCx')
            self.assertEqual(toy.select_audio('.',root,match='b'),'b.flac')
            with self.assertRaises(toy.PlaybackError):toy.select_audio('../escape.mp3',root)

    def test_local_listing_discovers_nested_music_and_never_requests_playback(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory)/'music';nested=root/'音乐测试';nested.mkdir(parents=True)
            (nested/'验证音乐.mp3').write_bytes(b'ID3x');(root/'other.txt').write_text('ignored')
            listing=toy.list_audio('.',root,match='验证音乐')
            self.assertEqual(listing['files'],[{'path':'音乐测试/验证音乐.mp3','filename':'验证音乐.mp3','bytes':4}])
            self.assertEqual(listing['count'],1)
            with self.assertRaises(toy.PlaybackError):toy.list_audio('..',root)

    def test_voice_retries_keep_same_idempotency_and_do_not_claim_speaker_ack(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);(root/'song.mp3').write_bytes(b'ID3music')
            args=toy.parser().parse_args(['play','song.mp3','--device-key','device123','--request-id','voice-request'])
            configuration={'url':'http://host:8090','token':'fixture-token-123456789','music_root':str(root)}
            sent=[]
            def fake(config,method,path,payload=None,timeout=12):
                sent.append(payload);return {'ok':True,'job':{'id':'job1','state':'queued'}}
            with patch.object(toy,'request',side_effect=fake):
                first=toy.play(args,configuration);second=toy.play(args,configuration)
            self.assertEqual(sent[0]['idempotency_key'],sent[1]['idempotency_key'])
            self.assertFalse(first['speaker_playback_confirmed'])
            self.assertEqual(first['job']['state'],'queued')
            self.assertEqual(second['job']['state'],'queued')

    def test_implicit_default_is_rejected_and_admin_opt_in_is_explicit(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);(root/'song.mp3').write_bytes(b'ID3music')
            configuration={'url':'http://host:8090','token':'fixture-token-123456789','music_root':str(root)}
            with self.assertRaises(toy.PlaybackError) as caught:
                toy.play(toy.parser().parse_args(['play','song.mp3']),configuration)
            self.assertEqual(caught.exception.code,'device_or_request_required')
            args=toy.parser().parse_args(['play','song.mp3','--default-device'])
            with patch.object(toy,'request',return_value={'ok':True,'job':{'id':'job','state':'queued'}}) as request:
                toy.play(args,configuration)
            body=request.call_args.args[3]
            self.assertNotIn('device_key',body);self.assertNotIn('request_id',body)

    def test_lost_post_receipt_is_not_retried(self):
        class Opener:
            count=0
            def open(self,*args,**kwargs):
                self.count+=1;raise urllib.error.URLError('fixture-token-123456789')
        opener=Opener()
        with self.assertRaises(toy.PlaybackError) as caught:
            toy.request({'url':'http://host:8090','token':'fixture-token-123456789'},'POST','/v1/music/play',{},opener=opener)
        self.assertEqual(opener.count,1)
        self.assertFalse(caught.exception.details['retry_automatically'])
        self.assertNotIn('fixture-token',str(caught.exception))

    def test_response_drops_credentials_and_redirects(self):
        result=toy.public_result({'ok':True,'token':'fixture-token','job':{'error':'fixture-token'}},'fixture-token')
        self.assertNotIn('token',result);self.assertEqual(result['job']['error'],'[REDACTED]')
        self.assertIsNone(toy.NoRedirect().redirect_request(None,None,302,'',{},'https://other.example/'))


if __name__=='__main__':
    unittest.main()
