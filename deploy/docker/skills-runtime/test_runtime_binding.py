import importlib.util
import json
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('runtime_override', Path(__file__).with_name('install-runtime-override.py'))
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)


class RuntimeBindingTests(unittest.TestCase):
    def test_maintenance_requires_existing_selection_and_preserves_both_stores(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            data = root / 'data/user-data'
            for directory in ('config/go-harness-state', 'data/go-harness'):
                db = data / directory / 'harness.db'
                db.parent.mkdir(parents=True)
                db.write_bytes(directory.encode())
            binding = data / 'config/runtime-data.json'
            with self.assertRaises(FileNotFoundError):
                release.bound_data_directory(root)
            for raw in ('{}', '{"data_dir":"../escape"}', '{"data_dir":"missing"}',
                        '{"data_dir":"config/go-harness-state","data_dir":"data/go-harness"}'):
                binding.write_text(raw)
                with self.assertRaises((ValueError, FileNotFoundError)):
                    release.bound_data_directory(root)
            for directory in ('config/go-harness-state', 'data/go-harness'):
                binding.write_text(json.dumps({'data_dir': directory}))
                self.assertEqual(release.bound_data_directory(root), '/var/lib/deerflow/user-data/' + directory)
                self.assertEqual((data / directory / 'harness.db').read_bytes(), directory.encode())

    def test_incompatible_image_and_wrong_live_store_are_rejected(self):
        selected = '/var/lib/deerflow/user-data/config/go-harness-state'
        checked = subprocess.CompletedProcess([], 0, json.dumps({'data_dir': selected}))
        with patch.object(release.subprocess, 'run', return_value=subprocess.CompletedProcess([], 2, '')):
            with self.assertRaisesRegex(RuntimeError, 'does not support'):
                release.verify_native_binding('fixture', selected)
        mismatch = subprocess.CompletedProcess([], 0, json.dumps({'ok': True, 'runtime': {'data_dir': 'other'}}))
        with patch.object(release.subprocess, 'run', side_effect=[checked, mismatch]):
            with self.assertRaisesRegex(RuntimeError, 'another database'):
                release.verify_native_binding('fixture', selected)
        live = subprocess.CompletedProcess([], 0, json.dumps({'ok': True, 'runtime': {'data_dir': selected}}))
        with patch.object(release.subprocess, 'run', side_effect=[checked, live]):
            release.verify_native_binding('fixture', selected)

    @unittest.skipUnless(sys.platform == 'linux' and os.environ.get('DEERFLOW_TEST_GO_DAEMON'), 'requires a built Linux daemon')
    def test_actual_wrapper_native_preflight_never_opens_another_store(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            native = root / 'deerflow-acpd.real'
            shutil.copyfile(os.environ['DEERFLOW_TEST_GO_DAEMON'], native)
            native.chmod(0o755)
            template = (Path(__file__).parent / 'runtime/deerflow-acpd-wrapper.sh').read_text()
            wrapper = root / 'wrapper.sh'
            wrapper.write_text(template.replace('/opt/deerflow/bin/deerflow-acpd.real', shlex.quote(str(native))))
            data = root / 'user-data'
            original = data / 'config/go-harness-state'
            other = data / 'data/go-harness'
            for directory in (original, other):
                directory.mkdir(parents=True)
                (directory / 'harness.db').write_bytes(directory.name.encode())
            (data / 'config/config.yaml').write_text('fixture: read-only preflight does not load model settings\n')
            binding = data / 'config/runtime-data.json'
            binding.write_text('{"data_dir":"config/go-harness-state"}')
            env = {key: value for key, value in os.environ.items() if not key.startswith(('DEERFLOW_', 'DEER_FLOW_'))}
            env['DEER_FLOW_PORTABLE_ROOT'] = str(root)
            run = lambda extra: subprocess.run(['/bin/sh', str(wrapper), '--check-paths'], env={**env, **extra}, text=True, capture_output=True, timeout=15)
            result = run({})
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(json.loads(result.stdout)['data_dir'], str(original))
            self.assertNotEqual(run({'DEERFLOW_PERSISTENT_DATA_DIR': str(other)}).returncode, 0)
            self.assertNotEqual(run({'DEERFLOW_GO_DATA_DIR': str(other)}).returncode, 0)
            binding.unlink()
            self.assertNotEqual(run({}).returncode, 0)
            for directory in (original, other):
                self.assertEqual(list(directory.iterdir()), [directory / 'harness.db'])
                self.assertEqual((directory / 'harness.db').read_bytes(), directory.name.encode())
            # An older image must fail before it reaches its real startup path.
            marker = root / 'legacy-started'
            legacy = root / 'legacy-daemon'
            legacy.write_text('#!/bin/sh\nfor arg; do [ "$arg" != --check-paths ] || exit 2; done\ntouch ' + shlex.quote(str(marker)) + '\n')
            legacy.chmod(0o755)
            wrapper.write_text(template.replace('/opt/deerflow/bin/deerflow-acpd.real', shlex.quote(str(legacy))))
            result = subprocess.run(['/bin/sh', str(wrapper)], env=env, capture_output=True, timeout=15)
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse(marker.exists())


if __name__ == '__main__':
    unittest.main()
