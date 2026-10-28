"""Run/reuse one administrative WS conversation and verify its native receipts.

PROBE_CODE/TEST_CASE may be supplied in memory by the SSH helper. Alternatively
use --probe-file, or --conversation to audit a saved conversation without making
another model call. Private authentication values are read only in memory.
"""
import argparse
import json
from pathlib import Path
import re
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request

CASES = ('music_search_kuwo', 'music_search_qq', 'music_download_qq',
         'opencli_doctor', 'opencli_read', 'music_play_admin')
ROOT = Path('/home/gen/deerflow-deploy')

AUDIT = r'''
import hashlib,json,pathlib,re,sqlite3,sys
request=json.load(sys.stdin);case=request['case'];sid=request['session'];media=request.get('media','')
database=pathlib.Path('/var/lib/deerflow/user-data/config/go-harness-state/harness.db')
db=sqlite3.connect(database.as_uri()+'?mode=ro',uri=True);db.row_factory=sqlite3.Row
row=db.execute('SELECT mode,model,approval_mode FROM harness_sessions s JOIN harness_session_configs c ON c.session_id=s.id WHERE s.id=?',(sid,)).fetchone()
run=db.execute('SELECT id,status,stop_reason FROM harness_runs WHERE session_id=? ORDER BY created_at DESC LIMIT 1',(sid,)).fetchone()
out={'native_session':sid,'database':str(database),'permission':dict(row) if row else None,'run':dict(run) if run else None,'tools':[],'verified_results':[],'unexpected_tools':[],'play_submissions':0,'passed':False}
if not row or not run:print(json.dumps(out));sys.exit(0)
events=[json.loads(r[0]) for r in db.execute('SELECT event FROM harness_events WHERE run_id=? ORDER BY sequence',(run['id'],))]
starts={};expected={'music_search_kuwo':'song-download','music_search_qq':'qq-music','music_download_qq':'qq-music','music_play_admin':'toy-playmusic'}.get(case,'opencli')
def operation(argv):return next((value for value in argv if value in ('search','resolve','download','list','play','status','health')),None)
def plain(text):return re.sub(r'\x1b\[[0-?]*[ -/]*[@-~]','',text)
def positive_line(text,subject,positive):
 return any(re.search(subject,line,re.I) and re.search(positive,line,re.I) and not re.search(r'not\s+(?:running|connected)|disconnected|not\s+ready',line,re.I) for line in plain(text).splitlines())
def collection(value,depth=0):
 if depth>6:return []
 if isinstance(value,list):return value
 if isinstance(value,dict):
  for key in ('rows','data','items','results','entries','records','list','videos','hot','trending'):
   if key in value:
    found=collection(value[key],depth+1)
    if found:return found
 return []
def file_check(value):
 try:
  p=pathlib.Path(value.get('path','')).resolve();root=pathlib.Path('/workspaces/folotoy/music').resolve()
  if not p.is_relative_to(root) or not p.is_file() or not 0<p.stat().st_size<=128*1024*1024:return {'passed':False}
  digest=hashlib.sha256()
  with p.open('rb') as f:
   for chunk in iter(lambda:f.read(1024*1024),b''):digest.update(chunk)
  actual={'path':str(p),'relative_path':p.relative_to(root).as_posix(),'bytes':p.stat().st_size,'sha256':digest.hexdigest()}
  return {**actual,'passed':actual['bytes']==value.get('bytes') and actual['sha256']==value.get('sha256')}
 except (OSError,ValueError,TypeError):return {'passed':False}
for event in events:
 name=event.get('toolName');kind=event.get('kind');call=event.get('toolCallId')
 if kind=='tool_start':
  args=event.get('arguments') or {};starts[call]=args
  if name not in ('skill','read_skill_file','skill_run','host_opencli','read_tool_output'):out['unexpected_tools'].append(name)
  if name=='skill_run' and args.get('name')!=expected:out['unexpected_tools'].append('unexpected_skill_run:'+str(args.get('name')))
  if name=='skill_run' and args.get('action')=='run':
   op=operation(args.get('args') or [])
   if case.startswith('music_search') and op in ('download','play'):out['unexpected_tools'].append('unexpected_action:'+str(op))
   if case=='music_download_qq' and op=='play':out['unexpected_tools'].append('unexpected_play')
   if case=='music_play_admin' and op=='play':out['play_submissions']+=1
  safe={k:args[k] for k in ('skill','name','action','path','args','site','command','arguments') if k in args} if name in ('skill','read_skill_file','skill_run','host_opencli') else {}
  out['tools'].append({'event':'start','name':name,'arguments':safe})
 elif kind=='tool_end':
  args=starts.get(call,{});item={'event':'end','name':name,'status':event.get('status'),'failure_code':(event.get('receipt') or {}).get('failureCode')}
  raw=''.join(p.get('text','') for p in event.get('content',[]) if isinstance(p,dict))
  if name in ('skill_run','host_opencli'):
   try:result=json.loads(raw)
   except (ValueError,TypeError):result=None;item['unstructured_result']=True
   if isinstance(result,dict) and name=='skill_run':
    command=result.get('command') or {};stdout=command.get('stdout') or {};text=stdout.get('text','')
    good=event.get('status')=='completed' and command.get('exitCode')==0 and command.get('terminationConfirmed') is True and stdout.get('truncated') is not True
    item.update(skill=result.get('name'),version=result.get('version'),exit_code=command.get('exitCode'),termination_confirmed=command.get('terminationConfirmed'))
    if args.get('action')=='run' and result.get('name')==expected:
     try:value=json.loads(text)
     except (ValueError,TypeError):value=None
     if isinstance(value,dict):
      if case.startswith('music_search'):
       provider='kuwo' if case=='music_search_kuwo' else 'qq-music'
       count=len(value.get('results',[])) if isinstance(value.get('results'),list) else 0
       item.update(provider=value.get('provider'),search_count=count)
       if good and value.get('ok') is True and value.get('operation')=='search' and value.get('provider')==provider and count>=3:
        out['verified_results'].append({'kind':'music_search','provider':provider,'count':count,'results':value['results'][:3]})
      elif case=='music_download_qq' and value.get('operation')=='download':
       checked=file_check(value);item['file_check']=checked
       if good and value.get('ok') is True and value.get('provider')=='qq-music' and checked['passed']:
        out['verified_results'].append({'kind':'music_download','provider':'qq-music','file':checked})
      elif case=='music_play_admin' and operation(args.get('args') or [])=='play':
       argv=args.get('args') or [];job=value.get('job') or {};item['job']=job
       try:
        selected=argv[argv.index('play')+1]
        selected=str(pathlib.PurePosixPath(selected).relative_to('/workspaces/folotoy/music')) if selected.startswith('/workspaces/folotoy/music/') else selected
       except (ValueError,IndexError):selected=None
       if good and value.get('ok') is True and '--default-device' in argv and '--device-key' not in argv and '--request-id' not in argv and selected==media and isinstance(job.get('id'),str):
        out['verified_results'].append({'kind':'music_queued','job':job,'media':selected,'explicit_default_device':True})
   elif isinstance(result,dict) and name=='host_opencli':
    text=result.get('stdout','');good=event.get('status')=='completed' and result.get('exit_code')==0 and result.get('termination_confirmed') is True and not result.get('stdout_truncated')
    item.update(exit_code=result.get('exit_code'),termination_confirmed=result.get('termination_confirmed'),stdout_truncated=result.get('stdout_truncated'))
    if case=='opencli_doctor' and not args.get('site') and args.get('command')=='doctor':
     daemon=positive_line(text,r'daemon',r'\brunning\b');extension=positive_line(text,r'extension',r'\bconnected\b')
     item.update(daemon_running=daemon,extension_connected=extension,stdout=plain(text)[:6000])
     if good and daemon and extension:out['verified_results'].append({'kind':'opencli_doctor','daemon_running':True,'extension_connected':True})
    elif case=='opencli_read' and args.get('site')=='bilibili' and args.get('command') not in ('help','--help','-h','list','doctor','version') and not any(a in ('--help','-h') for a in args.get('arguments',[])):
     try:value=json.loads(text);items=collection(value)
     except (ValueError,TypeError):items=[]
     item['result_count']=len(items)
     if good and len(items)>=3:out['verified_results'].append({'kind':'opencli_site_read','site':'bilibili','command':args.get('command'),'count':len(items),'sample':items[:3]})
  out['tools'].append(item)
out['passed']=bool(out['verified_results'] and not out['unexpected_tools'] and row['approval_mode']=='allow_always' and run['status']=='completed' and run['stop_reason']=='end_turn')
if case=='music_play_admin' and out['play_submissions']!=1:out['passed']=False
print(json.dumps(out,ensure_ascii=False));db.close()
'''


def run(args, *, payload=None, timeout=30):
    result = subprocess.run(args, input=payload, capture_output=True, text=True, timeout=timeout)
    return result


def parse_conversation(text, case):
    candidates = []
    try:candidates.append(json.loads(text))
    except ValueError:pass
    for line in text.splitlines():
        try:candidates.append(json.loads(line[line.index('{'):]))
        except (ValueError,TypeError):pass
    found = None
    for value in candidates:
        if not isinstance(value,dict):continue
        value = value.get('conversation', value)
        if isinstance(value,dict) and value.get('case')==case:found=value
    return found


def secret_values(root):
    values = []
    for relative in ('data/user-data/private/skills/music/api.json', 'data/user-data/private/skills/music/playback.json', 'data/user-data/private/host-tools/opencli.json'):
        try:value=json.loads((root/relative).read_text())
        except (OSError,ValueError):continue
        if isinstance(value,dict):
            values.extend(v for k,v in value.items() if k in ('token','song_download_api_key','qq_music_api_key','api_key') and isinstance(v,str) and v)
    return values


def sanitized(value, secrets):
    if isinstance(value,dict):return {k:sanitized(v,secrets) for k,v in value.items() if str(k).lower() not in ('token','authorization','api_key','password','cookie')}
    if isinstance(value,list):return [sanitized(v,secrets) for v in value]
    if isinstance(value,str):
        for secret in secrets:value=value.replace(secret,'[REDACTED]')
        value=re.sub(r'(?i)Bearer\s+[^\s"\']+','Bearer [REDACTED]',value)
        return re.sub(r'(?i)([?&](?:apikey|api_key|token|key)=)[^&\s"\']+',r'\1[REDACTED]',value)
    return value


def media_probe(relative):
    path=Path('/music')/relative
    if not path.is_relative_to('/music') or '..' in path.parts:return {'passed':False,'error':'invalid_shared_media_path'}
    result=run(['docker','exec','folotoy-ws-folotoy-1','ffprobe','-v','error','-protocol_whitelist','file,pipe','-show_entries','format=duration:stream=codec_name,codec_type,sample_rate,channels','-of','json',str(path)],timeout=25)
    try:
        value=json.loads(result.stdout);streams=[s for s in value.get('streams',[]) if s.get('codec_type')=='audio' and s.get('codec_name') not in (None,'unknown')];duration=float(value.get('format',{}).get('duration',0))
        return {'passed':result.returncode==0 and bool(streams) and duration>0,'audio_streams':streams,'duration_seconds':duration}
    except (ValueError,TypeError):return {'passed':False,'error':'ffprobe_did_not_identify_audio'}


def poll_music(root,job_id,seconds):
    if not re.fullmatch(r'[A-Za-z0-9_-]{1,128}',job_id):return {'passed':False,'error':'invalid_job_id'}
    try:
        configuration=json.loads((root/'data/user-data/private/skills/music/playback.json').read_text());base=configuration['url'].rstrip('/');token=configuration['token']
    except (OSError,ValueError,KeyError):return {'passed':False,'error':'private_playback_configuration_unavailable'}
    deadline=time.monotonic()+seconds;published_at=None;job={};error=None
    while time.monotonic()<deadline:
        try:
            request=urllib.request.Request(base+'/v1/music/jobs/'+job_id,headers={'Authorization':'Bearer '+token,'Accept':'application/json'})
            with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(request,timeout=5) as response:value=json.load(response)
            job=value.get('job') or {};state=job.get('state')
            if state in ('failed','cancelled','dry_run'):break
            if state=='published':
                if published_at is None:published_at=time.monotonic()
                if job.get('device_audio_requested') or time.monotonic()-published_at>=15:break
        except Exception as caught:error=type(caught).__name__;break
        time.sleep(1)
    return {'passed':job.get('state')=='published','job':job,'device_audio_requested':job.get('device_audio_requested') is True,'speaker_playback_confirmed':False,'error_type':error}


def main(argv=None):
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--case',choices=CASES,default=globals().get('TEST_CASE'))
    parser.add_argument('--root',type=Path,default=ROOT)
    parser.add_argument('--probe-file',type=Path)
    parser.add_argument('--conversation','--conversation-file',dest='conversation',type=Path,help='Audit an existing probe JSON/stdout file; no model call')
    parser.add_argument('--media',default=globals().get('TEST_MEDIA_RELATIVE') or '音乐测试/验证音乐.mp3')
    parser.add_argument('--poll-seconds',type=int,default=45)
    args=parser.parse_args(argv)
    if args.case not in CASES:parser.error('Select --case/TEST_CASE')
    if not 1<=args.poll_seconds<=90:parser.error('poll-seconds must be 1..90')
    report={'case':args.case,'passed':False,'conversation_reused':bool(args.conversation)}
    try:
        if args.conversation:
            conversation=parse_conversation(args.conversation.read_text(),args.case);probe_ok=True
        else:
            code=globals().get('PROBE_CODE')
            if code is None:
                if not args.probe_file:parser.error('Supply --probe-file or PROBE_CODE')
                code=args.probe_file.read_text()
            result=run(['docker','exec','-i','-w','/opt/folotoy','-e','TEST_CASE='+args.case,'-e','TEST_MEDIA_RELATIVE='+args.media,'folotoy-ws-folotoy-1','python3','-'],payload=code,timeout=280 if args.case=='music_download_qq' else 160)
            conversation=parse_conversation(result.stdout,args.case);probe_ok=result.returncode==0
        report['conversation']=conversation
        cursor=(conversation or {}).get('native_cursor') or {};session=cursor.get('sessionId') or (conversation or {}).get('native_session_id')
        if not session:report['error']='native_cursor_missing'
        else:
            result=run(['docker','exec','-i','deerflow-deploy-deerflow-1','/opt/deerflow-skill-python/bin/python','-c',AUDIT],payload=json.dumps({'case':args.case,'session':session,'media':args.media}),timeout=35)
            if result.returncode:report['error']='native_audit_failed'
            else:
                audit=json.loads(result.stdout);report['native_tool_audit']=audit
                report['passed']=bool(probe_ok and conversation.get('status')=='conversation_completed' and conversation.get('cleanup',{}).get('passed') and audit.get('passed'))
                if args.case=='music_download_qq' and audit.get('verified_results'):
                    file=next((v['file'] for v in audit['verified_results'] if v.get('kind')=='music_download'),None)
                    report['media_probe']=media_probe(file['relative_path']) if file else {'passed':False}
                    report['passed']=report['passed'] and report['media_probe']['passed']
                if args.case=='music_play_admin' and audit.get('verified_results'):
                    queued=next((v for v in audit['verified_results'] if v.get('kind')=='music_queued'),None)
                    report['music_job_status']=poll_music(args.root,queued['job']['id'],args.poll_seconds) if queued else {'passed':False}
                    report['passed']=report['passed'] and report['music_job_status']['passed']
    except Exception as error:report['error_type']=type(error).__name__;report['passed']=False
    report=sanitized(report,secret_values(args.root))
    target=args.root/'music-opencli-20261024'/('verification-'+args.case+'.json');target.parent.mkdir(mode=0o700,parents=True,exist_ok=True)
    target.write_text(json.dumps(report,ensure_ascii=False,indent=2)+'\n');target.chmod(0o600)
    print(json.dumps(report,ensure_ascii=False),flush=True)
    return 0 if report['passed'] else 1


if __name__=='__main__':raise SystemExit(main())
