"""Regression checks for the rollout's data-preservation and recovery guards."""
import importlib.util
from contextlib import closing
from pathlib import Path
import sqlite3
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

spec = importlib.util.spec_from_file_location('storage_release', Path(__file__).with_name('release.py'))
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)


class ReleaseGuards(unittest.TestCase):
    def test_binding_rejects_outside_and_traversal(self):
        self.assertEqual(release.binding_for(release.PRIMARY), b'{"data_dir":"config/go-harness-state"}\n')
        for path in ('relative/db', '/tmp/db', '/var/lib/deerflow/user-data',
                     '/var/lib/deerflow/user-data/../../db'):
            with self.subTest(path=path), self.assertRaises((ValueError, RuntimeError)):
                release.binding_for(path)

    def test_mount_order_ignored_but_identity_and_access_checked(self):
        first = {'Source': '/original', 'Destination': '/data', 'Type': 'bind', 'RW': True}
        second = {'Source': '/wrapper', 'Destination': '/bin', 'Type': 'bind', 'RW': False}
        baseline = release.mounts([first, second])
        self.assertEqual(baseline, release.mounts([second, first]))
        self.assertNotEqual(baseline, release.mounts([second, dict(first, Source='/new')]))
        self.assertNotEqual(baseline, release.mounts([first, dict(second, RW=True)]))

    def test_alternate_new_file_and_content_changes_detected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / 'alternate'
            absent = release.tree_hashes(root)
            root.mkdir()
            self.assertNotEqual(absent, release.tree_hashes(root))
            db = root / 'harness.db'
            db.write_bytes(b'original')
            original = release.tree_hashes(root)
            (root / 'new.db').write_bytes(b'accidental new database')
            self.assertNotEqual(original, release.tree_hashes(root))
            db.write_bytes(b'replaced')
            self.assertNotEqual(original['files']['harness.db'], release.tree_hashes(root)['files']['harness.db'])

    def test_history_allows_new_activity_but_rejects_loss_or_rewrite(self):
        with tempfile.TemporaryDirectory() as directory:
            before, after = (Path(directory) / name for name in ('before.db', 'after.db'))
            for path in (before, after):
                with closing(sqlite3.connect(path, isolation_level=None)) as db:
                    db.execute('CREATE TABLE events (id INTEGER PRIMARY KEY, payload BLOB)')
                    db.execute('INSERT INTO events VALUES (1, ?)', (b'original',))
            with closing(sqlite3.connect(after, isolation_level=None)) as db:
                db.execute('INSERT INTO events VALUES (2, ?)', (b'new activity',))
            check = lambda: release.preserve_rows(before, after, ['events'], {'events': ['payload']})
            self.assertEqual(check()['events'], {'before': 1, 'after': 2, 'missing': 0, 'payloads_changed': 0})
            with closing(sqlite3.connect(after, isolation_level=None)) as db:
                db.execute('UPDATE events SET payload=? WHERE id=1', (b'rewritten',))
            with self.assertRaisesRegex(RuntimeError, 'payload changed'):
                check()
            with closing(sqlite3.connect(after, isolation_level=None)) as db:
                db.execute('DELETE FROM events WHERE id=1')
            with self.assertRaisesRegex(RuntimeError, 'records missing'):
                check()

    def test_empty_resume_cannot_pass(self):
        with patch.object(release, 'ACP') as acp:
            with self.assertRaisesRegex(RuntimeError, 'no historical sessions'):
                release.native_resume('container', [], 0)
            acp.assert_not_called()

    def test_native_retention_empty_timestamps_and_foreground_selection(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'harness.db'
            with closing(sqlite3.connect(path, isolation_level=None)) as db:
                db.execute("CREATE TABLE harness_sessions (id TEXT PRIMARY KEY,cwd TEXT,updated_at TEXT,"
                           "closed_at TEXT NOT NULL DEFAULT '',archived_at TEXT NOT NULL DEFAULT '')")
                db.execute('CREATE TABLE harness_background_specs (child_session_id TEXT)')
                db.executemany('INSERT INTO harness_sessions VALUES (?,?,?,?,?)', [
                    ('original', '/work', '2026-11-01', '', ''),
                    ('recent', '/work', '2026-11-02', '', ''),
                    ('closed', '/work', '2026-11-03', '2026-11-03', ''),
                    ('archived', '/work', '2026-11-03', '', '2026-11-03'),
                    ('child', '/work', '2026-11-03', '', '')])
                db.execute("INSERT INTO harness_background_specs VALUES ('child')")
            self.assertEqual(release.historical_sessions(path), [('recent', '/work'), ('original', '/work')])

    def test_busy_rollback_restores_intake_without_stopping_or_writing(self):
        support = SimpleNamespace(
            inspect=Mock(return_value={'Image': 'candidate', 'State': {'Running': True}}),
            control=Mock(return_value={'active_runs': 1}),
            frontend_idle=Mock(side_effect=RuntimeError('busy')),
            management=Mock(return_value={'ok': True}), run=Mock(), atomic=Mock())
        with patch.object(release, 'util', support), patch.object(release, 'read', return_value={'image_id': 'candidate'}):
            with self.assertRaisesRegex(RuntimeError, 'busy'):
                release.rollback()
        support.management.assert_called_once_with('daemon.resume')
        support.run.assert_not_called()
        support.atomic.assert_not_called()

    def test_rollback_only_writes_program_selection(self):
        with tempfile.TemporaryDirectory() as directory:
            backup = Path(directory)
            (backup / 'env').write_bytes(b'original env')
            (backup / 'wrapper').write_bytes(b'original wrapper')
            support = SimpleNamespace(
                inspect=Mock(side_effect=[{'Image': 'candidate', 'State': {'Running': False}}, {'Image': release.BASE}]),
                ENV=Path('/deploy/.env'), WRAPPER=Path('/deploy/wrapper'),
                run=Mock(), atomic=Mock(), compose=lambda *args: list(args),
                wait_healthy=Mock(), control=Mock())
            with patch.object(release, 'util', support), patch.object(release, 'BACKUP', backup), \
                 patch.object(release, 'read', return_value={'image_id': 'candidate'}), patch.object(release, 'save'):
                release.rollback()
            self.assertEqual([call.args[0] for call in support.atomic.call_args_list], [support.ENV, support.WRAPPER])

    def test_busy_restart_keeps_container_running(self):
        support = SimpleNamespace(frontend_idle=Mock(side_effect=[None, RuntimeError('busy')]),
                                  control=Mock(), management=Mock(), run=Mock())
        with patch.object(release, 'util', support), patch.object(release, 'verify'), \
             patch.object(release, 'status', return_value={'data': {}}):
            with self.assertRaisesRegex(RuntimeError, 'busy'):
                release.restart()
        support.management.assert_called_once_with('daemon.resume')
        support.run.assert_not_called()


if __name__ == '__main__':
    unittest.main()
