"""Read-only auditor regressions using synthetic SQLite receipts, no services."""
import hashlib
from contextlib import closing
import importlib.util
import json
from pathlib import Path
import sqlite3
import tempfile
import unittest

SCRIPT=Path(__file__).resolve().parents[1]/'verify-voice-native.py'
spec=importlib.util.spec_from_file_location('voice_native_audit',SCRIPT)
MODULE=importlib.util.module_from_spec(spec);spec.loader.exec_module(MODULE)
HELPERS={};exec(MODULE.HELPERS,HELPERS)


class VoiceNativeAuditTests(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory();self.addCleanup(self.temp.cleanup)
        self.root=Path(self.temp.name);self.media=self.root/'music';self.media.mkdir()
        self.file=self.media/'song.mp3';self.file.write_bytes(b'ID3test-audio')
        self.db=self.root/'harness.db';self.device='MUSICVOICE0123456789ABCDEF';self.request='voice-request-123'
        self.session='native-session-123';self.run='run-123';self.sequence=0
        with closing(sqlite3.connect(self.db)) as db, db:
            db.executescript('CREATE TABLE harness_sessions(id TEXT,mode TEXT,model TEXT); CREATE TABLE harness_session_configs(session_id TEXT,approval_mode TEXT); CREATE TABLE harness_runs(id TEXT,session_id TEXT,status TEXT,stop_reason TEXT,created_at TEXT); CREATE TABLE harness_events(sequence INTEGER,session_id TEXT,run_id TEXT,event BLOB);')
            db.execute('INSERT INTO harness_sessions VALUES (?,?,?)',(self.session,'default','model'))
            db.execute('INSERT INTO harness_session_configs VALUES (?,?)',(self.session,'allow_always'))
            db.execute('INSERT INTO harness_runs VALUES (?,?,?,?,?)',(self.run,self.session,'completed','end_turn','2026-10-24'))
        text='本轮玩具播放上下文：\n'+json.dumps({'device_key':self.device,'request_id':self.request,'role':'1'})
        self.event(kind='user_message',content=[{'type':'text','text':text}])

    def event(self,**values):
        self.sequence+=1
        value={'sessionId':self.session,'runId':self.run,**values}
        with closing(sqlite3.connect(self.db)) as db, db:db.execute('INSERT INTO harness_events VALUES (?,?,?,?)',(self.sequence,self.session,self.run,json.dumps(value).encode()))

    def tool(self,skill,argv,output):
        call='call-'+str(self.sequence)
        self.event(kind='tool_start',toolName='skill_run',toolCallId=call,
                   arguments={'name':skill,'action':'run','path':'scripts/client.py','args':argv})
        result={'name':skill,'version':'fixture','command':{'exitCode':0,'terminationConfirmed':True,'stdout':{'text':json.dumps(output),'truncated':False}}}
        self.event(kind='tool_end',toolName='skill_run',toolCallId=call,status='completed',
                   content=[{'type':'text','text':json.dumps(result)}])

    def play(self,default=False,device=None,path='song.mp3'):
        argv=['play',path,'--device-key',device or self.device,'--request-id',self.request]
        if default:argv.append('--default-device')
        self.tool('toy-playmusic',argv,{'ok':True,'job':{'id':'a'*32,'state':'preparing','device_key':device or self.device,'request_id':self.request}})

    def full(self,provider='qq-music'):
        skill='song-download' if provider=='kuwo' else 'qq-music'
        self.tool(skill,['search','song'],{'ok':True,'operation':'search','provider':provider,'results':[{'title':'song'}]})
        self.tool(skill,['download','song'],{'ok':True,'operation':'download','provider':provider,'path':'/workspaces/folotoy/music/song.mp3','relative_path':'song.mp3','bytes':self.file.stat().st_size,'sha256':hashlib.sha256(self.file.read_bytes()).hexdigest()})
        self.play()

    def audit(self,case='local',**values):
        return HELPERS['audit_database']({'case':case,'device':self.device,**values},self.db,self.media)

    def test_local_recovers_missing_cursor_using_only_exact_device_context(self):
        self.play();value=self.audit()
        self.assertTrue(value['passed']);self.assertTrue(value['cursor_recovered_from_device'])
        self.assertEqual(value['native_cursor']['sessionId'],self.session)

    def test_full_qq_search_download_play_is_ordered_and_checks_actual_file(self):
        self.full();value=self.audit('full',session=self.session,request_id=self.request)
        self.assertTrue(value['passed']);self.assertTrue(value['search_download_play_sequence_verified'])
        self.file.write_bytes(b'changed')
        self.assertFalse(self.audit('full')['passed'])

    def test_kuwo_chain_uses_kuwo_receipts(self):
        self.full('kuwo');value=self.audit('kuwo')
        self.assertTrue(value['passed']);self.assertEqual(value['verified_searches'][0]['provider'],'kuwo')

    def test_upstream_zero_results_preserves_native_correlation_and_fails_chain(self):
        self.tool('qq-music',['search','song'],{'ok':True,'operation':'search','provider':'qq-music','results':[]})
        value=self.audit('full')
        self.assertFalse(value['passed']);self.assertIn('qq_search_returned_no_results',value['errors'])
        self.assertEqual(value['native_cursor']['sessionId'],self.session)

    def test_default_device_and_wrong_voice_target_are_rejected(self):
        self.play(default=True,device='different-device');value=self.audit()
        self.assertFalse(value['passed']);self.assertIn('voice_invoked_default_device',value['errors'])
        self.assertIn('play_target_did_not_match_current_voice_context',value['errors'])

    def test_secrets_in_argv_are_redacted(self):
        argv=['search','song','--apikey','private-secret','--url=https://example.test/?token=private-token']
        value=HELPERS['safe_argv'](argv)
        self.assertNotIn('private-secret',json.dumps(value));self.assertNotIn('private-token',json.dumps(value))

    def test_failed_tool_json_prefix_survives_native_error_suffix(self):
        value=HELPERS['decode_object']('{"name":"song-download","command":{"exitCode":1}}command exited with code 1')
        self.assertEqual(value['command']['exitCode'],1)


if __name__=='__main__':unittest.main()
