import datetime,hashlib,json,os,pathlib,sqlite3,subprocess,time,uuid
root=pathlib.Path('/home/gen/deerflow-deploy/voice-repair-20261025');folo=pathlib.Path('/home/gen/Documents/folotoy/deerflow-ws/src');fc='folotoy-ws-folotoy-1';image='folotoy-deerflow-ws:device-sessions-v2-20261025'
oldreport=json.loads((root/'deployment.json').read_text());baseline=json.loads((root/'preflight.json').read_text());names=('core/device_session_store.py','core/folotoy_app.py','core/folotoy_msg_processor.py')
def sha(b):return hashlib.sha256(b).hexdigest()
def command(cmd,data=None,timeout=60):
 p=subprocess.run(cmd,input=data,text=True,capture_output=True,timeout=timeout)
 if p.returncode:raise RuntimeError('revision command failed: '+cmd[0])
 return p.stdout
def atomic(p,b,mode):
 temp=p.with_name('.'+p.name+'.revision-'+uuid.uuid4().hex)
 try:temp.write_bytes(b);temp.chmod(mode);os.replace(temp,p)
 finally:temp.unlink(missing_ok=True)
for e in oldreport['files']:
 if sha(pathlib.Path(e['path']).read_bytes())!=e['after_sha256']:raise RuntimeError('source changed since prior revision')
payload=root/('revision-'+uuid.uuid4().hex[:8]);payload.mkdir(mode=0o700)
files={}
for name in names:
 p=payload/'folotoy-source'/name;p.parent.mkdir(parents=True,exist_ok=True,mode=0o700);p.write_text(SOURCE_FILES[pathlib.Path(name).name]);files[str(p.relative_to(payload))]=p.read_text()
p=payload/'tests/test_device_sessions.py';p.parent.mkdir(mode=0o700);p.write_text(SOURCE_FILES['test_device_sessions.py']);files[str(p.relative_to(payload))]=p.read_text()
tests='''import json,os,pathlib,subprocess,sys,tempfile
p=json.load(sys.stdin)
with tempfile.TemporaryDirectory(prefix='device-session-revision-tests-') as temp:
 root=pathlib.Path(temp)
 for name,data in p.items():
  dest=root/name;dest.parent.mkdir(parents=True,exist_ok=True);dest.write_text(data)
 result=subprocess.run([sys.executable,str(root/'tests/test_device_sessions.py')],env=dict(os.environ,FOLOTOY_BASE_SOURCE='/opt/folotoy'),capture_output=True,text=True)
 print(json.dumps({'passed':result.returncode==0,'diagnostics':(result.stdout+'\\n'+result.stderr)[-4000:]}))
'''
testresult=json.loads(command(['docker','exec','-i','-w','/opt/folotoy',fc,'python3','-c',tests],json.dumps(files)))
print(json.dumps({'stage':'revision_tests','result':testresult}),flush=True)
if not testresult['passed']:raise RuntimeError('revision tests failed')
assert json.loads(command(['docker','image','inspect',oldreport['old_image']]))[0]['Id']==oldreport['old_image_id']
dockerfile='FROM '+oldreport['old_image']+'\nUSER root\n'+''.join('COPY --chown=1000:1000 folotoy-source/'+n+' /opt/folotoy/'+n+'\n' for n in names)+'RUN rm -f /opt/folotoy/core/folotoy_app.cpython-*.so /opt/folotoy/core/folotoy_msg_processor.cpython-*.so\nUSER '+baseline['runtime_user']+'\n'
(payload/'Dockerfile').write_text(dockerfile);command(['docker','build','--network','none','--tag',image,str(payload)],timeout=180)
newimageid=json.loads(command(['docker','image','inspect',image]))[0]['Id']
composepath=folo/'compose.ws.yaml';composebefore=composepath.read_bytes()
parser="import json,sys,yaml;p=json.load(sys.stdin);d=yaml.safe_load(p['raw']);d['services']['folotoy']['image']=p['image'];print(json.dumps({'raw':yaml.safe_dump(d,sort_keys=False)}))"
composenew=json.loads(command(['docker','exec','-i',fc,'python3','-c',parser],json.dumps({'raw':composebefore.decode(),'image':image})))['raw'].encode()
with sqlite3.connect((folo/'data/deerflow-sessions.sqlite3').as_uri()+'?mode=ro',uri=True) as db:
 if db.execute("SELECT count(*) FROM deerflow_sessions WHERE status NOT IN ('completed','idle','invalidated','failed','cancelled')").fetchone()[0]:raise RuntimeError('voice turn active')
proposals={folo/n:SOURCE_FILES[pathlib.Path(n).name].encode() for n in names};proposals[composepath]=composenew;before={p:p.read_bytes() for p in proposals}
compose=['docker','compose','--env-file',str(folo/'.env.ws'),'-p','folotoy-ws','-f',str(composepath),'--project-directory',str(folo)]
try:
 for path,data in proposals.items():atomic(path,data,path.stat().st_mode & 0o777)
 command(compose+['config','--quiet']);command(compose+['up','-d','--no-build','--force-recreate','folotoy'],timeout=100)
 for _ in range(40):
  obj=json.loads(command(['docker','inspect',fc]))[0]
  if obj['State'].get('Health',{}).get('Status')=='healthy':break
  time.sleep(2)
 else:raise RuntimeError('revision health timeout')
 report=json.loads(json.dumps(oldreport));report['intermediate_image']=oldreport['image'];report['intermediate_image_id']=oldreport['image_id'];report.update(image=image,image_id=newimageid,payload=str(payload),offline_tests=testresult,login_atomicity_verified=True)
 for e in report['files']:e['after_sha256']=sha(pathlib.Path(e['path']).read_bytes())
 atomic(root/'deployment.json',(json.dumps(report,indent=2)+'\n').encode(),0o600)
 print(json.dumps({'stage':'revision_deployed_healthy','image':image,'image_id':newimageid,'original_rollback_preserved':True}),flush=True)
except Exception:
 for path,data in before.items():atomic(path,data,path.stat().st_mode & 0o777)
 command(compose+['up','-d','--no-build','--force-recreate','folotoy'],timeout=100)
 raise
