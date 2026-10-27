"""Single-user memory upgrade. Secrets stay on the host; reports contain counts only.

Run stage, verify the isolated instance, then apply. Rollback restores programs
and configuration only; it never overwrites newer business data with a backup.
"""
import argparse
import json
import os
from pathlib import Path
import re
import shutil
import sqlite3
import subprocess
import time

ROOT = Path('/home/gen/deerflow-deploy')
TASK = ROOT / 'single-user-memory-20261027'
DATA = ROOT / 'data'
REL = Path('user-data/config/go-harness-state/harness.db')
IMAGE = 'deerflow-server:single-user-memory-20261027'
EMBED = 'deerflow-memory-embedding:single-user-v1'
MODEL = 'BAAI/bge-small-zh-v1.5@46fbe35'
CONTAINER = 'deerflow-deploy-deerflow-1'

def run(args, data=None, timeout=120):
    p = subprocess.run(args, input=data, text=True, capture_output=True, timeout=timeout)
    if p.returncode: raise RuntimeError('command_failed:' + args[0])
    return p.stdout

def atomic(path, value):
    tmp = path.with_name(path.name + '.memory-tmp')
    with open(tmp, 'x', encoding='utf-8') as f:
        os.chmod(tmp, 0o600); f.write(value); f.flush(); os.fsync(f.fileno())
    os.replace(tmp, path)

def memory_config(raw, embedding=True):
    lines = raw.splitlines(keepends=True)
    found = [i for i,v in enumerate(lines) if re.fullmatch(r'memory:\s*(?:#.*)?\n?',v)]
    if len(found)!=1: raise RuntimeError('memory_section_ambiguous')
    start = found[0]+1; end=start
    while end<len(lines) and (not lines[end].strip() or lines[end].startswith((' ', '#'))): end+=1
    indents=[len(v)-len(v.lstrip(' ')) for v in lines[start:end] if v.strip() and not v.lstrip().startswith('#')]
    indent=' '*min(indents) if indents else '  '
    values={'single_user':'true','enabled':'true','include_global':'true','injection_enabled':'true','retrieval_enabled':'true','mode':'middleware'}
    if embedding: values.update(embedding_url='http://memory-embedding:8080',embedding_model=MODEL)
    for key in values:
        lines[start:end]=[v for v in lines[start:end] if not re.match(re.escape(indent+key)+r'\s*:',v)]
        end=start
        while end<len(lines) and (not lines[end].strip() or lines[end].startswith((' ', '#'))): end+=1
    lines[start:start]=[indent+k+': '+json.dumps(v)+'\n' if k in ('embedding_url','embedding_model','mode') else indent+k+': '+v+'\n' for k,v in values.items()]
    # Native scope is a local_acp setting; memory.scope is not consumed.
    raw=''.join(lines)
    match=re.search(r'(?m)^local_acp:\s*\n',raw)
    if not match:return raw+'\nlocal_acp:\n  memory_scope: workspace\n'
    a=match.end();next_section=re.search(r'(?m)^\S[^\n]*:',raw[a:]);b=a+next_section.start() if next_section else len(raw)
    indents=[len(v)-len(v.lstrip(' ')) for v in raw[a:b].splitlines() if v.strip() and not v.lstrip().startswith('#')]
    indent=' '*min(indents) if indents else '  '
    body=re.sub(r'(?m)^'+re.escape(indent)+r'memory_scope:.*\n?', '',raw[a:b])
    return raw[:a]+indent+'memory_scope: workspace\n'+body+raw[b:]

def compose(*args):
    return ['docker','compose','--env-file',str(ROOT/'.env'),'--project-directory',str(ROOT),'-p','deerflow-deploy','-f',str(ROOT/'compose.yaml'),*args]

def control(operation):
    out=run(['docker','exec','-i',CONTAINER,'/var/lib/deerflow/deerflow-control-go','--runtime-dir','/var/lib/deerflow/user-data/runtime/acp-go'],json.dumps({'operation':operation}))
    data=json.loads(out)
    if not data.get('ok'): raise RuntimeError('native_control_failed')
    return data['data']

def idle():
    state=control('daemon.status')
    if state.get('active_runs')!=0 or state.get('queued_runs')!=0: raise RuntimeError('wait_for_active_work')

def db_backup(source, target):
    target.parent.mkdir(parents=True,exist_ok=True)
    with sqlite3.connect('file:'+str(source)+'?mode=ro',uri=True) as src, sqlite3.connect(target) as dst: src.backup(dst)
    target.chmod(0o600)

def migrate(data):
    result=run(['docker','run','--rm','--network','none','--user','1000:1000','-v',str(data)+':/var/lib/deerflow','-v','/home/gen/deerflow-workspaces:/workspaces:ro','-v',str(TASK/'migration-plan.json')+':/plan.json:ro','--entrypoint','/opt/deerflow/bin/deerflow-personal-migrate',IMAGE,'--data-dir','/var/lib/deerflow/user-data/config/go-harness-state','--plan','/plan.json'])
    return json.loads(result)

def wait_healthy(name):
    for _ in range(60):
        s=json.loads(run(['docker','inspect',name]))[0]['State']
        if s.get('Health',{}).get('Status')=='healthy': return
        if s.get('Status') in ('dead','exited'): break
        time.sleep(2)
    raise RuntimeError('container_not_healthy:'+name)

def embedding():
    existing=run(['docker','ps','-a','--filter','name=^/deerflow-memory-embedding$','--format','{{.Names}}']).strip()
    if not existing:
        document={'services':{'embedding':{'image':EMBED,'container_name':'deerflow-memory-embedding','networks':{'runtime':{'aliases':['memory-embedding']}},'restart':'unless-stopped','mem_limit':'1g','cpus':2,'read_only':True,'tmpfs':['/tmp:rw,nosuid,size=64m'],'cap_drop':['ALL'],'security_opt':['no-new-privileges:true']}},'networks':{'runtime':{'external':True,'name':'deerflow-deploy_default'}}}
        atomic(TASK/'embedding-compose.json',json.dumps(document))
        run(['docker','compose','-p','deerflow-memory','-f',str(TASK/'embedding-compose.json'),'up','-d'])
    wait_healthy('deerflow-memory-embedding')

def stage():
    embedding()
    stage=TASK/'stage-data'
    if stage.exists(): raise RuntimeError('staging_already_exists')
    (stage/'user-data/config').mkdir(parents=True,mode=0o700)
    cfg=DATA/'user-data/config/config.yaml'
    atomic(stage/'user-data/config/config.yaml',memory_config(cfg.read_text()))
    db_backup(DATA/REL,stage/REL)
    shutil.copytree(ROOT/'skills',stage/'user-data/skills',symlinks=True)
    report=migrate(stage)
    original=json.loads(run(compose('config','--format','json')))['services']['deerflow']
    original['image']=IMAGE;original['container_name']='deerflow-memory-stage';original['restart']='no'
    original['ports']=[{'target':18787,'published':'18788','host_ip':'127.0.0.1','protocol':'tcp'}]
    original['networks']={'runtime':{}}
    for volume in original['volumes']:
        if volume['target']=='/var/lib/deerflow':volume['source']=str(stage)
        if volume['target']=='/var/log/deerflow':
            (TASK/'stage-logs').mkdir(exist_ok=True);volume['source']=str(TASK/'stage-logs')
        if volume['target']=='/var/lib/deerflow/user-data/skills':volume['source']=str(stage/'user-data/skills')
        if volume['target']=='/workspaces':volume['read_only']=True
    document={'services':{'stage':original},'networks':{'runtime':{'external':True,'name':'deerflow-deploy_default'}}}
    atomic(TASK/'stage-compose.json',json.dumps(document))
    run(['docker','compose','-p','deerflow-memory-stage','-f',str(TASK/'stage-compose.json'),'up','-d'])
    wait_healthy('deerflow-memory-stage')
    atomic(TASK/'stage-result.json',json.dumps({'migration':report,'healthy':True}))
    print(json.dumps({'stage':'healthy','migration':report}))

def apply():
    if not (TASK/'acceptance-stage.json').exists(): raise RuntimeError('staging_acceptance_required')
    accepted=json.loads((TASK/'acceptance-stage.json').read_text())
    if accepted.get('passed') is not True: raise RuntimeError('staging_acceptance_failed')
    for name in ('restart-stage.json','embedding-validation.json'):
        if json.loads((TASK/name).read_text()).get('passed') is not True: raise RuntimeError('release_gate_failed:'+name)
    embedding();idle()
    state=control('daemon.drain')
    if state.get('active_runs')!=0 or state.get('queued_runs')!=0:
        control('daemon.resume');raise RuntimeError('work_started_before_drain')
    backup=TASK/'backup'
    if backup.exists():
        control('daemon.resume');raise RuntimeError('backup_already_exists')
    backup.mkdir(mode=0o700)
    for name,path in [('config.yaml',DATA/'user-data/config/config.yaml'),('compose.yaml',ROOT/'compose.yaml'),('env',ROOT/'.env')]:
        shutil.copy2(path,backup/name);(backup/name).chmod(0o600)
    run(compose('stop','deerflow'))
    try:
        db_backup(DATA/REL,backup/'harness.db')
        report=migrate(DATA)
        cfg=DATA/'user-data/config/config.yaml';atomic(cfg,memory_config(cfg.read_text()))
        env=(ROOT/'.env').read_text()
        env,n=re.subn(r'(?m)^DEERFLOW_IMAGE=.*$', 'DEERFLOW_IMAGE='+IMAGE,env)
        if n!=1: raise RuntimeError('image_setting_ambiguous')
        atomic(ROOT/'.env',env)
        run(compose('up','-d','--no-deps','deerflow'))
        wait_healthy(CONTAINER)
        atomic(TASK/'deployment.json',json.dumps({'status':'deployed','image':IMAGE,'migration':report,'backup':str(backup),'embedding_host_port':False}))
        print(json.dumps({'status':'deployed','migration':report}))
    except Exception:
        for name,path in [('config.yaml',DATA/'user-data/config/config.yaml'),('compose.yaml',ROOT/'compose.yaml'),('env',ROOT/'.env')]: atomic(path,(backup/name).read_text())
        run(compose('up','-d','--no-deps','deerflow'))
        raise

def rollback():
    idle();run(compose('stop','deerflow'))
    for name,path in [('config.yaml',DATA/'user-data/config/config.yaml'),('compose.yaml',ROOT/'compose.yaml'),('env',ROOT/'.env')]:atomic(path,(TASK/'backup'/name).read_text())
    run(compose('up','-d','--no-deps','deerflow'));wait_healthy(CONTAINER)
    print(json.dumps({'rolled_back':True,'business_data_restored':False}))

if __name__=='__main__':
    parser=argparse.ArgumentParser();parser.add_argument('action',choices=['stage','apply','rollback']);args=parser.parse_args()
    try: globals()[args.action]()
    except Exception as error:
        print(json.dumps({'passed':False,'error':str(error) if isinstance(error,RuntimeError) else type(error).__name__}));raise SystemExit(1)
