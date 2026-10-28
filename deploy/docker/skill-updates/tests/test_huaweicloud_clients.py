"""Offline deployment-contract tests. No private keys or SSH connections used."""

from contextlib import redirect_stderr
import importlib.util
import io
import json
import os
from pathlib import Path
import sys
import tempfile
import types
import unittest
from unittest.mock import MagicMock, patch

ROOT = Path(__file__).resolve().parents[1]
WORKSPACE = ROOT.parents[2]


def load_linux(paramiko, environment=None):
    spec = importlib.util.spec_from_file_location(
        'deployment_huaweicloud_linux', ROOT / 'huaweicloud-linux/scripts/huaweicloud_ssh.py')
    module = importlib.util.module_from_spec(spec)
    with patch.dict(sys.modules, {'paramiko': paramiko}), patch.dict(os.environ, environment or {}):
        spec.loader.exec_module(module)
    return module


class HuaweiClientTests(unittest.TestCase):
    def setUp(self):
        self.temp_root = WORKSPACE / '.tmp/gentoo-fix/cloud-tests'
        self.temp_root.mkdir(parents=True, exist_ok=True)
        self.temporary = tempfile.TemporaryDirectory(dir=self.temp_root)
        self.addCleanup(self.temporary.cleanup)
        self.key = Path(self.temporary.name) / 'fixture key'
        self.hosts = Path(self.temporary.name) / 'fixture known_hosts'
        self.key.write_text('offline test placeholder; not a private key')
        self.hosts.write_text('offline test placeholder; no real host enrollment')
        self.client = MagicMock()
        self.paramiko = types.SimpleNamespace(
            SSHClient=MagicMock(return_value=self.client),
            RejectPolicy=type('RejectPolicy', (), {}),
            AutoAddPolicy=MagicMock(side_effect=AssertionError('automatic trust is forbidden')),
            Ed25519Key=types.SimpleNamespace(from_private_key_file=MagicMock(return_value=object())),
            RSAKey=types.SimpleNamespace(from_private_key_file=MagicMock(return_value=object())),
        )
        self.module = load_linux(self.paramiko)

    def test_global_connection_arguments_and_command_options_remain_separate(self):
        prefix = ['--key', str(self.key), '--known-hosts', str(self.hosts),
                  '--host', 'host.invalid', '--port', '2244', '--user', 'operator']
        for operation in (['status'], ['exec', 'ls', '-la', '/tmp/path with spaces'],
                          ['upload', '/local/file', '/remote/file'],
                          ['download', '/remote/file', '/local/file']):
            with self.subTest(action=operation[0]):
                args = self.module.parser().parse_args(prefix + operation)
                self.assertEqual((args.key, args.known_hosts), (self.key, self.hosts))
                self.assertEqual((args.host, args.port, args.user), ('host.invalid', 2244, 'operator'))
                if args.action == 'exec':
                    self.assertEqual(args.command, ['ls', '-la', '/tmp/path with spaces'])
                with patch.object(self.module, 'get_client', return_value=self.client) as connect:
                    self.module.client_for(args)
                    connect.assert_called_once_with(key_path=self.key, known_hosts=self.hosts,
                                                    host='host.invalid', port=2244, user='operator')

    def test_environment_compatibility_and_explicit_cli_precedence(self):
        env = {'HUAWEICLOUD_SSH_KEY': '/fixture/env-key',
               'HUAWEICLOUD_SSH_KNOWN_HOSTS': '/fixture/env-known-hosts',
               'HUAWEICLOUD_SSH_HOST': 'environment.invalid',
               'HUAWEICLOUD_SSH_PORT': '2245', 'HUAWEICLOUD_SSH_USER': 'environment-user'}
        with patch.dict(os.environ, env):
            module = load_linux(self.paramiko)
            args = module.parser().parse_args(['status'])
            self.assertEqual((args.key, args.known_hosts), (Path('/fixture/env-key'), Path('/fixture/env-known-hosts')))
            self.assertEqual((args.host, args.port, args.user), ('environment.invalid', 2245, 'environment-user'))
            args = module.parser().parse_args(['--key', str(self.key), '--known-hosts', str(self.hosts), 'status'])
            self.assertEqual((args.key, args.known_hosts), (self.key, self.hosts))

    def test_verified_known_hosts_loaded_and_ambient_auth_disabled(self):
        client = self.module.get_client(self.key, 'host.invalid', 2244, 'operator', self.hosts)
        self.assertIs(client, self.client)
        self.client.load_host_keys.assert_called_once_with(str(self.hosts))
        policy = self.client.set_missing_host_key_policy.call_args.args[0]
        self.assertIsInstance(policy, self.paramiko.RejectPolicy)
        self.paramiko.AutoAddPolicy.assert_not_called()
        self.paramiko.Ed25519Key.from_private_key_file.assert_called_once_with(str(self.key))
        kwargs = self.client.connect.call_args.kwargs
        self.assertEqual((kwargs['hostname'], kwargs['port'], kwargs['username']), ('host.invalid', 2244, 'operator'))
        self.assertFalse(kwargs['allow_agent'])
        self.assertFalse(kwargs['look_for_keys'])

    def test_missing_key_fails_before_ssh_client_or_network(self):
        with redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as caught:
            self.module.get_client(self.key.with_name('absent'), known_hosts=self.hosts)
        self.assertEqual(caught.exception.code, 2)
        self.paramiko.SSHClient.assert_not_called()

    def test_missing_known_hosts_fails_before_ssh_client_or_network(self):
        with redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as caught:
            self.module.get_client(self.key, known_hosts=self.hosts.with_name('absent'))
        self.assertEqual(caught.exception.code, 2)
        self.paramiko.SSHClient.assert_not_called()

    def test_changed_or_unknown_host_fails_closed_and_closes_client(self):
        self.client.connect.side_effect = RuntimeError('host key mismatch or unknown host')
        with redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as caught:
            self.module.get_client(self.key, known_hosts=self.hosts)
        self.assertEqual(caught.exception.code, 3)
        self.client.close.assert_called_once()
        self.paramiko.AutoAddPolicy.assert_not_called()

    def test_invalid_known_hosts_fails_without_connecting(self):
        self.client.load_host_keys.side_effect = ValueError('invalid known_hosts')
        with redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as caught:
            self.module.get_client(self.key, known_hosts=self.hosts)
        self.assertEqual(caught.exception.code, 2)
        self.client.connect.assert_not_called()
        self.client.close.assert_called_once()

    def test_help_never_loads_connection_files_or_opens_ssh(self):
        with patch('sys.stdout', new=io.StringIO()), self.assertRaises(SystemExit) as caught:
            self.module.main(['--help'])
        self.assertEqual(caught.exception.code, 0)
        self.paramiko.SSHClient.assert_not_called()

    def test_invalid_port_and_empty_exec_fail_before_network(self):
        for argv in (['--port', '0', 'status'], ['exec']):
            with self.subTest(argv=argv), redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as caught:
                self.module.main(argv)
            self.assertEqual(caught.exception.code, 2)
        self.paramiko.SSHClient.assert_not_called()

    def test_both_runtime_manifests_reference_actual_scripts_and_no_credentials(self):
        for name in ('huaweicloud-linux', 'huaweicloud-sandbox'):
            skill = ROOT / name
            manifest = json.loads((skill / 'skill-runtime.json').read_text())
            self.assertEqual(manifest['version'], 1)
            self.assertIn('paramiko', manifest['python']['packages'])
            self.assertEqual(manifest['python']['min_version'], '3.10')
            for entrypoint in manifest['python']['entrypoints']:
                self.assertTrue((skill / entrypoint).is_file())
            self.assertFalse(any(path.name in ('huaweicloud_ed25519', 'known_hosts') for path in skill.rglob('*')))

    def test_sandbox_executable_and_requirements_are_unchanged(self):
        source = WORKSPACE / 'dist/desktop/DeerFlow-Desktop-Go-20261010-103944/user-data/skills/evolved/huaweicloud-sandbox'
        for path in ('scripts/huaweicloud_sandbox.py', 'skill-runtime.json', 'requirements.txt'):
            self.assertEqual((source / path).read_bytes(), (ROOT / 'huaweicloud-sandbox' / path).read_bytes())


if __name__ == '__main__':
    unittest.main()
