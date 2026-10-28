"""Offline rollout invariants: secret isolation, stage gating and exact rollback."""
import argparse
from contextlib import closing
import copy
import importlib.util
import io
import json
from pathlib import Path
import sqlite3
import sys
import tempfile
import types
import unittest
from unittest.mock import patch

MODULE = Path(__file__).with_name('deploy.py')
spec = importlib.util.spec_from_file_location('asr2_deployment', MODULE)
deploy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(deploy)
WORK = MODULE.parents[3] / '.tmp/asr2-deployment-tests'
WORK.mkdir(parents=True, exist_ok=True)


class RolloutTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(dir=WORK)
        self.addCleanup(self.temporary.cleanup)
        self.base = Path(self.temporary.name).resolve()
        self.folo = self.base / 'folo'
        self.payload = self.base / 'payload'
        self.root = self.base / 'rollout'
        self.folo.mkdir()
        self.payload.mkdir()
        self.roles = {str(i): {'stt_type': 'volcengine-asr', 'name': 'role' + str(i),
                              'voice': {'keep': i}, 'llm_type': 'deerflow', 'nested': [i, True]}
                      for i in range(1, 8)}
        self.original = {}
        self.write_before('roles-ws.json', (json.dumps(self.roles) + '\n').encode())
        self.write_before('compose.ws.yaml', b'services:\n  folotoy:\n    image: original\n')
        self.write_before('.env.ws', b'FOLOTOY_WS_IMAGE=original\nUNCHANGED=keep\n')
        records = []
        for name in deploy.SOURCE_FILES:
            relative = Path(name).relative_to('src')
            self.write_before(str(relative), b'# before ' + name.encode() + b'\n')
            target = self.payload / 'folotoy-source' / name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(b'# after ' + name.encode() + b'\n')
            records.append({'path': name, 'before_sha256': deploy.sha(self.original[str(relative)]),
                            'after_sha256': deploy.sha(target.read_bytes())})
        (self.payload / 'folotoy-source/manifest.json').write_text(json.dumps({'files': records}))
        (self.payload / 'Dockerfile.folotoy').write_text('FROM original\n')
        (self.payload / 'rollback.py').write_text('# rollback CLI\n')
        self.previous = {'image': 'original', 'image_id': 'sha256:' + 'a' * 64,
                         'status': 'running', 'health': 'healthy', 'runtime_user': '1000:1000'}
        self.fresh = {**self.previous, 'image': deploy.IMAGE, 'image_id': 'sha256:' + 'b' * 64}
        self.calls = []
        self.build_files = []
        self.key = 'fixture-test-api-key-123456789'

    def write_before(self, name, data):
        target = self.folo / name
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(data)
        self.original[name] = data

    def fake_command(self, argv, **kwargs):
        self.calls.append((argv, kwargs))
        if argv[:2] == ['docker', 'build']:
            context = Path(argv[-1])
            self.build_files = [p.relative_to(context).as_posix() for p in context.rglob('*') if p.is_file()]
            self.assertTrue(all(self.key.encode() not in p.read_bytes() for p in context.rglob('*') if p.is_file()))
        return types.SimpleNamespace(returncode=0, stdout='', stderr='')

    def args(self):
        return argparse.Namespace(payload=self.payload, folotoy_root=self.folo, root=self.root,
                                  allow_reapply=False)

    def start(self, fail_runtime=False):
        with patch.object(deploy, 'wait_idle'), patch.object(deploy, 'inspect', return_value=self.previous), \
                patch.object(deploy, 'compose_candidate', return_value=b'new compose\n'), \
                patch.object(deploy, 'command', side_effect=self.fake_command), \
                patch.object(deploy, 'compose_validate'), patch.object(deploy, 'compose_up') as up, \
                patch.object(deploy, 'healthy', return_value=self.previous if fail_runtime else self.fresh), \
                patch.object(deploy, 'runtime_check', side_effect=deploy.DeploymentError('fixture runtime failure') if fail_runtime else lambda selected: {'selected': sorted(selected)}), \
                patch.object(sys, 'stdin', io.StringIO(json.dumps({'api_key': self.key, 'key_label': 'fixture-key'}))):
            if fail_runtime:
                with self.assertRaises(deploy.DeploymentError):
                    deploy.stage1(self.args())
                return up.call_args_list
            return deploy.stage1(self.args())

    def accept(self, report):
        target = self.base / 'role1-passed.json'
        target.write_text(json.dumps({'passed': True, 'role': 1, 'provider': 'volcengine-asr2',
                                     'deployment_id': report['deployment_id']}))
        return argparse.Namespace(deployment_report=self.root / 'deployment.json', acceptance=target)

    def finish(self, report, fail_runtime=False):
        with patch.object(deploy, 'wait_idle'), patch.object(deploy, 'compose_up'), \
                patch.object(deploy, 'healthy', return_value=self.fresh), \
                patch.object(deploy, 'runtime_check', side_effect=[deploy.DeploymentError('new role failed'), {'selected': [1]}] if fail_runtime else lambda selected: {'selected': sorted(selected)}):
            if fail_runtime:
                with self.assertRaises(deploy.DeploymentError):
                    deploy.stage2(self.accept(report))
                return json.loads((self.root / 'deployment.json').read_text())
            return deploy.stage2(self.accept(report))

    def test_stage1_changes_only_role1_and_keeps_secret_out_of_build_and_report(self):
        report = self.start()
        current = json.loads((self.folo / 'roles-ws.json').read_text())
        expected = copy.deepcopy(self.roles)
        expected['1']['stt_type'] = 'volcengine-asr2'
        self.assertEqual(current, expected)
        self.assertEqual((self.folo / '.env.ws').read_bytes(), self.original['.env.ws'])
        self.assertEqual(report['active_asr2_roles'], [1])
        self.assertIn(self.key, (self.folo / 'private/asr2.env').read_text())
        self.assertNotIn(self.key, json.dumps(report))
        self.assertNotIn(self.key, json.dumps(self.calls))
        self.assertEqual(len(self.build_files), 5)
        self.assertNotIn('private/asr2.env', self.build_files)
        self.assertIn(str(self.payload / 'rollback.py'), report['manual_rollback'])

    def test_stage2_requires_matching_role1_acceptance_before_any_role_change(self):
        report = self.start()
        args = self.accept(report)
        invalid = json.loads(args.acceptance.read_text())
        for key, bad in [('passed', False), ('role', 2), ('provider', 'volcengine-asr'), ('deployment_id', 'different')]:
            value = {**invalid, key: bad}
            args.acceptance.write_text(json.dumps(value))
            with self.assertRaises(deploy.DeploymentError):
                deploy.stage2(args)
        current = json.loads((self.folo / 'roles-ws.json').read_text())
        self.assertEqual([n for n, value in current.items() if value['stt_type'] == 'volcengine-asr2'], ['1'])

    def test_stage2_preserves_all_non_asr_role_fields(self):
        report = self.finish(self.start())
        expected = copy.deepcopy(self.roles)
        for role in expected.values():
            role['stt_type'] = 'volcengine-asr2'
        self.assertEqual(json.loads((self.folo / 'roles-ws.json').read_text()), expected)
        self.assertEqual(report['status'], 'all_roles_ready')

    def test_stage2_failure_restores_verified_canary(self):
        report = self.finish(self.start(), fail_runtime=True)
        expected = copy.deepcopy(self.roles)
        expected['1']['stt_type'] = 'volcengine-asr2'
        self.assertEqual(json.loads((self.folo / 'roles-ws.json').read_text()), expected)
        self.assertEqual(report['status'], 'role1_ready_stage2_rolled_back')

    def test_manual_rollback_restores_exact_files_and_pins_old_image(self):
        self.finish(self.start())
        with patch.object(deploy, 'wait_idle'), patch.object(deploy, 'compose_validate'), \
                patch.object(deploy, 'compose_up') as up, patch.object(deploy, 'healthy', return_value=self.previous):
            report = deploy.rollback(self.root / 'deployment.json')
        for name, raw in self.original.items():
            self.assertEqual((self.folo / name).read_bytes(), raw)
        self.assertFalse((self.folo / 'private/asr2.env').exists())
        self.assertEqual(up.call_args.kwargs['pinned_image'], self.previous['image_id'])
        self.assertTrue(report['rollback_image_verified'])

    def test_rollback_external_edit_stops_before_any_file_is_restored(self):
        self.start()
        changed = self.folo / 'sdk/stt.py'
        changed.write_text('# external modification\n')
        compose = (self.folo / 'compose.ws.yaml').read_bytes()
        with patch.object(deploy, 'wait_idle'), self.assertRaises(deploy.DeploymentError):
            deploy.rollback(self.root / 'deployment.json')
        self.assertEqual(changed.read_text(), '# external modification\n')
        self.assertEqual((self.folo / 'compose.ws.yaml').read_bytes(), compose)
        self.assertTrue((self.folo / 'private/asr2.env').is_file())

    def test_stage1_runtime_failure_restores_original_files(self):
        calls = self.start(fail_runtime=True)
        for name, raw in self.original.items():
            self.assertEqual((self.folo / name).read_bytes(), raw)
        self.assertFalse((self.folo / 'private/asr2.env').exists())
        self.assertEqual(calls[-1].kwargs['pinned_image'], self.previous['image_id'])

    def test_idle_check_ignores_completed_and_invalidated_without_mutation(self):
        data = self.folo / 'data'
        data.mkdir()
        with closing(sqlite3.connect(data / 'deerflow-sessions.sqlite3')) as db, db:
            db.execute('CREATE TABLE deerflow_sessions(status TEXT)')
            db.executemany('INSERT INTO deerflow_sessions VALUES (?)', [('completed',), ('invalidated',)])
        deploy.wait_idle(self.folo, timeout=1)
        with closing(sqlite3.connect(data / 'deerflow-sessions.sqlite3')) as db:
            self.assertEqual(db.execute('SELECT status FROM deerflow_sessions').fetchall(), [('completed',), ('invalidated',)])


if __name__ == '__main__':
    unittest.main()
