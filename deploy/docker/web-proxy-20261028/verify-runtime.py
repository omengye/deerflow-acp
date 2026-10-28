import datetime,json,pathlib,re,sqlite3,subprocess,threading,time
root=pathlib.Path('/home/gen/deerflow-deploy');stage=root/'web-proxy-20261028';dc='deerflow-deploy-deerflow-1';fc='folotoy-ws-folotoy-1';target='gpt-6.1-terra'
pre=json.loads((stage/'preflight.json').read_text());ips=set(pre['container_ips'])
def socket_snapshot():
 p=subprocess.run(['ss','-H','-tn','(','sport','=',':11055','or','dport','=',':11055',')'],capture_output=True,text=True,timeout=3)
 if p.returncode:raise RuntimeError('proxy connection observation unavailable')
 rows={}
 for line in p.stdout.splitlines():
  cols=line.split()
  if len(cols)<5:continue
  local,peer=cols[3],cols[4]
  peer_ip=peer.rsplit(':',1)[0].strip('[]')
  if local.endswith(':11055') and peer_ip in ips and cols[0]=='ESTAB':rows[(local,peer)]={'local':local,'peer':peer,'state':cols[0]}
 return rows
def native_audit(report):
 sid=report.get('native_session_id')
 if not sid:return {'passed':False,'reason':'native_session_missing'}
 db=sqlite3.connect((root/'data/user-data/config/go-harness-state/harness.db').as_uri()+'?mode=ro',uri=True);db.row_factory=sqlite3.Row
 model=dict(db.execute('SELECT id,model FROM harness_sessions WHERE id=?',(sid,)).fetchone());run=dict(db.execute('SELECT id,status,stop_reason FROM harness_runs WHERE session_id=? ORDER BY created_at DESC LIMIT 1',(sid,)).fetchone());calls=[]
 for value in db.execute('SELECT event FROM harness_events WHERE run_id=? ORDER BY sequence',(run['id'],)):
  event=json.loads(value[0])
  if event.get('kind')!='tool_end':continue
  item={k:event[k] for k in ('toolCallId','toolName','status') if k in event};receipt=event.get('receipt') or {};item['failure_code']=receipt.get('failureCode');content=event.get('content') or receipt.get('result') or []
  texts=[v.get('text','') for v in content if isinstance(v,dict) and v.get('type')=='text'];text='\n'.join(texts)
  if item.get('toolName') in ('web_search','image_search'):
   for body in texts:
    try:data=json.loads(body)
    except ValueError:continue
    if isinstance(data,dict) and isinstance(data.get('results'),list):item['result_count']=data.get('total_results',len(data['results']));item['non_empty_result']=item['result_count']>0
  elif item.get('toolName')=='web_fetch':item['non_empty_result']=bool(text.strip());item['expected_page_marker_present']='example domain' in text.lower()
  if item.get('status')!='completed':item['safe_error']=text[:350]
  calls.append(item)
 db.close();selected=[v for v in calls if v.get('toolName')==report['case'] and v.get('status')=='completed'];meaningful=any(v.get('non_empty_result') is True for v in selected)
 if report['case']=='web_fetch':meaningful=meaningful and any(v.get('expected_page_marker_present') for v in selected)
 return {'session':model,'run':run,'tool_ends':calls,'meaningful_native_result':meaningful,'passed':model['model']==target and run['status']=='completed' and run['stop_reason']=='end_turn' and meaningful}
payload={'verify-ws-base.py':SOURCE_FILES['verify-ws-base.py']};code='SOURCE_FILES='+repr(payload)+"\n__name__='__main__'\n__file__='/tmp/verify-tools.py'\n"+SOURCE_FILES['verify-tools.py']
for case in ('web_search','web_fetch','image_search'):
 before=socket_snapshot();observed={};errors=[];stop=threading.Event();start=time.monotonic();started=datetime.datetime.now(datetime.timezone.utc).isoformat()
 def observe():
  while not stop.is_set():
   try:
    for key,value in socket_snapshot().items():
     if key not in before and key not in observed:observed[key]={**value,'first_seen_elapsed_ms':round((time.monotonic()-start)*1000)}
   except Exception as error:errors.append(type(error).__name__)
   stop.wait(.05)
 thread=threading.Thread(target=observe,daemon=True);thread.start()
 try:
  p=subprocess.run(['docker','exec','-i','-w','/opt/folotoy',fc,'python3','-','--run','--case',case,'--expected-model',target,'--timeout','180'],input=code,text=True,capture_output=True,timeout=215)
 finally:stop.set();thread.join(timeout=5)
 values=[json.loads(line) for line in p.stdout.splitlines() if line.startswith('{')]
 if not values:raise RuntimeError('native tool verifier produced no safe report')
 report=values[-1];report['started_at']=started;report['process_exit_code']=p.returncode;report['native_audit']=native_audit(report)
 report['proxy_evidence']={'kind':'new_host_proxy_tcp_connections_correlated_with_isolated_native_tool_run','proxy':'http://172.17.0.1:11055','container_source_ips':sorted(ips),'new_connections':list(observed.values()),'observation_error_types':sorted(set(errors)),'passed':bool(observed) and not errors,'xray_access_logging_enabled_for_test':False}
 report['native_model_receipts_verified']=report['native_audit']['passed'];report['native_tool_receipts_verified']=report['native_audit']['passed'];report['tool_receipts_verified']=report['native_audit']['passed'];report['requires_native_model_audit']=False;report['requires_native_tool_audit']=False
 report['passed']=p.returncode==0 and report.get('ws_protocol_passed') is True and report['native_audit']['passed'] and report['proxy_evidence']['passed'];report['status']='passed' if report['passed'] else 'failed'
 file=stage/(case+'.json');file.write_text(json.dumps(report,ensure_ascii=False,indent=2)+'\n');file.chmod(0o600)
 print(json.dumps(report,ensure_ascii=False),flush=True)
 if not report['passed']:raise RuntimeError('native web tool verification failed: '+case)
