import importlib.util
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('search_cleanup', Path(__file__).with_name('cleanup.py'))
cleanup = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cleanup)

class CleanupGuards(unittest.TestCase):
    def test_live_data_and_backup_are_not_cleanup_targets(self):
        with tempfile.TemporaryDirectory() as tmp:
            task = Path(tmp) / 'release'; task.mkdir()
            for name in ('backup', 'config.yaml', 'source.tar.gz', 'runtime-data.json'):
                with self.assertRaisesRegex(RuntimeError, 'not a temporary'):
                    cleanup.target_for_cleanup(task, task / name)

    def test_parent_escape_is_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            task = Path(tmp) / 'release'; task.mkdir()
            with self.assertRaisesRegex(RuntimeError, 'escaped'):
                cleanup.target_for_cleanup(task, task.parent / 'build')

    def test_only_this_release_build_and_profiles_are_allowed(self):
        with tempfile.TemporaryDirectory() as tmp:
            task = Path(tmp)
            for name in ('build', 'image-abc', 'stage-abc'):
                path = task / name; path.mkdir()
                self.assertEqual(cleanup.target_for_cleanup(task, path), path)
            self.assertEqual(cleanup.target_for_cleanup(task, task / 'go-artifacts.tar.gz'), task / 'go-artifacts.tar.gz')

if __name__ == '__main__':
    unittest.main()
