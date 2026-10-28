"""Offline gate tests; do not access service config, MQTT, or paid APIs."""
import contextlib
import importlib.util
import io
import json
from pathlib import Path
import sys
import tempfile
import types
import unittest
from unittest.mock import patch

ROOT=Path(__file__).resolve().parent
TEST_TEMP=ROOT.parents[2]/'.tmp/asr2-role-voice-tests'
TEST_TEMP.mkdir(parents=True,exist_ok=True)
tempfile.tempdir=str(TEST_TEMP)
SPEC=importlib.util.spec_from_file_location('role_voice_verifier',ROOT/'verify-role-voice.py')
verify=importlib.util.module_from_spec(SPEC);SPEC.loader.exec_module(verify)
DEVICE='ASR2TEST0123456789ABCDEF'
REQUEST='12345678-1234-1234-1234-123456789abc'


def report():
    return {'role':1,'scope':'role1_mqtt_udp_ws_tts','test_device':DEVICE,
            'pipeline_completed':True,'asr2_dispatch_verified':True,
            'status':'voice_chain_completed_evidence_pending','passed':False,
            'cleanup':{'durable_synthetic_identity_removed':True}}


def receipt(**changes):
    value={'provider':verify.PROVIDER,'resource_id':verify.RESOURCE_ID,'request_id':REQUEST,
           'final':True,'text':'测试通过。'};value.update(changes)
    return '2026 INFO Dkey='+DEVICE+': Transcribed:'+repr(value)


class AuditTests(unittest.TestCase):
    def test_main_receipt_is_required_to_pass(self):
        for logs in ('','only model replied 测试通过',receipt(provider='volcengine-asr')):
            result=verify.audit_main_log(report(),logs)
            self.assertFalse(result['passed']);self.assertIsNone(result['main_process_asr_receipt'])

    def test_only_exact_synthetic_device_final_asr2_receipt_passes(self):
        data=verify.audit_main_log(report(),receipt())
        self.assertTrue(data['passed']);self.assertEqual(data['status'],'passed')
        self.assertEqual(data['main_process_asr_receipt']['request_id'],REQUEST)
        self.assertNotIn('text',data['main_process_asr_receipt'])
        self.assertFalse(verify.audit_main_log(report(),receipt().replace(DEVICE,'ASR2TEST1111111111111111'))['passed'])

    def test_wrong_resource_interim_empty_or_bad_request_cannot_pass(self):
        for changes in ({'resource_id':'volc.bigasr.sauc.duration'},{'final':False},
                        {'text':''},{'request_id':'invalid'}):
            self.assertFalse(verify.audit_main_log(report(),receipt(**changes))['passed'])

    def test_duplicate_receipt_or_failed_cleanup_does_not_pass(self):
        self.assertFalse(verify.audit_main_log(report(),receipt()+'\n'+receipt())['passed'])
        data=report();data['cleanup']['durable_synthetic_identity_removed']=False
        self.assertFalse(verify.audit_main_log(data,receipt())['passed'])

    def test_audit_never_executes_log_content(self):
        logs='Dkey='+DEVICE+': Transcribed:__import__("builtins").print("do-not-execute")'
        output=io.StringIO()
        with contextlib.redirect_stdout(output):result=verify.audit_main_log(report(),logs)
        self.assertFalse(result['passed']);self.assertEqual(output.getvalue(),'')

    def test_non_synthetic_device_and_other_role_are_rejected(self):
        data=report();data['test_device']='48ca439d3b78'
        with self.assertRaises(verify.VoiceFailure):verify.audit_main_log(data,receipt())
        data=report();data['role']=2
        with self.assertRaises(verify.VoiceFailure):verify.audit_main_log(data,receipt())

    def test_run_is_explicit_and_default_does_not_read_config(self):
        output=io.StringIO()
        with patch.dict(sys.modules,{'config':None}),contextlib.redirect_stdout(output):
            code=verify.main(['--role','3'])
        data=json.loads(output.getvalue());self.assertEqual(code,0)
        self.assertEqual(data['status'],'not_run');self.assertFalse(data['passed']);self.assertEqual(data['role'],3)


class DispatchTests(unittest.TestCase):
    def invoke(self, value, *, options=None, selected=None):
        seen={};config=types.SimpleNamespace(ROLES={'3':selected or {'stt_type':verify.PROVIDER}},STT_TYPE='volcengine-asr')
        class Sdk:
            def __init__(self,c):self.config=c
            def transcribe(self,path,stt_type,stt_config):
                self_outer.assertTrue(Path(path).is_file())
                seen.update(path=path,stt_type=stt_type,options=stt_config)
                return value
        self_outer=self
        mods={'sdk':types.ModuleType('sdk'),'sdk.stt':types.ModuleType('sdk.stt'),
              'sdk.stt_sdk':types.ModuleType('sdk.stt_sdk'),'util':types.ModuleType('util'),
              'util.config_util':types.ModuleType('util.config_util')}
        mods['sdk.stt'].SttSdk=Sdk
        mods['sdk.stt_sdk'].volcengine_asr2=types.SimpleNamespace(__file__='/opt/folotoy/sdk/stt_sdk/volcengine_asr2.py')
        opts=options or {'api_key':'fixture-private-key','resource_id':verify.RESOURCE_ID,'timeout':45}
        mods['util.config_util'].get_role_stt_config=lambda *unused:opts
        with patch.dict(sys.modules,mods):
            result=verify.role_dispatch_check(config,3,b'\x00\x00'*1600,verify.time.monotonic()+60)
        return result,seen

    def test_actual_dispatch_receipt_is_checked_and_key_not_returned(self):
        value={'provider':verify.PROVIDER,'resource_id':verify.RESOURCE_ID,'request_id':REQUEST,'final':True,'text':'测试通过','elapsed_ms':123}
        result,seen=self.invoke(value)
        self.assertEqual(seen['stt_type'],verify.PROVIDER);self.assertEqual(result['request_id'],REQUEST)
        self.assertNotIn('fixture-private-key',json.dumps(result));self.assertFalse(Path(seen['path']).exists())

    def test_old_model_or_interim_output_is_not_accepted(self):
        for value in (None,{'text':'测试通过'},
                      {'provider':'volcengine-asr','resource_id':verify.RESOURCE_ID,'request_id':REQUEST,'final':True,'text':'测试通过'}):
            with self.assertRaises(verify.VoiceFailure):self.invoke(value)

    def test_hotwords_wrong_resource_or_old_role_are_rejected(self):
        for options in ({'api_key':'fixture','resource_id':'volc.bigasr.sauc.duration'},
                        {'api_key':'fixture','resource_id':verify.RESOURCE_ID,'context':'not allowed'}):
            with self.assertRaises(verify.VoiceFailure):self.invoke({},options=options)
        with self.assertRaises(verify.VoiceFailure):self.invoke({},selected={'stt_type':'volcengine-asr'})


if __name__=='__main__':unittest.main()
