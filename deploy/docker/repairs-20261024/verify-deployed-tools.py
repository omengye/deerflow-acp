"""Run one isolated FoloToy probe and audit actual native tool receipts.

The SSH helper can supply PROBE_CODE and TEST_CASE in memory. Otherwise use
--probe-file and --case on the Docker host. No credentials are accepted here.
"""
import argparse
import json
from pathlib import Path
import subprocess


AUDIT = r'''
from datetime import datetime,timedelta,timezone
import json,pathlib,sqlite3,sys
request=json.load(sys.stdin);sid=request['session'];case=request['case'];db=None;database=None
for path in pathlib.Path('/var/lib/deerflow/user-data').rglob('harness.db'):
 candidate=sqlite3.connect(path.as_uri()+'?mode=ro',uri=True);candidate.row_factory=sqlite3.Row
 if candidate.execute('SELECT 1 FROM harness_sessions WHERE id=?',(sid,)).fetchone():db=candidate;database=str(path);break
 candidate.close()
if db is None:print(json.dumps({'passed':False,'error':'native_session_not_found'}));sys.exit(1)
row=db.execute('SELECT mode,model,approval_mode FROM harness_sessions s JOIN harness_session_configs c ON c.session_id=s.id WHERE s.id=?',(sid,)).fetchone()
out={'native_session':sid,'database':database,'permission':dict(row) if row else None,'tools':[],'verified_results':[],'unexpected_tools':[]}
run=db.execute('SELECT id,status,stop_reason FROM harness_runs WHERE session_id=? ORDER BY created_at DESC LIMIT 1',(sid,)).fetchone()
if run is None:print(json.dumps({'passed':False,'error':'native_run_not_found'}));sys.exit(1)
out['run']=dict(run);es=[json.loads(r[0]) for r in db.execute('SELECT event FROM harness_events WHERE run_id=? ORDER BY sequence',(run['id'],))]
starts={};now=datetime.now(timezone(timedelta(hours=8)));tomorrow=(now+timedelta(days=1)).date().isoformat()
out['expected_tomorrow']=tomorrow
text=''.join(x.get('text','') for e in es if e.get('kind')=='user_message' for x in e.get('content',[]) if isinstance(x,dict))
out['trusted_clock_context_present']=('本轮时间基准（来自 Folotoy 服务器系统时钟' in text and '时区 Asia/Shanghai' in text and '明天：'+tomorrow in text)
for e in es:
 name=e.get('toolName');kind=e.get('kind');call=e.get('toolCallId')
 if kind=='tool_start':
  args=e.get('arguments') or {};starts[call]=args
  if name not in ('skill','read_skill_file','skill_run'):out['unexpected_tools'].append(name)
  safe={k:args[k] for k in ('skill','name','action','path','args') if k in args} if name in ('skill','read_skill_file','skill_run') else {}
  out['tools'].append({'event':'start','name':name,'arguments':safe})
 elif kind=='tool_end':
  item={'event':'end','name':name,'status':e.get('status'),'failure_code':(e.get('receipt') or {}).get('failureCode')}
  if name=='skill_run':
   args=starts.get(call,{})
   raw=''.join(x.get('text','') for x in e.get('content',[]) if isinstance(x,dict))
   try:
    result=json.loads(raw);command=result.get('command') or {};stdout=command.get('stdout') or {};output=stdout.get('text','')
    item.update(skill=result.get('name'),version=result.get('version'),ready=(result.get('readiness') or {}).get('ready'),exit_code=command.get('exitCode'),termination_confirmed=command.get('terminationConfirmed'))
    succeeded=(e.get('status')=='completed' and command.get('exitCode')==0 and command.get('terminationConfirmed') is True and stdout.get('truncated') is not True)
    if case=='weather' and result.get('name')=='weather-cma' and args.get('action')=='run':
     forecast=json.loads(output);item['weather_result']=forecast
     dates=[x.get('date') for x in forecast.get('forecast',[])]
     if succeeded and forecast.get('ok') is True and forecast.get('requested_date')==tomorrow and tomorrow in dates:out['verified_results'].append({'kind':'dated_weather','date':tomorrow,'source':forecast.get('source'),'forecast':forecast.get('forecast')})
    elif case in ('cloud_linux','cloud_sandbox') and args.get('action')=='run':
     expected='huaweicloud-linux' if case=='cloud_linux' else 'huaweicloud-sandbox';argv=args.get('args') or []
     item['stdout']=output[:1800]
     if succeeded and result.get('name')==expected and argv and argv[-1]=='status' and '--key' in argv and '--known-hosts' in argv:
      out['verified_results'].append({'kind':'strict_ssh_status','skill':expected,'exit_code':0,'stdout':output[:1800]})
   except (ValueError,TypeError,AttributeError):item['result_summary']=raw[-1200:]
  out['tools'].append(item)
out['passed']=bool(out['verified_results'] and not out['unexpected_tools'] and out['trusted_clock_context_present'] and row and row['approval_mode']=='allow_always' and run['status']=='completed')
print(json.dumps(out,ensure_ascii=False));db.close()
'''


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--case', choices=('weather', 'cloud_linux', 'cloud_sandbox'),
                        default=globals().get('TEST_CASE'))
    parser.add_argument('--probe-file', type=Path)
    args = parser.parse_args()
    if not args.case:
        parser.error('Select a case')
    code = globals().get('PROBE_CODE')
    if code is None:
        if not args.probe_file:
            parser.error('Supply --probe-file')
        code = args.probe_file.read_text()
    result = subprocess.run(['docker', 'exec', '-i', '-w', '/opt/folotoy',
                             '-e', 'TEST_CASE=' + args.case,
                             'folotoy-ws-folotoy-1', 'python3', '-'],
                            input=code, capture_output=True, text=True, timeout=155)
    conversation = None
    for line in result.stdout.splitlines():
        try:
            value = json.loads(line)
            if isinstance(value, dict) and value.get('case') == args.case:
                conversation = value
        except ValueError:
            continue
    report = {'case': args.case, 'passed': False, 'conversation': conversation}
    if conversation:
        cursor = conversation.get('native_cursor') or {}
        if cursor.get('sessionId'):
            audit = subprocess.run(['docker', 'exec', '-i', 'deerflow-deploy-deerflow-1',
                                    '/opt/deerflow-skill-python/bin/python', '-c', AUDIT],
                                   input=json.dumps({'session': cursor['sessionId'], 'case': args.case}),
                                   capture_output=True, text=True, timeout=25)
            if audit.returncode == 0:
                report['native_tool_audit'] = json.loads(audit.stdout)
                report['passed'] = (result.returncode == 0 and report['native_tool_audit']['passed']
                                    and conversation.get('cleanup', {}).get('passed') is True)
    path = Path('/home/gen/deerflow-deploy/repair-20261024') / ('verification-' + args.case + '.json')
    path.write_text(json.dumps(report, ensure_ascii=False, indent=2) + '\n')
    path.chmod(0o600)
    print(json.dumps(report, ensure_ascii=False), flush=True)
    return 0 if report['passed'] else 1


if __name__ == '__main__':
    raise SystemExit(main())
