"""Offline contracts for the administrative WS acceptance probe."""
from contextlib import closing, redirect_stdout
import importlib.util
import io
import json
from pathlib import Path
import sqlite3
import sys
import tempfile
import types
import unittest
from unittest.mock import patch

ROOT=Path(__file__).resolve().parents[1]
spec=importlib.util.spec_from_file_location('verify_music_ws',ROOT/'verify-ws.py')
probe=importlib.util.module_from_spec(spec);sys.modules[spec.name]=probe;spec.loader.exec_module(probe)
TEMP=ROOT.parents[2]/'.tmp/music-integration/ws-probe-tests';TEMP.mkdir(parents=True,exist_ok=True)
tempfile.tempdir=str(TEMP)


class FakeClient:
    instances=[]
    fail_prompt=False
    fail_new=False
    def __init__(self,config):
        self.config=config;self.workspace=config['workspace'];self.mode=config['ws_mode'];self.approval='allow_always'
        self.state=Path(config['state_path']);self.closed=[];self.socket_closed=False
        self.session=types.SimpleNamespace(cursor={'provider':'deerFlow','sessionId':'NATIVE_TEST'},runtime='RUNTIME_TEST',approval='allow_always',model='fixture-model')
        with closing(sqlite3.connect(self.state)) as db:
            db.execute('CREATE TABLE deerflow_ws_sessions(session_id TEXT)');db.commit()
        self.__class__.instances.append(self)
    def ensure_connected(self):return 1
    def request(self,method,params,**kwargs):
        with closing(sqlite3.connect(self.state)) as db:
            db.execute('INSERT INTO deerflow_ws_sessions VALUES(?)',('WAKU_TEST',));db.commit()
        if self.fail_new:raise RuntimeError('sentinel-daemon-secret')
        return {'sessionId':'WAKU_TEST'}
    def remember_session(self,*args):pass
    def stream_prompt(self,session_id,text,**kwargs):
        self.prompt=text;self.stream_options=kwargs
        if self.fail_prompt:raise RuntimeError('sentinel-daemon-secret')
        yield {'sessionUpdate':'agent_message_chunk','content':{'text':'报告需审计实际工具回执'}}
        yield {'_completion':{'stopReason':'end_turn'}}
    def _load(self,session_id):return self.session
    def close_session(self,session_id):self.closed.append(session_id)
    def close(self):self.socket_closed=True
    def _event(self,*args):pass


class WsProbeTests(unittest.TestCase):
    def setUp(self):
        FakeClient.instances=[];FakeClient.fail_prompt=False;FakeClient.fail_new=False
        self.sdk=types.ModuleType('sdk.deerflow_ws_client');self.sdk.DeerFlowWsClient=FakeClient
        self.environment={'DEERFLOW_WS_URL':'ws://fixture:18787/v1','WAKU_DAEMON_TOKEN':'sentinel-daemon-secret','DEERFLOW_WORKSPACE':'/workspaces/folotoy'}

    def run_probe(self,case):
        output=io.StringIO()
        with patch.dict(sys.modules,{'sdk':types.ModuleType('sdk'),'sdk.deerflow_ws_client':self.sdk}),patch.dict(probe.os.environ,self.environment,clear=True),patch.object(probe,'source_path'),redirect_stdout(output):
            code=probe.main(['--case',case])
        return code,json.loads(output.getvalue()),output.getvalue()

    def test_direct_ws_policy_native_cursor_and_cleanup_without_voice_identity(self):
        code,result,_=self.run_probe('music_search_qq')
        self.assertEqual(code,0)
        self.assertEqual(result['status'],'conversation_completed')
        self.assertFalse(result['tool_receipts_verified'])
        self.assertFalse(result['voice_turn_context_injected'])
        self.assertEqual(result['native_cursor']['sessionId'],'NATIVE_TEST')
        instance=FakeClient.instances[0]
        self.assertEqual(instance.config['ws_mode'],'fullAccess')
        self.assertEqual(instance.config['approval'],'allow_always')
        self.assertEqual(instance.closed,['WAKU_TEST'])
        self.assertTrue(instance.socket_closed)
        self.assertNotIn('本轮FoloToy',instance.prompt)
        self.assertNotIn('request_id',instance.prompt)

    def test_failure_hides_private_exception_and_releases_created_actor(self):
        FakeClient.fail_prompt=True
        code,result,raw=self.run_probe('opencli_read')
        self.assertEqual(code,1);self.assertEqual(result['error_type'],'RuntimeError')
        self.assertNotIn('sentinel-daemon-secret',raw)
        self.assertEqual(FakeClient.instances[0].closed,['WAKU_TEST'])
        self.assertTrue(result['cleanup']['passed'])

    def test_actor_recorded_before_lost_new_receipt_is_cleaned(self):
        FakeClient.fail_new=True
        code,result,_=self.run_probe('music_search_kuwo')
        self.assertEqual(code,1)
        self.assertEqual(result['error_type'],'RuntimeError')
        self.assertEqual(result['cleanup']['actors_released'],1)

    def test_admin_media_is_relative_and_default_choice_explicit(self):
        text=probe.prompt_for('music_play_admin','音乐测试/验证音乐.mp3')
        self.assertIn('--default-device',text)
        self.assertIn('省略这两参数',text)
        self.assertIn('不要下载',text)
        for media in ('../private/secret.mp3','/music/song.mp3','song.txt','C:\\secret.mp3'):
            with self.assertRaises(probe.ProbeError):probe.prompt_for('music_play_admin',media)

    def test_activity_contains_only_safe_arguments_never_raw_output(self):
        audit=probe.SafeActivities();audit.observe({'kind':'richActivity','payload':{'source_id':'call1','tool_name':'skill_run','arguments':{'name':'qq-music','args':['--apikey','sentinel-secret','search','song']},'output':'sentinel-private-result','complete':True}})
        result=audit.snapshot();raw=json.dumps(result)
        self.assertNotIn('sentinel-secret',raw);self.assertNotIn('sentinel-private-result',raw)
        self.assertTrue(result[0]['complete'])


if __name__=='__main__':unittest.main()
