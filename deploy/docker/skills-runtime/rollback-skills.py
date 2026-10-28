"""Restore the saved container setup and selectively undo new skill enablement."""
import json,pathlib,re,shutil,subprocess,time
root=pathlib.Path('/home/gen/deerflow-deploy');stage=root/'skill-migration-20261024';name='deerflow-deploy-deerflow-1'
def manage(request):
    result=subprocess.run(['docker','exec','-i',name,'/var/lib/deerflow/deerflow-control-go','--runtime-dir','/var/lib/deerflow/user-data/runtime/acp-go'],input=json.dumps(request),capture_output=True,text=True,check=True)
    return json.loads(result.stdout)
try:
    manage({'operation':'daemon.drain'})
    for _ in range(120):
        status=manage({'operation':'daemon.status'})['data']
        if not status['active_runs'] and not status['active_operations']:break
        time.sleep(1)
    else:raise RuntimeError('wait for active work before rollback')
except subprocess.CalledProcessError:pass
shutil.copy2(stage/'backup/compose.yaml',root/'compose.yaml')
before=(stage/'backup/.env').read_text();current=(root/'.env').read_text()
old=re.search(r'^DEERFLOW_IMAGE=.*$',before,re.M)
line=old.group(0) if old else 'DEERFLOW_IMAGE=deerflow-server:pilot-20261024'
current=re.sub(r'^DEERFLOW_IMAGE=.*$',line,current,flags=re.M) if re.search(r'^DEERFLOW_IMAGE=',current,re.M) else current.rstrip()+'\n'+line+'\n'
(root/'.env').write_text(current);(root/'.env').chmod(0o600)
extension=stage/'backup/data/user-data/config/extensions_config.json'
if extension.exists():shutil.copy2(extension,root/'data/user-data/config/extensions_config.json')
subprocess.run(['docker','compose','up','-d','--no-build'],cwd=root,check=True)
time.sleep(2)
subprocess.run(['docker','exec',name,'/var/lib/deerflow/deerflow-acp','--config','/var/lib/deerflow/user-data/config/config.yaml','--daemon','/var/lib/deerflow/deerflow-acpd','--runtime-dir','/var/lib/deerflow/user-data/runtime/acp-go','--start-daemon'],check=True)
baseline=json.loads((stage/'backup/catalog-before.json').read_text())['data']['skills']
old_enabled={(x['target']['source_id'],x['target']['name']):x['head']['enabled'] for x in baseline}
names={x['name'] for x in json.loads((stage/'manifest.json').read_text())['skills']}
changed=0
for entry in manage({'operation':'skill.list','status':'current','limit':100})['data']['skills']:
    if entry['target']['name'] not in names:continue
    desired=old_enabled.get((entry['target']['source_id'],entry['target']['name']),False)
    if entry['head']['enabled']!=desired:
        manage({'operation':'skill.set_enabled','target':entry['target'],'expected_head':entry['head'],'enabled':desired,'request_key':'rollback-skills-20261024-'+entry['target']['name']});changed+=1
manage({'operation':'daemon.resume'})
print(json.dumps({'status':'rolled_back','skill_enablements_restored':changed,'data_and_skill_files_preserved':True}))
