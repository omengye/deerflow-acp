"""Offline rollback/CAS checks; no Docker, network or real credentials."""
from contextlib import ExitStack, closing
import copy
import importlib.util
import json
from pathlib import Path
import sqlite3
import tempfile
import types
import unittest
from unittest.mock import patch

HERE = Path(__file__).resolve().parent
TEMP = HERE.parents[2]/'.tmp/asr2-audiofix-deployment-tests'
TEMP.mkdir(parents=True, exist_ok=True)


def load(name, file):
    spec = importlib.util.spec_from_file_location(name, file)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


d = load('audiofix_deployment', HERE/'deploy.py')
original_deployer = load('original_asr_deployment', HERE.parent/'asr2-rollout-20261025/deploy.py')
OLD_ID = 'sha256:'+'1'*64
NEW_ID = 'sha256:'+'2'*64
V2_ID = 'sha256:'+'3'*64


class AudiofixTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(dir=TEMP)
        self.addCleanup(self.temp.cleanup)
        root = Path(self.temp.name).resolve()
        self.folo, self.root, self.rollout, self.payload = (root/n for n in ('folo','audiofix','asr2-rollout','payload'))
        self.folo.mkdir();self.rollout.mkdir();self.payload.mkdir()
        self.old_source, self.new_source = b'old reviewed asr2 module\n', b'fixed asr2 module\n'
        self.source = self.folo/'sdk/stt_sdk/volcengine_asr2.py'
        self.source.parent.mkdir(parents=True);self.source.write_bytes(self.old_source)
        self.compose = self.folo/'compose.ws.yaml';self.compose.write_text(json.dumps({'services':{'folotoy':{'image':'old-asr2','env_file':['.env.ws','private/asr2.env'],'volumes':['audio:/audio']}}}))
        self.roles = self.folo/'roles-ws.json';self.roles.write_text(json.dumps({str(n):{'stt_type':'volcengine-asr2','voice':'v'+str(n)} for n in range(1,8)}))
        self.secret = self.folo/'private/asr2.env';self.secret.parent.mkdir();self.secret.write_bytes(b'FIXTURE_PRIVATE_KEY=never-real\n')
        for name in ('config.py','util/config_util.py','sdk/stt.py'):
            p=self.folo/name;p.parent.mkdir(parents=True,exist_ok=True);p.write_text('current '+name)
        dbpath=self.folo/'data/deerflow-sessions.sqlite3';dbpath.parent.mkdir()
        with closing(sqlite3.connect(dbpath)) as db, db:
            db.execute('CREATE TABLE deerflow_sessions(status TEXT)')
            db.executemany('INSERT INTO deerflow_sessions VALUES (?)',[('completed',),('invalidated',)])
        original_backup=self.rollout/'backup-original';original_backup.mkdir()
        paths=[self.compose,self.roles,self.secret,self.folo/'config.py',self.folo/'util/config_util.py',self.folo/'sdk/stt.py',self.source]
        original_changes=[]
        for n,p in enumerate(paths):
            before=None if p in (self.source,self.secret) else ('original-v2 '+p.name).encode()
            backup_file=str(n)+'.before' if before is not None else None
            if before is not None:(original_backup/backup_file).write_bytes(before)
            original_changes.append({'path':str(p),'before_sha256':d.sha(before),'after_sha256':d.sha(p.read_bytes()),'before_mode':0o600 if before is not None else None,'after_mode':0o600,'backup_file':backup_file})
        self.original_index_path=original_backup/'index.json'
        self.original_index={'format_version':1,'deployment_id':'fixture-original','backup':str(original_backup),'folotoy_root':str(self.folo),'old_image_id':V2_ID,'changes':original_changes}
        self.original_index_path.write_bytes(d.encoded(self.original_index))
        self.original_path=self.rollout/'deployment.json'
        self.original={'format_version':1,'deployment_id':'fixture-original','status':'all_roles_ready','backup':str(original_backup),'folotoy_root':str(self.folo),'container':{'image_id':OLD_ID},'new_image':'old-asr2','runtime':{'roles':{str(n):'volcengine-asr2' for n in range(1,8)}},'manual_rollback':'original rollback command','key_label':'fixture-public-label'}
        self.original_path.write_bytes(d.encoded(self.original))
        source_target=self.payload/'folotoy-source'/d.SOURCE;source_target.parent.mkdir(parents=True);source_target.write_bytes(self.new_source)
        (self.payload/'folotoy-source/manifest.json').write_bytes(d.encoded({'files':[{'path':d.SOURCE,'before_sha256':d.sha(self.old_source),'after_sha256':d.sha(self.new_source)}]}))
        (self.payload/'Dockerfile.folotoy').write_text('FROM fake\n')
        (self.payload/'rollback.py').write_text('# reviewed fixture')
        self.args=types.SimpleNamespace(payload=self.payload,rollout_report=self.original_path,root=self.root)
        self.before={p:p.read_bytes() for p in (self.source,self.compose,self.original_path,self.original_index_path)}
        self.old_state={'image':'old-asr2','image_id':OLD_ID,'runtime_user':'1000:1000','health':'healthy','status':'running'}
        self.new_state={'image':d.IMAGE,'image_id':NEW_ID,'runtime_user':'1000:1000','health':'healthy','status':'running'}
        self.build_files=[]
        self.up=[]

    def command(self,argv,**kw):
        if argv[:3]==['docker','image','inspect']:
            return types.SimpleNamespace(stdout=json.dumps([{'Id':NEW_ID}]))
        if argv[:2]==['docker','build']:
            path=Path(argv[-1]);self.build_files.append(sorted(p.relative_to(path).as_posix() for p in path.rglob('*') if p.is_file()))
        return types.SimpleNamespace(stdout='')

    def patches(self, *, runtime_error=None, healthy_states=None):
        stack=ExitStack()
        stack.enter_context(patch.object(d,'EXPECTED_BEFORE',d.sha(self.old_source)))
        stack.enter_context(patch.object(d,'command',side_effect=self.command))
        stack.enter_context(patch.object(d,'healthy',side_effect=healthy_states or [self.old_state,self.new_state,self.old_state]))
        def candidate(raw):
            value=json.loads(raw);value['services']['folotoy']['image']=d.IMAGE;return d.encoded(value)
        stack.enter_context(patch.object(d,'compose_candidate',side_effect=candidate))
        stack.enter_context(patch.object(d,'compose_validate'))
        stack.enter_context(patch.object(d,'compose_up',side_effect=lambda folo,pinned_image=None:self.up.append(pinned_image)))
        stack.enter_context(patch.object(d,'runtime_check',side_effect=runtime_error,return_value={'module_sha256':d.sha(self.new_source),'roles':{str(n):{'provider':'volcengine-asr2'} for n in range(1,8)}}))
        return stack

    def test_success_updates_two_original_after_hashes_only_and_no_secret_in_build(self):
        old_roles,old_secret=self.roles.read_bytes(),self.secret.read_bytes()
        with self.patches():report=d.deploy(self.args)
        self.assertEqual(report['status'],'deployed_pending_live_validation')
        self.assertTrue(report['full_v2_rollback_preserved'])
        self.assertEqual(self.roles.read_bytes(),old_roles);self.assertEqual(self.secret.read_bytes(),old_secret)
        updated=json.loads(self.original_index_path.read_text())
        for before,after in zip(self.original_index['changes'],updated['changes']):
            expected=copy.deepcopy(before)
            if before['path'] in (str(self.source),str(self.compose)):expected['after_sha256']=d.sha(Path(before['path']).read_bytes())
            self.assertEqual(after,expected)
        self.assertEqual(updated['old_image_id'],V2_ID)
        self.assertEqual(self.build_files,[['Dockerfile','folotoy-source/src/sdk/stt_sdk/volcengine_asr2.py']])
        self.assertNotIn('never-real',json.dumps(report))

    def test_manual_rollback_restores_source_compose_and_original_metadata(self):
        with self.patches():
            d.deploy(self.args);report=d.rollback(self.root/'deployment.json')
        self.assertEqual(report['status'],'rolled_back');self.assertEqual(self.up[-1],OLD_ID)
        for path,before in self.before.items():self.assertEqual(path.read_bytes(),before)

    def test_runtime_failure_automatically_restores_every_target(self):
        with self.patches(runtime_error=d.DeploymentError('fixture runtime failed'),healthy_states=[self.old_state,self.new_state,self.old_state]):
            with self.assertRaises(d.DeploymentError):d.deploy(self.args)
        for path,before in self.before.items():self.assertEqual(path.read_bytes(),before)
        self.assertEqual(self.up[-1],OLD_ID)
        self.assertEqual(json.loads((self.root/'deployment.json').read_text())['status'],'rolled_back')

    def test_partial_metadata_update_failure_restores_both_indexes(self):
        real_apply=d.apply_change
        def apply(record,after):
            if record['path']==str(self.original_index_path):raise d.DeploymentError('fixture atomic index failure')
            return real_apply(record,after)
        with self.patches(),patch.object(d,'apply_change',side_effect=apply):
            with self.assertRaises(d.DeploymentError):d.deploy(self.args)
        for path,before in self.before.items():self.assertEqual(path.read_bytes(),before)
        self.assertEqual(self.up[-1],OLD_ID)

    def test_external_edit_during_build_is_not_overwritten(self):
        original_command=self.command
        def command(argv,**kw):
            value=original_command(argv,**kw)
            if argv[:2]==['docker','build']:self.source.write_bytes(b'external edit')
            return value
        with self.patches(),patch.object(d,'command',side_effect=command):
            with self.assertRaises(d.DeploymentError):d.deploy(self.args)
        self.assertEqual(self.source.read_bytes(),b'external edit')
        self.assertEqual(self.original_path.read_bytes(),self.before[self.original_path])
        self.assertEqual(self.up,[])
        self.assertEqual(json.loads((self.root/'deployment.json').read_text())['status'],'not_applied')

    def test_rollback_external_edit_refuses_before_restoring_anything(self):
        with self.patches():d.deploy(self.args)
        self.compose.write_bytes(b'external compose')
        with self.assertRaises(d.DeploymentError):d.rollback(self.root/'deployment.json')
        self.assertEqual(self.source.read_bytes(),self.new_source)
        self.assertEqual(self.compose.read_bytes(),b'external compose')
        self.assertEqual(json.loads(self.original_path.read_text())['new_image'],d.IMAGE)

    def test_original_full_v2_rollback_remains_loadable_and_restores(self):
        with self.patches():d.deploy(self.args)
        report,index,_=original_deployer.load_report(self.original_path)
        self.assertEqual(index['old_image_id'],V2_ID)
        original_deployer.restore_index(index)
        self.assertFalse(self.source.exists());self.assertFalse(self.secret.exists())
        self.assertEqual(self.roles.read_bytes(),b'original-v2 roles-ws.json')
        self.assertEqual(self.compose.read_bytes(),b'original-v2 compose.ws.yaml')

    def test_original_rollback_target_tamper_rejected_before_docker(self):
        value=copy.deepcopy(self.original_index);value['changes'][0]['path']=str(self.folo/'unrelated')
        self.original_index_path.write_bytes(d.encoded(value))
        with patch.object(d,'command') as command,self.assertRaises(d.DeploymentError):d.deploy(self.args)
        command.assert_not_called()


if __name__=='__main__':unittest.main()
