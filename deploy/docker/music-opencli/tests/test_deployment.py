"""Offline invariants for the coordinated Gentoo deployment (no Docker calls)."""
import contextlib
import copy
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
REPO = ROOT.parents[2]
LIBS = REPO / '.tmp/music-opencli/deploy-test-libs'
sys.path.insert(0, str(LIBS))
import yaml


def module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    loaded = importlib.util.module_from_spec(spec)
    sys.modules[name] = loaded
    spec.loader.exec_module(loaded)
    return loaded


deploy = module('music_deploy', ROOT / 'deploy.py')
package = module('music_package', ROOT / 'package.py')


class DeploymentTests(unittest.TestCase):
    def setUp(self):
        scratch = REPO / '.tmp/music-opencli/deployment-tests'
        scratch.mkdir(parents=True, exist_ok=True)
        self.temp = tempfile.TemporaryDirectory(dir=scratch)
        self.folder = Path(self.temp.name)

    def tearDown(self):
        self.temp.cleanup()

    def fixtures(self):
        deer = {'models': [{'name': 'configured', 'api_key': 'FAKE_MODEL_SECRET'}],
                'default_model': 'configured', 'local_acp': {'enable_bash': False},
                'sandbox': {'allow_host_tools': False, 'allow_host_bash': False},
                'tools': [{'name': 'web_search', 'api_key': 'FAKE_SEARCH_SECRET'},
                          {'name': 'host_opencli', 'executable': 'opencli', 'timeout': 45}]}
        deer_compose = {'services': {'deerflow': {'image': '${DEERFLOW_IMAGE:-old}',
                        'environment': {'WAKU_DAEMON_TOKEN': '${WAKU_DAEMON_TOKEN:?required}'},
                        'volumes': ['./data:/var/lib/deerflow', './runtime/wrapper:/opt/deerflow/bin/deerflow-acpd:ro'],
                        'user': '1000:1000', 'ports': ['192.168.1.190:18787:18787']}}}
        folo_compose = {'services': {'folotoy': {'image': '${FOLOTOY_WS_IMAGE:-old}', 'env_file': ['.env.ws'],
                        'network_mode': 'host', 'environment': {'ROLES_FILE_PATH': '/config/roles.json',
                        'AUDIO_SAVE_PATH': '/audio', 'DEERFLOW_STATE_PATH': '/data/sessions.sqlite3'},
                        'volumes': ['./audio:/audio', './data:/data', './roles-ws.json:/config/roles.json:ro']}}}
        return deer, deer_compose, folo_compose

    def transform(self, deer, deer_compose, folo_compose):
        request = {'deer_config': yaml.safe_dump(deer), 'deer_compose': yaml.safe_dump(deer_compose),
                   'folo_compose': yaml.safe_dump(folo_compose), 'sites': ['web', 'github'],
                   'music_mount': '/home/gen/deerflow-workspaces/folotoy/music:/music:ro'}
        environment = dict(os.environ, PYTHONPATH=os.pathsep.join(sys.path))
        return subprocess.run([sys.executable, '-c', deploy.YAML_TRANSFORM], input=json.dumps(request),
                              capture_output=True, text=True, env=environment)

    def test_native_yaml_changes_only_authorized_fields(self):
        deer, deer_compose, folo = self.fixtures()
        result = self.transform(deer, deer_compose, folo)
        self.assertEqual(result.returncode, 0, result.stderr)
        result = json.loads(result.stdout)
        expected = copy.deepcopy(deer)
        expected['sandbox']['allow_host_tools'] = True
        expected['tools'][1].update(executable='/opt/deerflow-host-tools/opencli-proxy',
                                     timeout=100, allowed_sites=['web', 'github'])
        self.assertEqual(yaml.safe_load(result['deer_config']), expected)
        self.assertEqual(yaml.safe_load(result['deer_compose'])['services']['deerflow']['volumes'],
                         deer_compose['services']['deerflow']['volumes'])

    def test_folotoy_yaml_preserves_audio_roles_and_mode_inputs(self):
        deer, deer_compose, original = self.fixtures()
        result = json.loads(self.transform(deer, deer_compose, original).stdout)
        after = yaml.safe_load(result['folo_compose'])['services']['folotoy']
        before = original['services']['folotoy']
        self.assertEqual(after['env_file'], before['env_file'])
        self.assertEqual(after['network_mode'], 'host')
        self.assertTrue(all(v in after['volumes'] for v in before['volumes']))
        self.assertEqual(after['environment']['DEERFLOW_TURN_TIMEOUT'], '240')
        self.assertEqual(after['environment']['MUSIC_CONTROL_CONFIG'], '/run/deerflow-music-control.json')
        for key, value in before['environment'].items():
            self.assertEqual(after['environment'][key], value)

    def test_conflicting_music_mount_is_rejected(self):
        deer, deer_compose, folo = self.fixtures()
        folo['services']['folotoy']['volumes'].append('/different:/music:rw')
        result = self.transform(deer, deer_compose, folo)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(result.stdout, '')

    def test_image_env_edit_preserves_other_original_bytes(self):
        raw = b'WAKU_DAEMON_TOKEN=FAKE_PRIVATE\r\nDEERFLOW_IMAGE="old"\r\n# comment\r\nFOO=a=b\r\n'
        updated = deploy.update_image_env(raw, 'DEERFLOW_IMAGE', deploy.DEER_IMAGE)
        self.assertEqual(updated, raw.replace(b'DEERFLOW_IMAGE="old"', b'DEERFLOW_IMAGE=' + deploy.DEER_IMAGE.encode()))
        self.assertEqual(deploy.update_image_env(raw, 'FOLOTOY_WS_IMAGE', deploy.FOLO_IMAGE), raw)
        with self.assertRaises(deploy.DeploymentError):
            deploy.update_image_env(b'DEERFLOW_IMAGE=one\nDEERFLOW_IMAGE=two\n', 'DEERFLOW_IMAGE', 'new')

    def test_file_change_restores_original_and_removes_only_its_new_file(self):
        path = self.folder / 'config'
        path.write_bytes(b'old')
        change = deploy.Change.prepare(path, b'new')
        change.apply()
        self.assertEqual(path.read_bytes(), b'new')
        self.assertTrue(change.restore())
        self.assertEqual(path.read_bytes(), b'old')
        added = deploy.Change.prepare(self.folder / 'added', b'new')
        added.apply()
        self.assertTrue(added.restore())
        self.assertFalse(added.path.exists())

    def test_concurrent_file_changes_are_preserved(self):
        path = self.folder / 'config'
        path.write_bytes(b'old')
        change = deploy.Change.prepare(path, b'new')
        path.write_bytes(b'external')
        with self.assertRaises(deploy.DeploymentError):
            change.apply()
        self.assertEqual(path.read_bytes(), b'external')
        path.write_bytes(b'old')
        change = deploy.Change.prepare(path, b'new')
        change.apply()
        path.write_bytes(b'external-after')
        self.assertFalse(change.restore())
        self.assertEqual(path.read_bytes(), b'external-after')

    def test_payload_excludes_private_files_tests_and_unverified_sources(self):
        # A small dummy wheel exercises packaging only; real offline wheels are
        # validated by pip during the no-network Docker image build.
        wheels = self.folder / 'wheels'
        wheels.mkdir()
        (wheels / 'requests-2.32.5-py3-none-any.whl').write_bytes(b'packaging-fixture')
        output = self.folder / 'payload.tar.gz'
        with patch.object(sys, 'argv', ['package.py', '--wheels', str(wheels), '--output', str(output)]):
            with contextlib.redirect_stdout(io.StringIO()):
                package.main()
        with tarfile.open(output, 'r:gz') as archive:
            names = archive.getnames()
            self.assertTrue(any(n.startswith('folotoy-source/src/') for n in names))
            self.assertFalse(any('/verify/' in n or '/tests/' in n or '__pycache__' in n or '/private/' in n for n in names))
            self.assertFalse(any(Path(n).name.startswith('.env') for n in names))
            manifest = json.load(archive.extractfile('manifest.json'))
            self.assertFalse(manifest['credentials_included'])
            for name, expected in manifest['files'].items():
                self.assertEqual(deploy.digest(archive.extractfile(name).read()), expected)


if __name__ == '__main__':
    unittest.main()
