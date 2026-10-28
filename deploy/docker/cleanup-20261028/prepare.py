import datetime,hashlib,json,os,pathlib,re,stat,subprocess
root=pathlib.Path('/home/gen/deerflow-deploy');stage=root/'cleanup-20261028';audit=json.loads(SOURCE_FILES['inventory-summary.json']);candidates=[pathlib.Path(v['path']) for v in audit['cleanup_candidates']]
expected=['voice-repair-20261025/payload-66c92e3784a7','voice-repair-20261025/payload-dc21966e968c','voice-repair-20261025/revision-464f9311','voice-repair-20261025/revision-5d487745','asr2-diagnosis-20261025','asr2-validation-20261025','skill-migration-20261024/runtime-build-fast.log','skill-migration-20261024/runtime-build.log','build-cache-cleanup-20261024.log','cleanup-20261024.log','build-20261024.log.gz']
if root.resolve()!=root or {str(p.relative_to(root)) for p in candidates}!=set(expected):raise RuntimeError('unexpected cleanup scope')
docker=['docker','--host','unix:///var/run/docker.sock']
def run(args,timeout=45,check=True):
 p=subprocess.run(args,text=True,capture_output=True,timeout=timeout)
 if check and p.returncode:raise RuntimeError('preflight command failed: '+args[0])
 return p
def containers():
 ids=run(docker+['ps','-aq']).stdout.split()
 if not ids:return []
 result=json.loads(run(docker+['inspect',*ids]).stdout)
 return [{'id':v['Id'],'name':v['Name'].lstrip('/'),'image_id':v['Image'],'image':v['Config']['Image'],'state':v['State']['Status'],'started_at':v['State']['StartedAt'],'restart_count':v['RestartCount'],'health':v['State'].get('Health',{}).get('Status'),'mounts':[{'source':m.get('Source'),'destination':m.get('Destination')} for m in v.get('Mounts',[])]} for v in result]
def images():
 ids=sorted(set(run(docker+['image','ls','--quiet','--no-trunc','--all']).stdout.split()))
 if not ids:return []
 result=json.loads(run(docker+['image','inspect',*ids]).stdout)
 return [{'id':v['Id'],'tags':sorted(v.get('RepoTags') or []),'digests':sorted(v.get('RepoDigests') or [])} for v in result]
def sha(path):
 h=hashlib.sha256()
 with path.open('rb') as stream:
  while True:
   chunk=stream.read(1024*1024)
   if not chunk:break
   h.update(chunk)
 return h.hexdigest()
def file_record(path):
 info=path.lstat()
 if not stat.S_ISREG(info.st_mode) or info.st_nlink!=1:raise RuntimeError('cleanup target is not a unique regular file')
 return {'path':str(path),'bytes':info.st_size,'allocated_bytes':info.st_blocks*512,'sha256':sha(path),'mtime_ns':info.st_mtime_ns,'inode':info.st_ino}
before=containers();before_images=images();entries=[]
for path in candidates:
 if not path.exists():entries.append({'path':str(path),'already_absent':True});continue
 if path.is_symlink() or path.resolve()!=path:raise RuntimeError('cleanup symlink rejected')
 for container in before:
  for mount in container['mounts']:
   if not mount.get('source'):continue
   source=pathlib.Path(mount['source'])
   if source==path or source in path.parents or path in source.parents:raise RuntimeError('cleanup path overlaps container mount')
 if path.is_dir():
  all_paths=list(path.rglob('*'))
  if any(p.is_symlink() or any('backup' in part.lower() for part in p.relative_to(path).parts) or p.name in ('rollback.py','deployment.json','index.json','.env') for p in all_paths):raise RuntimeError('unexpected protected item in historical directory')
  records=[file_record(p) for p in all_paths if p.is_file()]
 else:records=[file_record(path)]
 if any(v['mtime_ns']/1e9>datetime.datetime.fromisoformat(audit['captured_at']).timestamp() for v in records):raise RuntimeError('historical files changed after audit')
 entries.append({'path':str(path),'directory':path.is_dir(),'files':records,'allocated_bytes':sum(r['allocated_bytes'] for r in records),'directory_count':(1+sum(p.is_dir() for p in all_paths)) if path.is_dir() else 0})
# Re-scan readable deployment code and metadata, excluding the deletion targets
# and this cleanup's audit output. A deletion manifest is not a runtime reference.
references=[]
for current,dirs,files in os.walk(root,followlinks=False):
 current=pathlib.Path(current);dirs[:]=[d for d in dirs if d not in ('.git','node_modules','__pycache__','.venv') and current/d!=stage and not any(current/d==p or p in (current/d).parents for p in candidates) and not (current==root and d in ('data','skills','private','logs'))]
 for name in files:
  p=current/name
  if p.suffix.lower() not in ('.py','.sh','.json','.yaml','.yml','.md','.service','.txt') or p.stat().st_size>2*1024*1024:continue
  try:text=p.read_text(errors='replace')
  except OSError:continue
  for target in candidates:
   if str(target) in text:references.append({'source':str(p),'target':str(target)})
if references:raise RuntimeError('a current deployment document references a cleanup target')
service=run(['systemctl','--user','show','deerflow-opencli-bridge.service','--property=ActiveState,MainPID,ActiveEnterTimestampMonotonic,ExecMainStartTimestampMonotonic']).stdout
open_refs=[];build_processes=[]
for process in pathlib.Path('/proc').iterdir():
 if not process.name.isdigit():continue
 try:
  args=(process/'cmdline').read_bytes().split(b'\0');decoded=[v.decode(errors='replace') for v in args if v]
  if decoded and pathlib.Path(decoded[0]).name in ('docker','docker-buildx','buildctl') and 'build' in decoded[1:]:build_processes.append(int(process.name))
  for target in candidates:
   if any(str(target) in arg for arg in decoded):open_refs.append({'pid':int(process.name),'target':str(target),'kind':'command_line'})
  for fd in (process/'fd').iterdir():
   try:link=os.readlink(fd)
   except OSError:continue
   for target in candidates:
    if link==str(target) or link.startswith(str(target)+'/'):open_refs.append({'pid':int(process.name),'target':str(target),'kind':'open_file'})
 except (OSError,PermissionError):continue
if open_refs or build_processes:raise RuntimeError('cleanup target/build is currently in use')
protected={}
for current,dirs,files in os.walk(root,followlinks=False):
 current=pathlib.Path(current);dirs[:]=[d for d in dirs if current/d!=stage and not any(current/d==p or p in (current/d).parents for p in candidates)]
 is_backup=any('backup' in part.lower() for part in current.relative_to(root).parts)
 for name in files:
  p=current/name
  preserve=is_backup or p in (root/'compose.yaml',root/'.env',root/'data/user-data/config/config.yaml') or current in (root/'runtime',root/'private')
  if preserve and p.is_file() and not p.is_symlink():protected[str(p)]={'sha256':sha(p),'bytes':p.stat().st_size}
help_text=run(docker+['builder','prune','--help']).stdout
if '--all' not in help_text or '--force' not in help_text:raise RuntimeError('builder prune flags unavailable')
builder=run(docker+['buildx','inspect','default'],check=False)
context=run(['docker','context','inspect','default'],check=False)
usage=run(docker+['system','df']).stdout.splitlines()
plan={'created_at':datetime.datetime.now(datetime.timezone.utc).isoformat(),'root':str(root),'candidates':entries,'containers_before':before,'images_before':before_images,'protected_files':protected,'opencli_service_before':service.splitlines(),'docker_endpoint':'unix:///var/run/docker.sock','builder_prune_help':help_text,'builder_inspect':builder.stdout[:4000] if builder.returncode==0 else None,'default_context_endpoint':None,'docker_disk_usage_before':usage,'filesystem_before':{'free_bytes':__import__('shutil').disk_usage(root).free},'revalidated_no_mount_or_document_references':True,'accessible_active_build_processes':build_processes}
if context.returncode==0:
 doc=json.loads(context.stdout);plan['default_context_endpoint']=doc[0].get('Endpoints',{}).get('docker',{}).get('Host')
stage.mkdir(mode=0o700,exist_ok=True)
path=stage/'plan.json';path.write_text(json.dumps(plan,indent=2)+'\n');path.chmod(0o600)
print(json.dumps({'plan':str(path),'candidate_count':len(entries),'historical_allocated_file_bytes':sum(v.get('allocated_bytes',0) for v in entries),'protected_file_count':len(protected),'container_count':len(before),'image_count':len(before_images),'docker_disk_usage':usage,'builder_prune_help':help_text,'builder_inspect':plan['builder_inspect'],'default_context_endpoint':plan['default_context_endpoint']},ensure_ascii=False),flush=True)
