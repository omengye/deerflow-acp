"""Read existing synthetic voice reports and their exact native tool receipts.

Run on the Gentoo Docker host. This makes no model, music, browser, MQTT or HTTP
requests. Native SQLite is opened read-only inside the existing DeerFlow
container; file verification reads only the referenced shared audio files.
Missing cursors from an older failed probe are recovered by its unique synthetic
device context, never by inspecting unrelated conversation contents.
"""
import argparse
import json
from pathlib import Path
import subprocess


HELPERS = r'''
import hashlib,json,pathlib,re,sqlite3

def decode_object(text):
 try:
  value,_=json.JSONDecoder().raw_decode(text.lstrip())
  return value if isinstance(value,dict) else None
 except (ValueError,TypeError,AttributeError):return None

def context_from_event(event,device):
 text=''.join(p.get('text','') for p in event.get('content',[]) if isinstance(p,dict))
 if '本轮玩具播放上下文' not in text:return None
 decoder=json.JSONDecoder()
 for match in list(re.finditer(r'\{',text))[:128]:
  try:value,_=decoder.raw_decode(text,match.start())
  except ValueError:continue
  if isinstance(value,dict) and value.get('device_key')==device and isinstance(value.get('request_id'),str):return value
 return None

def operation(argv):
 return next((a for a in argv if a in ('search','resolve','download','list','play','status','health')),None)

def option(argv,name):
 for i,arg in enumerate(argv):
  if arg==name:return argv[i+1] if i+1<len(argv) else None
  if isinstance(arg,str) and arg.startswith(name+'='):return arg[len(name)+1:]
 return None

def safe_argv(argv):
 out=list(argv)
 for i,arg in enumerate(out):
  if arg in ('--apikey','-k','--token','--password') and i+1<len(out):out[i+1]='[REDACTED]'
  elif isinstance(arg,str) and any(arg.startswith(key+'=') for key in ('--apikey','--token','--password')):out[i]=arg.split('=',1)[0]+'=[REDACTED]'
  if isinstance(out[i],str):
   out[i]=re.sub(r'(?i)([?&](?:apikey|api_key|token|key)=)[^&\s"\']+',r'\1[REDACTED]',out[i])
   out[i]=re.sub(r'(?i)Bearer\s+[^\s"\']+','Bearer [REDACTED]',out[i])
 return out

def relative_media(value):
 if not isinstance(value,str) or not value or '\\' in value:return None
 path=pathlib.PurePosixPath(value)
 prefix=pathlib.PurePosixPath('/workspaces/folotoy/music')
 if path.is_absolute():
  try:path=path.relative_to(prefix)
  except ValueError:return None
 if '..' in path.parts or str(path) in ('','.'):return None
 return path.as_posix()

def file_check(relative,root,receipt=None):
 try:
  relative=relative_media(relative)
  if relative is None:return {'passed':False,'error':'invalid_media_path'}
  root=pathlib.Path(root).resolve();path=(root/relative).resolve()
  if not path.is_relative_to(root) or not path.is_file() or not 0<path.stat().st_size<=256*1024*1024:return {'passed':False,'error':'referenced_file_missing_or_invalid'}
  digest=hashlib.sha256()
  with path.open('rb') as source:
   for chunk in iter(lambda:source.read(1024*1024),b''):digest.update(chunk)
  out={'passed':True,'relative_path':relative,'bytes':path.stat().st_size,'sha256':digest.hexdigest()}
  if receipt is not None:out['passed']=out['bytes']==receipt.get('bytes') and out['sha256']==receipt.get('sha256')
  return out
 except (OSError,ValueError,TypeError):return {'passed':False,'error':'file_verification_failed'}

def audit_database(request,database='/var/lib/deerflow/user-data/config/go-harness-state/harness.db',media_root='/workspaces/folotoy/music'):
 device=request.get('device');full=request.get('case') in ('full','kuwo');expected_request=request.get('request_id')
 download_skill='song-download' if request.get('case')=='kuwo' else 'qq-music'
 provider='kuwo' if request.get('case')=='kuwo' else 'qq-music';code_prefix='kuwo' if provider=='kuwo' else 'qq'
 out={'case':request.get('case'),'test_device':device,'passed':False,'tools':[],'verified_searches':[],'verified_downloads':[],'verified_plays':[],'unexpected_tools':[],'errors':[]}
 if not isinstance(device,str) or not re.fullmatch('MUSICVOICE[0-9A-F]{16}',device):out['errors'].append('device_is_not_a_synthetic_voice_identity');return out
 db=sqlite3.connect(pathlib.Path(database).resolve().as_uri()+'?mode=ro',uri=True);db.row_factory=sqlite3.Row
 try:
  session=request.get('session')
  if session:
   rows=db.execute("SELECT event FROM harness_events WHERE session_id=? AND json_extract(event,'$.kind')='user_message' ORDER BY sequence DESC LIMIT 10",(session,)).fetchall()
  else:
   rows=db.execute("SELECT event FROM harness_events WHERE json_extract(event,'$.kind')='user_message' AND instr(CAST(event AS TEXT),?)>0 ORDER BY sequence DESC LIMIT 10",(device,)).fetchall()
  matches=[]
  for row in rows:
   event=json.loads(row[0]);context=context_from_event(event,device)
   if context and (not expected_request or context.get('request_id')==expected_request):matches.append((event,context))
  if len(matches)!=1:out['errors'].append('native_voice_context_missing_or_ambiguous');out['matching_context_count']=len(matches);return out
  event,context=matches[0];session=event['sessionId'];rid=event['runId'];expected_request=context['request_id']
  out.update(native_cursor={'provider':'deerFlow','sessionId':session},voice_request_id=expected_request,cursor_recovered_from_device=not bool(request.get('session')),voice_context_verified=True)
  permission=db.execute('SELECT mode,model,approval_mode FROM harness_sessions s JOIN harness_session_configs c ON c.session_id=s.id WHERE s.id=?',(session,)).fetchone()
  run=db.execute('SELECT id,status,stop_reason,created_at FROM harness_runs WHERE id=? AND session_id=?',(rid,session)).fetchone()
  out['permission']=dict(permission) if permission else None;out['run']=dict(run) if run else None
  events=[json.loads(row[0]) for row in db.execute('SELECT event FROM harness_events WHERE session_id=? AND run_id=? ORDER BY sequence',(session,rid))]
  starts={};play_count=0;search_attempts=0
  for index,event in enumerate(events):
   kind=event.get('kind');name=event.get('toolName');call=event.get('toolCallId')
   if kind=='tool_start':
    args=event.get('arguments') or {};args=args if isinstance(args,dict) else {};starts[call]=(args,index)
    if name not in ('skill','read_skill_file','skill_run','read_tool_output'):out['unexpected_tools'].append(name)
    if name=='skill_run':
     skill=args.get('name');argv=args.get('args') or [];op=operation(argv)
     if skill not in (('toy-playmusic',download_skill) if full else ('toy-playmusic',)):out['unexpected_tools'].append('unexpected_skill:'+str(skill))
     if '--default-device' in argv or any(isinstance(a,str) and a.startswith('--default-device=') for a in argv):out['errors'].append('voice_invoked_default_device')
     if skill=='toy-playmusic' and args.get('action')=='run' and op=='play':play_count+=1
     if skill==download_skill and args.get('action')=='run' and op=='search':search_attempts+=1
     out['tools'].append({'event':'start','name':name,'skill':skill,'action':args.get('action'),'path':args.get('path'),'arguments':safe_argv(argv)})
    elif name=='skill':out['tools'].append({'event':'start','name':name,'skill':args.get('skill')})
   elif kind=='tool_end' and name=='skill_run':
    args,start_index=starts.get(call,({},-1));item={'event':'end','name':name,'skill':args.get('name'),'status':event.get('status')}
    raw=''.join(p.get('text','') for p in event.get('content',[]) if isinstance(p,dict))
    result=decode_object(raw)
    if result is None:item['structured_result']=False;out['tools'].append(item);continue
    command=result.get('command') or {};stdout=command.get('stdout') or {}
    item.update(exit_code=command.get('exitCode'),termination_confirmed=command.get('terminationConfirmed'),version=result.get('version'))
    good=(event.get('status')=='completed' and result.get('name')==args.get('name') and command.get('exitCode')==0 and command.get('terminationConfirmed') is True and stdout.get('truncated') is not True)
    if args.get('action')!='run':out['tools'].append(item);continue
    value=decode_object(stdout.get('text',''))
    if value is None:item['structured_stdout']=False;out['tools'].append(item);continue
    skill=args.get('name');argv=args.get('args') or [];op=operation(argv);item.update(ok=value.get('ok'),operation=value.get('operation') or op)
    if value.get('error') in ('selected_song_not_found','no_downloadable_audio_candidates','api_request_failed','download_timeout','api_rejected'):
     item['business_error']=value['error']
    if skill==download_skill and op=='search':
     results=value.get('results');count=len(results) if isinstance(results,list) else 0;item['result_count']=count
     if good and value.get('ok') is True and value.get('provider')==provider and value.get('operation')=='search' and count:
      out['verified_searches'].append({'index':index,'count':count,'provider':provider})
    elif skill==download_skill and op=='download':
     if value.get('ok') is True:
      relative=value.get('relative_path') or value.get('path');checked=file_check(relative,media_root,value);item['file']=checked
      if good and value.get('provider')==provider and value.get('operation')=='download' and checked['passed']:
       out['verified_downloads'].append({'index':index,'file':checked})
     else:item['download_file_returned']=False
    elif skill=='toy-playmusic' and op=='play':
     job=value.get('job') or {};device_arg=option(argv,'--device-key');request_arg=option(argv,'--request-id')
     try:selected=relative_media(argv[argv.index('play')+1])
     except (ValueError,IndexError):selected=None
     checked=file_check(selected,media_root);targeted=(device_arg in (None,device) and request_arg==expected_request and job.get('device_key')==device and job.get('request_id')==expected_request)
     item.update(job={k:job.get(k) for k in ('id','state','device_key','request_id')},voice_target_verified=targeted,file=checked)
     if not targeted:out['errors'].append('play_target_did_not_match_current_voice_context')
     if good and value.get('ok') is True and targeted and checked['passed'] and isinstance(job.get('id'),str):
      out['verified_plays'].append({'index':start_index,'job':item['job'],'file':checked})
    out['tools'].append(item)
  out['play_submissions']=play_count
  if play_count!=1:out['errors'].append('expected_exactly_one_voice_play_submission')
  if not out['verified_plays']:out['errors'].append('no_verified_voice_play')
  if full:
   if not out['verified_searches']:out['errors'].append(code_prefix+'_search_returned_no_results' if search_attempts else code_prefix+'_search_was_not_called')
   if not out['verified_downloads']:out['errors'].append('no_verified_'+code_prefix+'_download')
   chain=any(s['index']<d['index']<p['index'] and d['file']['relative_path']==p['file']['relative_path'] and d['file']['sha256']==p['file']['sha256'] for s in out['verified_searches'] for d in out['verified_downloads'] for p in out['verified_plays'])
   out['search_download_play_sequence_verified']=chain
   if not chain:out['errors'].append(code_prefix+'_search_download_play_sequence_missing')
  out['passed']=bool(not out['errors'] and not out['unexpected_tools'] and permission and permission['approval_mode']=='allow_always' and run and run['status']=='completed' and run['stop_reason']=='end_turn')
  return out
 finally:db.close()
'''


def run_case(path, case):
    report = {'case': case, 'voice_report': str(path), 'passed': False}
    if not path.is_file():
        return {**report, 'error': 'voice_report_missing'}
    source = json.loads(path.read_text())
    request = {'case': case, 'device': source.get('test_device'),
               'request_id': source.get('voice_request_id'),
               'session': (source.get('native_cursor') or {}).get('sessionId')}
    engine = HELPERS + '\nimport sys\nprint(json.dumps(audit_database(json.load(sys.stdin)),ensure_ascii=False))\n'
    process = subprocess.run(['docker', 'exec', '-i', 'deerflow-deploy-deerflow-1',
                              '/opt/deerflow-skill-python/bin/python', '-c', engine],
                             input=json.dumps(request), capture_output=True, text=True, timeout=40)
    if process.returncode:
        return {**report, 'error': 'read_only_native_audit_failed'}
    native = json.loads(process.stdout)
    report['native_tool_audit'] = native
    report['voice_status'] = source.get('status')
    report['voice_failure_code'] = source.get('failure_code')
    count = source.get('audio_segments')
    orders = source.get('music_orders') or []
    job = source.get('music_job') or {}
    queue_ok = (source.get('status') == 'passed' and source.get('simulated_device') is True
                and source.get('physical_toy_playback_claimed') is False
                and source.get('music_queue_verified') is True and source.get('terminal_count') == 1
                and isinstance(count, int) and count >= 2 and orders and max(orders) == count
                and job.get('device_key') == request['device']
                and job.get('request_id') == native.get('voice_request_id')
                and job.get('state') == 'published' and job.get('audio_requested') is True
                and job.get('device_audio_requested') is False)
    report['same_recording_queue_verified'] = bool(queue_ok)
    filename = source.get('music_filename') or ''
    report['reported_music_job_matches_native'] = any(
        value['job'].get('id') == job.get('id') and filename == 'music-' + str(job.get('id')) + '.mp3'
        for value in native.get('verified_plays', []))
    if __import__('re').fullmatch(r'music-[0-9a-f]{32}\.mp3', filename):
        converted = path.parent.parent / 'audio' / filename
        report['converted_audio_exists'] = converted.is_file() and converted.stat().st_size > 0
    else:
        report['converted_audio_exists'] = False
    report['passed'] = bool(native.get('passed') and queue_ok and report['converted_audio_exists']
                            and report['reported_music_job_matches_native'])
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    base = Path('/home/gen/Documents/folotoy/deerflow-ws/src/data')
    parser.add_argument('--voice-report', type=Path, default=base / 'music-voice-verification.json')
    parser.add_argument('--full-report', type=Path, default=base / 'music-voice-full-chain.json')
    parser.add_argument('--kuwo-report', type=Path, default=base / 'music-voice-kuwo-chain.json')
    parser.add_argument('--case', choices=('local', 'full', 'kuwo', 'all'), default='all')
    args = parser.parse_args()
    cases = [('local', args.voice_report), ('full', args.full_report), ('kuwo', args.kuwo_report)]
    results = []
    for case, path in cases:
        if args.case not in ('all', case):
            continue
        try:
            results.append(run_case(path, case))
        except Exception as error:
            results.append({'case': case, 'voice_report': str(path), 'passed': False,
                            'error_type': type(error).__name__})
    output = {'read_only': True, 'new_model_or_playback_requests': False,
              'passed': bool(results) and all(value['passed'] for value in results), 'cases': results}
    print(json.dumps(output, ensure_ascii=False), flush=True)
    return 0 if output['passed'] else 1


if __name__ == '__main__':
    raise SystemExit(main())
