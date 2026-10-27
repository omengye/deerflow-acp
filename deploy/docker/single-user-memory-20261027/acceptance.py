"""Authenticated Waku acceptance with synthetic facts and real provider calls.

Run on the authorized Gentoo host. Tokens and personal contents never leave the
process. Test facts are forgotten in finally; reports contain only pass/fail.
"""
import argparse
import collections
import json
from pathlib import Path
import socket
import subprocess
import time
import uuid
import websocket

NIL=str(uuid.UUID(int=0))
TASK=Path('/home/gen/deerflow-deploy/single-user-memory-20261027')

def require(value, code):
    if not value:raise RuntimeError(code)

def server_token():
    info=json.loads(subprocess.check_output(['docker','inspect','deerflow-deploy-deerflow-1'],text=True))[0]
    return next(v.split('=',1)[1] for v in info['Config']['Env'] if v.startswith('WAKU_DAEMON_TOKEN='))

class Client:
    def __init__(self,url):
        self.ws=websocket.create_connection(url,timeout=30,suppress_origin=True,http_no_proxy=['*'])
        self.events=[]
        self.ws.send(json.dumps({'type':'hello','protocolVersion':10,'token':server_token(),'clientId':str(uuid.uuid4()),'resumeFrom':[]}))
        require(self.read().get('type')=='hello','hello_failed')
    def read(self):return json.loads(self.ws.recv())
    def request(self,command,sid=NIL,rid=NIL,wait=True):
        id=str(uuid.uuid4());self.ws.send(json.dumps({'type':'request','requestId':id,'sessionId':sid,'runtimeId':rid,'command':command}))
        if not wait:return
        end=time.monotonic()+40
        while time.monotonic()<end:
            x=self.read()
            if x.get('type')=='event':self.events.append(x)
            if x.get('type')=='response' and x.get('requestId')==id:
                require(x.get('outcome',{}).get('status')=='ok','rpc_rejected')
                return x['outcome']['payload']
        raise RuntimeError('rpc_timeout')
    def manage(self,operation,**params):
        data=self.request({'type':'deerFlow','operation':'manage-envelope','input':{'operation':operation,**params}})['data']
        require(data.get('ok'),data.get('code','manage_failed'))
        return data['data']
    def close(self,abrupt=False):
        if abrupt and self.ws.sock:
            self.ws.sock.shutdown(socket.SHUT_RDWR);self.ws.sock.close()
        else:self.ws.close()
    def session(self,workspace='/workspaces'):
        sid,rid=str(uuid.uuid4()),str(uuid.uuid4())
        options={'provider':'deerFlow','binary':'/var/lib/deerflow/deerflow-acp','cwd':workspace,'mode':'fullAccess','model':None,'reasoningEffort':None,'thinkingMode':None,'serviceTier':None,'contextWindow':None,'agentPreset':None,'computerUseEnabled':False,'providerCursor':None}
        self.events=[]
        self.request({'type':'start','options':options},sid,rid)
        end=time.monotonic()+40
        while not any(x.get('sessionId')==sid and x.get('event',{}).get('kind')=='connected' for x in self.events):
            require(time.monotonic()<end,'start_timeout');self.events.append(self.read())
        self.request({'type':'setToolApproval','mode':'allow_always'},sid,rid)
        return sid,rid
    def prompt(self,session,text):
        self.events=[]
        self.request({'type':'prompt','prompt':text,'turnId':str(uuid.uuid4()),'messageId':str(uuid.uuid4())},*session)
        end=time.monotonic()+150;reply=[];kinds=collections.Counter()
        while time.monotonic()<end:
            x=self.events.pop(0) if self.events else self.read()
            if x.get('type')!='event' or x.get('sessionId')!=session[0]:continue
            event=x.get('event',{});kind=event.get('kind');payload=event.get('payload');kinds[kind]+=1
            if kind=='textDelta' and isinstance(payload,str):reply.append(payload)
            if kind=='permission':
                options=payload.get('options',[])
                option=next((v for v in options if v.get('allow') and v.get('id') in ('allow_always','allow_once')),None)
                require(option,'permission_unavailable')
                self.request({'type':'respond','requestId':payload['requestId'],'optionId':option['id']},*session,wait=False)
            if kind=='turnFinished':
                require(payload.get('success') is True,'turn_failed')
                return ''.join(reply)
            require(kind not in ('error','processExited'),'provider_error')
        self.request({'type':'cancel'},*session,wait=False)
        raise RuntimeError('turn_timeout')

def acceptance(url,stage):
    client=Client(url);sessions=[];keys=['profile.memory_probe_marker','profile.memory_acceptance_code'];checks={};profile=None
    suffix=uuid.uuid4().hex[:12];code='MEM-'+suffix
    try:
        snapshot=client.manage('memory.personal.get');profile=snapshot['profile_id']
        checks['migrated_personal_present']=any(f['key']=='profile.last_reported_location' for f in snapshot['facts'])
        require(checks['migrated_personal_present'],'migration_not_visible')
        operation={'profile_id':profile,'operation_id':str(uuid.uuid4()),'action':'remember','key':keys[0],'content':'这是临时验收记录 '+suffix,'category':'profile','expected_revision':snapshot['versions'].get(keys[0],0)}
        client.request({'type':'deerFlow','operation':'manage-envelope','input':{'operation':'memory.personal.mutate',**operation}},wait=False)
        time.sleep(.3);client.close(abrupt=True);client=Client(url)
        recovered=client.manage('memory.personal.receipt',operation_id=operation['operation_id'])
        receipt=recovered.get('receipt') or client.manage('memory.personal.mutate',**operation)
        duplicate=client.manage('memory.personal.mutate',**operation)
        require(receipt==duplicate,'idempotent_replay_changed')
        checks['lost_ack_reconciled']=True
        revision=receipt['revision']
        updated={**operation,'operation_id':str(uuid.uuid4()),'content':'验收修订 '+suffix,'expected_revision':revision}
        client.manage('memory.personal.mutate',**updated)
        try:client.manage('memory.personal.mutate',**{**operation,'operation_id':str(uuid.uuid4())})
        except RuntimeError as error:require(str(error)=='memory_conflict','wrong_conflict_error')
        else:raise RuntimeError('stale_edit_accepted')
        checks['conflict_protected']=True
        a=client.session();sessions.append(a)
        client.prompt(a,'请长期记住一条用于验收的虚拟资料：我的验收代号是 '+code+'。使用 global 范围和 profile.memory_acceptance_code 属性保存，成功保存后简短确认。不要使用文件工具。')
        current=client.manage('memory.personal.get')
        require(any(f['key']==keys[1] and code in f['content'] for f in current['facts']),'explicit_tool_not_committed')
        checks['explicit_save_committed']=True
        b=client.session('/workspaces/folotoy');sessions.append(b)
        answer=client.prompt(b,'我的验收代号是什么？只回答已保存的代号，不要猜测。')
        require(code in answer,'new_session_recall_failed')
        checks['cross_session_cross_workspace_recall']=True
        if stage and not any(f['key']=='profile.name' for f in snapshot['facts']):
            keys.append('profile.name')
            c=client.session();sessions.append(c)
            client.prompt(c,'我叫验收秋枫。请只回复收到，不要调用任何工具。')
            deadline=time.monotonic()+150
            while time.monotonic()<deadline:
                current=client.manage('memory.personal.get')
                if any(f['key']=='profile.name' and '验收秋枫' in f['content'] and f['source'].get('policy')=='personal-extraction-v1' for f in current['facts']):break
                time.sleep(2)
            else:raise RuntimeError('automatic_extraction_not_committed')
            checks['automatic_extraction_committed']=True
        for key in keys:
            current=client.manage('memory.personal.get')
            client.manage('memory.personal.mutate',profile_id=profile,operation_id=str(uuid.uuid4()),action='forget',key=key,expected_revision=current['versions'].get(key,0))
        current=client.manage('memory.personal.get')
        require(not any(f['key'] in keys for f in current['facts']),'forget_not_committed')
        checks['forget_confirmed']=True
        checks['passed']=True
        return checks
    finally:
        for session in sessions:
            for command in ('closeSession','removeSession'):
                try:client.request({'type':command},*session)
                except Exception:pass
        try:
            current=client.manage('memory.personal.get')
            for key in keys:
                if any(f['key']==key for f in current['facts']):client.manage('memory.personal.mutate',profile_id=current['profile_id'],operation_id=str(uuid.uuid4()),action='forget',key=key,expected_revision=current['versions'][key])
        finally:client.close()

if __name__=='__main__':
    p=argparse.ArgumentParser();p.add_argument('--stage',action='store_true');p.add_argument('--url');args=p.parse_args()
    report={}
    try:report=acceptance(args.url or ('ws://127.0.0.1:18788/v1' if args.stage else 'wss://deerflow.sprwhisp.cc/v1'),args.stage)
    except Exception as error:report={'passed':False,'error':str(error) if isinstance(error,RuntimeError) else type(error).__name__}
    file=TASK/('acceptance-stage.json' if args.stage else 'acceptance-production.json')
    file.write_text(json.dumps(report,indent=2));file.chmod(0o600)
    print(json.dumps(report));raise SystemExit(0 if report.get('passed') else 1)
