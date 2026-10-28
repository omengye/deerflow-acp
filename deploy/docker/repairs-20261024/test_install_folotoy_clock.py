"""Offline installer checks with simulated Docker commands and fixture secrets."""

from contextlib import redirect_stdout
import importlib.util
import io
import json
import os
from pathlib import Path
import sys
import tempfile
import types
import unittest
from unittest.mock import patch

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location('folotoy_clock_installer', HERE / 'install-folotoy-clock.py')
installer = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = installer
spec.loader.exec_module(installer)


class InstallerTests(unittest.TestCase):
    def setUp(self):
        workspace = HERE.parents[2]
        temporary = workspace / '.tmp/gentoo-fix/installer-tests'
        temporary.mkdir(parents=True, exist_ok=True)
        self.temporary = tempfile.TemporaryDirectory(dir=temporary)
        self.addCleanup(self.temporary.cleanup)
        self.base = Path(self.temporary.name)
        self.root = self.base / 'deployment/src'
        self.root.mkdir(parents=True)
        self.provider = self.root / 'sdk/llm_sdk/deerflow.py'
        self.provider.parent.mkdir(parents=True)
        self.provider.write_bytes(b'original provider\n')
        self.new_source = self.base / 'new-provider.py'
        self.new_source.write_bytes(b'reviewed clock provider\n')
        self.compose = self.root / 'compose.ws.yaml'
        self.compose.write_bytes(b'services:\r\n  folotoy:\r\n    image: ${FOLOTOY_WS_IMAGE:-folotoy-deerflow-ws:20261024}\r\n    env_file: .env.ws\r\n')
        (self.root / '.env').write_bytes(b'# retain bytes\r\nPRIVATE_TOKEN=fixture-secret\r\nFOLOTOY_WS_IMAGE=previous\r\n')
        (self.root / '.env.ws').write_bytes(b'ASR_TOKEN=fixture-secret\nFOLOTOY_WS_IMAGE="previous" # prior tag\n')
        self.original = {path: path.read_bytes() for path in
                         (self.provider, self.compose, self.root / '.env', self.root / '.env.ws')}
        self.calls = []
        self.build_hook = None
        self.fail_compose_number = None
        self.compose_count = 0
        self.image_id = 'sha256:' + 'a' * 64
        self.patches = [patch.object(installer.tempfile, 'tempdir', str(temporary)),
                        patch.object(installer, 'OLD_SHA', installer.digest(self.provider.read_bytes())),
                        patch.object(installer, 'NEW_SHA', installer.digest(self.new_source.read_bytes())),
                        patch.object(installer, 'command', side_effect=self.docker)]
        for item in self.patches:
            item.start()
            self.addCleanup(item.stop)

    def docker(self, argv, *, cwd=None):
        self.calls.append(argv)
        if argv[:2] == ['docker', 'build']:
            context = Path(argv[-1])
            self.assertEqual(sorted(path.name for path in context.iterdir()), ['Dockerfile', 'deerflow.py'])
            self.assertEqual((context / 'deerflow.py').read_bytes(), self.new_source.read_bytes())
            self.assertIn('--network=none', argv)
            self.assertNotIn(b'fixture-secret', (context / 'Dockerfile').read_bytes())
            if self.build_hook:
                self.build_hook()
        elif argv[:3] == ['docker', 'image', 'inspect']:
            return self.image_id
        elif argv[:2] == ['docker', 'compose']:
            self.assertEqual(argv[-2:], ['config', '--quiet'])
            self.assertEqual(cwd, self.root.resolve())
            self.compose_count += 1
            if self.compose_count == self.fail_compose_number:
                raise installer.RepairError('simulated validation failure')
        else:
            self.fail('Unexpected Docker lifecycle operation')
        return ''

    def test_dry_run_verifies_without_build_or_changes(self):
        report = installer.install(self.root, self.new_source)
        self.assertEqual(report['status'], 'planned')
        self.assertFalse(report['services_restarted'])
        self.assertEqual(self.calls, [])
        for path, data in self.original.items():
            self.assertEqual(path.read_bytes(), data)
        self.assertFalse((self.root.parent / 'backups').exists())

    def test_apply_uses_tiny_context_and_private_backups(self):
        report = installer.install(self.root, self.new_source, apply=True)
        self.assertEqual(report['status'], 'applied')
        self.assertEqual(report['image_sha256'], self.image_id)
        self.assertEqual(self.provider.read_bytes(), self.new_source.read_bytes())
        self.assertIn(installer.NEW_IMAGE.encode(), self.compose.read_bytes())
        self.assertEqual((self.root / '.env').read_bytes(),
                         b'# retain bytes\r\nPRIVATE_TOKEN=fixture-secret\r\nFOLOTOY_WS_IMAGE=' + installer.NEW_IMAGE.encode() + b'\r\n')
        self.assertEqual((self.root / '.env.ws').read_bytes(),
                         b'ASR_TOKEN=fixture-secret\nFOLOTOY_WS_IMAGE=' + installer.NEW_IMAGE.encode() + b'\n')
        backup = Path(report['backup'])
        self.assertEqual((backup / 'provider.before.py').read_bytes(), self.original[self.provider])
        self.assertEqual((backup / 'compose.ws.yaml.before').read_bytes(), self.original[self.compose])
        self.assertEqual((backup / '.env.before').read_bytes(), self.original[self.root / '.env'])
        if os.name == 'posix':
            self.assertEqual(backup.stat().st_mode & 0o777, 0o700)
            self.assertTrue(all(path.stat().st_mode & 0o777 == 0o600 for path in backup.iterdir()))
        self.assertNotIn('fixture-secret', json.dumps(report))
        self.assertFalse(report['services_restarted'])

    def test_absent_override_is_never_appended_or_created(self):
        (self.root / '.env').unlink()
        unmodified = b'ASR_TOKEN=fixture-secret\r\n'
        (self.root / '.env.ws').write_bytes(unmodified)
        installer.install(self.root, self.new_source, apply=True)
        self.assertFalse((self.root / '.env').exists())
        self.assertEqual((self.root / '.env.ws').read_bytes(), unmodified)

    def test_provider_or_source_checksum_mismatch_has_no_docker_effect(self):
        self.provider.write_bytes(b'concurrent provider')
        with self.assertRaisesRegex(installer.RepairError, 'checksum'):
            installer.install(self.root, self.new_source, apply=True)
        self.provider.write_bytes(self.original[self.provider])
        self.new_source.write_bytes(b'unreviewed source')
        with self.assertRaisesRegex(installer.RepairError, 'checksum'):
            installer.install(self.root, self.new_source, apply=True)
        self.assertEqual(self.calls, [])

    def test_concurrent_change_during_build_is_preserved(self):
        self.build_hook = lambda: self.compose.write_bytes(b'concurrent compose update')
        with self.assertRaisesRegex(installer.RepairError, 'concurrently'):
            installer.install(self.root, self.new_source, apply=True)
        self.assertEqual(self.compose.read_bytes(), b'concurrent compose update')
        self.assertEqual(self.provider.read_bytes(), self.original[self.provider])
        self.assertFalse((self.root.parent / 'backups').exists())

    def test_post_write_compose_failure_restores_original_bytes(self):
        self.fail_compose_number = 2
        with self.assertRaisesRegex(installer.RepairError, 'original files restored'):
            installer.install(self.root, self.new_source, apply=True)
        for path, data in self.original.items():
            self.assertEqual(path.read_bytes(), data)
        self.assertEqual(len(list((self.root.parent / 'backups').iterdir())), 1)

    def test_second_apply_is_idempotent(self):
        installer.install(self.root, self.new_source, apply=True)
        report = installer.install(self.root, self.new_source, apply=True)
        self.assertEqual(report['status'], 'already_configured')
        self.assertEqual(report['changed_files'], [])
        self.assertIsNone(report['backup'])
        self.assertEqual(len(list((self.root.parent / 'backups').iterdir())), 1)

    def test_duplicate_env_assignments_are_rejected_without_docker(self):
        (self.root / '.env').write_bytes(b'FOLOTOY_WS_IMAGE=a\nFOLOTOY_WS_IMAGE=b\n')
        with self.assertRaisesRegex(installer.RepairError, 'Duplicate'):
            installer.install(self.root, self.new_source, apply=True)
        self.assertEqual(self.calls, [])


if __name__ == '__main__':
    unittest.main()
