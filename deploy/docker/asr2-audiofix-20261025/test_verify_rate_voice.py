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
import wave
from unittest.mock import patch

ROOT=Path(__file__).resolve().parent
TEST_TEMP=ROOT.parents[2]/'.tmp/asr2-rate-voice-tests'
TEST_TEMP.mkdir(parents=True,exist_ok=True)
tempfile.tempdir=str(TEST_TEMP)
SPEC=importlib.util.spec_from_file_location('rate_voice_verifier',ROOT/'verify-rate-voice.py')
verify=importlib.util.module_from_spec(SPEC);SPEC.loader.exec_module(verify)
DEVICE='ASR2TEST0123456789ABCDEF'
REQUEST='12345678-1234-1234-1234-123456789abc'


def report():
    return {'role':1,'scope':'role1_mqtt_udp_ws_tts','test_device':DEVICE,
            'pipeline_completed':True,'asr2_dispatch_verified':True,
            'input_sample_rate':32000,'device_wire_sample_rate':32000,
            'device_wire_frame_type_hex':'01','asr_dispatch':{'input_sample_rate':32000},
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
        self.assertEqual(data['input_sample_rate'],32000)

    def test_rate_path_evidence_is_required_for_voice_gate(self):
        for changes in ({'device_wire_sample_rate':16000}, {'device_wire_frame_type_hex':'04'},
                        {'asr_dispatch':{'input_sample_rate':16000}}, {'input_sample_rate':44100}):
            data=report();data.update(changes)
            self.assertFalse(verify.audit_main_log(data,receipt())['passed'])


class DispatchTests(unittest.TestCase):
    def invoke(self, value, *, options=None, selected=None,sample_rate=32000):
        seen={};config=types.SimpleNamespace(ROLES={'3':selected or {'stt_type':verify.PROVIDER}},STT_TYPE='volcengine-asr')
        class Sdk:
            def __init__(self,c):self.config=c
            def transcribe(self,path,stt_type,stt_config):
                self_outer.assertTrue(Path(path).is_file())
                with wave.open(str(path),'rb') as audio:
                    seen.update(input_rate=audio.getframerate(),channels=audio.getnchannels(),width=audio.getsampwidth(),frames=audio.getnframes())
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
            result=verify.role_dispatch_check(config,3,b'\x00\x00'*(sample_rate//10),verify.time.monotonic()+60,sample_rate)
        return result,seen

    def test_actual_dispatch_receipt_is_checked_and_key_not_returned(self):
        value={'provider':verify.PROVIDER,'resource_id':verify.RESOURCE_ID,'request_id':REQUEST,'final':True,'text':'测试通过','elapsed_ms':123}
        result,seen=self.invoke(value)
        self.assertEqual(seen['stt_type'],verify.PROVIDER);self.assertEqual(result['request_id'],REQUEST)
        self.assertNotIn('fixture-private-key',json.dumps(result));self.assertFalse(Path(seen['path']).exists())

    def test_dispatch_wav_preserves_each_actual_input_rate(self):
        value={'provider':verify.PROVIDER,'resource_id':verify.RESOURCE_ID,'request_id':REQUEST,'final':True,'text':'测试通过'}
        for rate in (8000,16000,32000):
            with self.subTest(rate=rate):
                result,seen=self.invoke(value,sample_rate=rate)
                self.assertEqual((seen['input_rate'],seen['channels'],seen['width']),(rate,1,2))
                self.assertEqual(result['input_sample_rate'],rate)
                self.assertAlmostEqual(result['input_duration_seconds'],.1)

    def test_old_model_or_interim_output_is_not_accepted(self):
        for value in (None,{'text':'测试通过'},
                      {'provider':'volcengine-asr','resource_id':verify.RESOURCE_ID,'request_id':REQUEST,'final':True,'text':'测试通过'}):
            with self.assertRaises(verify.VoiceFailure):self.invoke(value)

    def test_hotwords_wrong_resource_or_old_role_are_rejected(self):
        for options in ({'api_key':'fixture','resource_id':'volc.bigasr.sauc.duration'},
                        {'api_key':'fixture','resource_id':verify.RESOURCE_ID,'context':'not allowed'}):
            with self.assertRaises(verify.VoiceFailure):self.invoke({},options=options)
        with self.assertRaises(verify.VoiceFailure):self.invoke({},selected={'stt_type':'volcengine-asr'})


class DeviceRateTests(unittest.TestCase):
    def test_real_udp_frame_types_and_pcm_pacing_are_bound_to_rate(self):
        for rate,frame_type in ((32000,b'\x01'),(16000,b'\x04'),(8000,b'\x05')):
            sent=[];sleeps=[]
            udp=types.SimpleNamespace(sendto=lambda frame,destination:sent.append((frame,destination)))
            token=bytes(range(16));recording=1234;pcm=b'\x00\x01'*(rate//10)
            with patch.object(verify.time,'sleep',side_effect=sleeps.append):
                packets=verify.upload_pcm(udp,('127.0.0.1',8085),token,recording,pcm,rate,verify.time.monotonic()+60)
            prefix=token+recording.to_bytes(2,'big')
            self.assertEqual(sent[0][0],prefix+b'\x02\x01')
            self.assertEqual(sent[-1][0],prefix+b'\x02\x02')
            audio=[frame for frame,_ in sent[1:-1]]
            self.assertEqual(len(audio),packets)
            self.assertTrue(all(frame[:19]==prefix+frame_type for frame in audio))
            self.assertEqual(b''.join(frame[19:] for frame in audio),pcm)
            self.assertAlmostEqual(sum(sleeps[1:]),.1)
            self.assertEqual(sleeps[0],.1)
            self.assertEqual(sleeps[1],min(1280,len(pcm))/(rate*2))

    def test_invalid_rate_or_pcm_never_sends_device_commands(self):
        sent=[];udp=types.SimpleNamespace(sendto=lambda *args:sent.append(args))
        for rate,pcm in ((44100,b'\x00\x00'),(32000,b'\x00'),(32000,b'')):
            with self.assertRaises(verify.VoiceFailure):
                verify.upload_pcm(udp,('127.0.0.1',8085),b'x'*16,1,pcm,rate,verify.time.monotonic()+10)
        self.assertEqual(sent,[])

    def test_conversion_uses_actual_rate_and_rate_specific_duration_bounds(self):
        for rate in (8000,16000,32000):
            pcm=b'\x00\x00'*(rate//10)
            with patch.object(verify.subprocess,'run',return_value=types.SimpleNamespace(returncode=0,stdout=pcm)) as run:
                self.assertEqual(verify.convert_pcm(Path('test-source.mp3'),rate,verify.time.monotonic()+60),pcm)
                argv=run.call_args.args[0]
                self.assertEqual(argv[argv.index('-ar')+1],str(rate))
                self.assertEqual(argv[argv.index('-ac')+1],'1')
            for bad in (b'',b'\x00',b'\x00\x00'*(rate*20+1)):
                with patch.object(verify.subprocess,'run',return_value=types.SimpleNamespace(returncode=0,stdout=bad)):
                    with self.assertRaises(verify.VoiceFailure):
                        verify.convert_pcm(Path('test-source.mp3'),rate,verify.time.monotonic()+60)


if __name__=='__main__':unittest.main()
