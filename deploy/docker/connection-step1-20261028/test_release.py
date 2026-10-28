"""Exercise release boundaries without Docker, production access, or credentials."""
from contextlib import ExitStack
import importlib.util
import json
from pathlib import Path
import sys
import tempfile
import types
import unittest
from unittest.mock import Mock, patch


# The bundled Python runtime need not include websocket-client. The production
# module is imported with a stub only in this test; network entrypoints are
# always patched explicitly below.
try:
    import websocket
except ImportError:
    websocket = types.ModuleType('websocket')
    websocket.WebSocketConnectionClosedException = type('Closed', (Exception,), {})
    websocket.create_connection = Mock(side_effect=AssertionError('network forbidden'))

with patch.dict(sys.modules, {'websocket': websocket}):
    spec = importlib.util.spec_from_file_location('connection_step1_release', Path(__file__).with_name('release.py'))
    release = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(release)


CANDIDATE = 'sha256:' + 'a' * 64


def idle(draining=False, **changes):
    return dict(active_operations=0, active_runs=0, queued_runs=0, draining=draining, **changes)


class ReleaseTest(unittest.TestCase):
    def setUp(self):
        self.stack = ExitStack()
        self.addCleanup(self.stack.close)
        self.root = Path(self.stack.enter_context(tempfile.TemporaryDirectory(prefix='step1-release-test-')))
        self.task = self.root / 'release'
        self.task.mkdir()
        self.original = b'DEERFLOW_IMAGE=previous-image\nPRIVATE_SETTING=fixture-only\n'
        (self.root / '.env').write_bytes(self.original)
        self.data = self.root / 'business-data'
        self.data.mkdir()
        (self.data / 'sessions.sqlite').write_bytes(b'durable-business-data')
        (self.data / 'artifact.bin').write_bytes(b'durable-artifact')
        self.write_evidence()
        self.current_image = release.BASE_ID
        self.status = idle()
        self.drain_results = []
        self.controls = []
        self.commands = []
        self.trace = []
        self.patch('ROOT', self.root)
        self.patch('TASK', self.task)
        self.patch('image_id', Mock(return_value=CANDIDATE))
        self.patch('inspect', lambda: {'Image': self.current_image})
        self.patch('run', self.run_command)
        self.patch('control', self.control)
        self.health = self.patch('healthy', Mock())
        self.verify = self.patch('verify', Mock(side_effect=lambda: self.trace.append('verify')))

    def patch(self, name, value):
        return self.stack.enter_context(patch.object(release, name, value))

    def write_evidence(self, **changes):
        evidence = {'passed': True, 'protocol': 10, 'image_id': CANDIDATE}
        evidence.update(changes)
        (self.task / 'smoke.json').write_text(json.dumps(evidence))

    def run_command(self, args, **_):
        self.commands.append(args)
        self.trace.append('compose')
        self.assertEqual(args, release.compose('up', '-d', '--no-deps', 'deerflow'))
        self.current_image = release.BASE_ID if (self.root / '.env').read_bytes() == self.original else CANDIDATE
        return ''

    def control(self, operation):
        self.controls.append(operation)
        self.trace.append(operation)
        if operation == 'daemon.status':
            return self.status
        if operation == 'daemon.drain':
            if self.drain_results:
                result = self.drain_results.pop(0)
                if isinstance(result, Exception):
                    raise result
                if callable(result):
                    return result()
                return result
            return idle(True)
        self.assertEqual(operation, 'daemon.resume')
        return idle()

    def assert_original(self):
        self.assertEqual((self.root / '.env').read_bytes(), self.original)
        self.assertEqual(self.current_image, release.BASE_ID)
        self.assert_business_intact()

    def assert_business_intact(self):
        self.assertEqual((self.data / 'sessions.sqlite').read_bytes(), b'durable-business-data')
        self.assertEqual((self.data / 'artifact.bin').read_bytes(), b'durable-artifact')

    def test_active_or_queued_work_prevents_every_mutation(self):
        for key in ('active_operations', 'active_runs', 'queued_runs'):
            with self.subTest(key=key):
                self.status = idle()
                self.status[key] = 1
                self.controls.clear()
                with self.assertRaisesRegex(RuntimeError, 'active_work_wait_required'):
                    release.apply()
                self.assertEqual(self.controls, ['daemon.status'])
                self.assertEqual(self.commands, [])
                self.assertFalse((self.task / 'previous.env').exists())
                self.assert_original()

    def test_changed_production_baseline_prevents_drain_and_env_write(self):
        self.current_image = 'sha256:' + 'b' * 64
        with self.assertRaisesRegex(RuntimeError, 'deployment_baseline_changed'):
            release.apply()
        self.assertEqual(self.controls, [])
        self.assertEqual(self.commands, [])
        self.assertEqual((self.root / '.env').read_bytes(), self.original)

    def test_smoke_must_match_the_immutable_candidate_image(self):
        self.write_evidence(image_id='sha256:' + 'b' * 64)
        with self.assertRaisesRegex(RuntimeError, 'smoke_image_changed'):
            release.apply()
        self.assertEqual(self.controls, [])
        self.assertEqual(self.commands, [])
        self.assert_original()

    def test_existing_drain_is_not_released_by_an_unrelated_attempt(self):
        self.status = idle(True)
        with self.assertRaisesRegex(RuntimeError, 'deployment_already_drained'):
            release.apply()
        self.assertEqual(self.controls, ['daemon.status'])
        self.assert_original()

    def test_lost_drain_response_still_resumes_original_daemon(self):
        self.drain_results = [RuntimeError('response_lost')]
        with self.assertRaisesRegex(RuntimeError, 'response_lost'):
            release.apply()
        self.assertEqual(self.controls, ['daemon.status', 'daemon.drain', 'daemon.resume'])
        self.assertEqual(self.commands, [])
        self.assert_original()

    def test_work_racing_with_initial_drain_does_not_restart_container(self):
        busy = idle(True)
        busy['active_operations'] = 1
        self.drain_results = [busy]
        with self.assertRaisesRegex(RuntimeError, 'active_work_started'):
            release.apply()
        self.assertEqual(self.commands, [])
        self.assertEqual(self.controls[-1], 'daemon.resume')
        self.assert_original()

    def test_concurrent_configuration_edit_is_preserved(self):
        edited = self.original + b'NEW_SETTING=keep\n'
        def drain():
            (self.root / '.env').write_bytes(edited)
            return idle(True)
        self.drain_results = [drain]
        with self.assertRaisesRegex(RuntimeError, 'configuration_changed'):
            release.apply()
        self.assertEqual((self.root / '.env').read_bytes(), edited)
        self.assertEqual(self.commands, [])
        self.assertEqual(self.controls[-1], 'daemon.resume')

    def test_failed_postdeploy_health_rolls_back_env_and_image_only(self):
        self.health.side_effect = [RuntimeError('unhealthy'), None]
        with self.assertRaisesRegex(RuntimeError, 'unhealthy'):
            release.apply()
        self.assertEqual(len(self.commands), 2)
        self.verify.assert_not_called()
        self.assertEqual(self.controls[-1], 'daemon.resume')
        self.assertEqual((self.task / 'previous.env').read_bytes(), self.original)
        self.assert_original()

    def test_failed_protocol_verification_rolls_back_without_restoring_data(self):
        self.verify.side_effect = RuntimeError('rpc_contract_changed')
        with self.assertRaisesRegex(RuntimeError, 'rpc_contract_changed'):
            release.apply()
        self.assertEqual(len(self.commands), 2)
        self.assertEqual(self.controls.count('daemon.resume'), 1)
        self.assert_original()

    def test_retry_after_rollback_reuses_exact_original_backup(self):
        self.verify.side_effect = RuntimeError('first_attempt_unreachable')
        with self.assertRaisesRegex(RuntimeError, 'first_attempt_unreachable'):
            release.apply()
        self.assert_original()
        backup = self.task / 'previous.env'
        original_stat = backup.stat()
        self.verify.side_effect = lambda: self.trace.append('verify')
        release.apply()
        self.assertEqual(self.current_image, CANDIDATE)
        self.assertEqual(backup.read_bytes(), self.original)
        self.assertEqual(backup.stat().st_mtime_ns, original_stat.st_mtime_ns)
        self.assertEqual(len(self.commands), 3)
        self.assertTrue(json.loads((self.task / 'deployment.json').read_text())['passed'])
        self.assert_business_intact()

    def test_mismatched_existing_backup_aborts_without_replacing_it(self):
        backup = self.task / 'previous.env'
        backup.write_bytes(b'configuration-from-another-release')
        with self.assertRaisesRegex(RuntimeError, 'previous_configuration_backup_changed'):
            release.apply()
        self.assertEqual(backup.read_bytes(), b'configuration-from-another-release')
        self.assertEqual(self.commands, [])
        self.assertEqual(self.controls[-1], 'daemon.resume')
        self.assert_original()

    def test_success_pins_tested_image_and_keeps_drain_until_verification_finishes(self):
        release.apply()
        value = (self.root / '.env').read_bytes()
        self.assertEqual(value, self.original.replace(b'previous-image', CANDIDATE.encode()))
        self.assertEqual(self.current_image, CANDIDATE)
        self.assertLess(self.trace.index('verify'), self.trace.index('daemon.resume'))
        self.assertEqual(self.controls.count('daemon.drain'), 2)
        self.assertEqual(self.controls.count('daemon.resume'), 1)
        self.assertEqual(len(self.commands), 1)
        report = json.loads((self.task / 'deployment.json').read_text())
        self.assertTrue(report['passed'])
        self.assertFalse(report['business_data_restored'])
        self.assertEqual(report['image_id'], CANDIDATE)
        self.assert_business_intact()

    def test_new_work_after_restart_is_preserved_instead_of_interrupted_by_rollback(self):
        busy = idle(True)
        busy['active_runs'] = busy['active_operations'] = 1
        self.drain_results = [idle(True), busy]
        with self.assertRaisesRegex(RuntimeError, 'active_work_started_after_restart'):
            release.apply()
        self.assertEqual(len(self.commands), 1)
        self.assertEqual(self.current_image, CANDIDATE)
        self.verify.assert_not_called()
        self.assertEqual(self.controls[-1], 'daemon.resume')
        self.assertTrue(json.loads((self.task / 'deployment.json').read_text())['verification_pending'])
        self.assert_business_intact()

    def test_failed_atomic_write_cleans_only_own_temp_and_preserves_env(self):
        with patch.object(release.os, 'fsync', side_effect=OSError('disk error')):
            with self.assertRaises(OSError):
                release.atomic_env(b'candidate')
        self.assertFalse((self.root / '.env.connection-step1-tmp').exists())
        self.assert_original()

    def test_existing_atomic_temp_is_not_deleted(self):
        temporary = self.root / '.env.connection-step1-tmp'
        temporary.write_bytes(b'other-attempt')
        with self.assertRaises(FileExistsError):
            release.atomic_env(b'candidate')
        self.assertEqual(temporary.read_bytes(), b'other-attempt')
        self.assert_original()


class HandshakeTest(unittest.TestCase):
    def test_event_traffic_cannot_extend_rpc_deadline_forever(self):
        socket = Mock()
        socket.recv.return_value = json.dumps({'type': 'hello'})
        with patch.object(release.websocket, 'create_connection', return_value=socket), \
                patch.object(release.time, 'sleep'), \
                patch.object(release.time, 'monotonic', side_effect=[0, 1, 37]):
            with self.assertRaisesRegex(RuntimeError, 'rpc_timeout'):
                release.handshake('ws://fixture/v1', 'fixture-token', rpc=True)
        socket.close.assert_called_once()

    def test_load_task_state_contract_accepts_task_state_and_closes_socket(self):
        pending = []
        def send(raw):
            value = json.loads(raw)
            if value['type'] == 'hello':
                pending.append({'type': 'hello'})
            else:
                pending.append({'type': 'event'})
                kind = {'getSettings': 'settings', 'loadTaskState': 'taskState'}[value['command']['type']]
                pending.append({'type': 'response', 'requestId': value['requestId'],
                                'outcome': {'status': 'ok', 'payload': {'type': kind}}})
        socket = Mock()
        socket.send.side_effect = send
        socket.recv.side_effect = lambda: json.dumps(pending.pop(0))
        with patch.object(release.websocket, 'create_connection', return_value=socket), \
                patch.object(release.time, 'sleep'):
            result = release.handshake('ws://fixture/v1', 'fixture-token', rpc=True)
        self.assertEqual(result['rpc'], {'getSettings': True, 'loadTaskState': True})
        socket.close.assert_called_once()


class SmokeTest(unittest.TestCase):
    def test_smoke_runs_and_records_same_immutable_image(self):
        def handshake(*args, **kwargs):
            return {'result': kwargs.get('expected', 'hello')}
        with patch.object(release, 'image_id', return_value=CANDIDATE), \
                patch.object(release, 'run') as command, \
                patch.object(release, 'handshake', side_effect=handshake), \
                patch.object(release, 'save') as save, patch('builtins.print'):
            release.smoke()
        self.assertIn(CANDIDATE, command.call_args_list[0].args[0])
        self.assertNotIn(release.IMAGE, command.call_args_list[0].args[0])
        self.assertEqual(save.call_args.args[1]['image_id'], CANDIDATE)
        self.assertEqual(command.call_args.args[0], ['docker', 'stop', release.SMOKE])

    def test_failed_smoke_removes_isolated_container_and_never_records_pass(self):
        with patch.object(release, 'image_id', return_value=CANDIDATE), \
                patch.object(release, 'run') as command, \
                patch.object(release, 'handshake', side_effect=RuntimeError('contract_failed')), \
                patch.object(release, 'save') as save:
            with self.assertRaisesRegex(RuntimeError, 'contract_failed'):
                release.smoke()
        self.assertEqual(command.call_args.args[0], ['docker', 'stop', release.SMOKE])
        save.assert_not_called()


if __name__ == '__main__':
    unittest.main()
