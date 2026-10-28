"""FoloToy ASR 2.0 integration regressions; no network, dotenv or credentials."""
import importlib.util
import json
import logging
import os
from pathlib import Path
import sys
import tempfile
import types
import unittest
from unittest.mock import AsyncMock, patch


ROOT = Path(__file__).resolve().parents[1]
SRC = ROOT / 'src'


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


def configuration(values=None):
    with tempfile.TemporaryDirectory(prefix='asr2-config-test-') as directory:
        roles = Path(directory) / 'roles.json'
        roles.write_text('{}', encoding='utf-8')
        environment = {'ROLES_FILE_PATH': str(roles)}
        environment.update(values or {})
        dotenv = types.ModuleType('dotenv')
        dotenv.load_dotenv = lambda *args, **kwargs: None
        with patch.dict(os.environ, environment, clear=True), patch.dict(sys.modules, {'dotenv': dotenv}):
            config = load('_asr2_integration_config', SRC / 'config.py')
        with patch.dict(sys.modules, {'config': config}):
            util = load('_asr2_integration_config_util', SRC / 'util/config_util.py')
        return config.default_config, util


asr = load('_asr2_integration_engine', SRC / 'sdk/stt_sdk/volcengine_asr2.py')


class ConfigurationTests(unittest.TestCase):
    def test_default_is_opt_in_and_existing_provider_is_retained(self):
        config, _ = configuration()
        self.assertEqual(config.STT_TYPE, 'openai-whisper')
        self.assertEqual(config.VOLCENGINE_ASR2_API_KEY, '')
        self.assertEqual(config.VOLCENGINE_ASR2_RESOURCE_ID, 'volc.seedasr.sauc.duration')
        self.assertEqual(config.VOLCENGINE_ASR2_TIMEOUT, 45.0)

    def test_private_environment_wins_over_every_role_override(self):
        config, util = configuration({'VOLCENGINE_ASR2_API_KEY': 'fixture-service-key',
                                     'VOLCENGINE_ASR2_RESOURCE_ID': 'volc.seedasr.sauc.duration',
                                     'VOLCENGINE_ASR2_TIMEOUT': '55'})
        role = {'stt_config': {'api_key': 'fixture-role-key', 'app_id': 'untrusted-app',
                               'access_token': 'untrusted-token', 'timeout': 999,
                               'resource_id': 'volc.bigasr.sauc.duration',
                               'endpoint': 'wss://untrusted.example/',
                               'hotwords': ['wrong'], 'context': 'wrong'}}
        selected = util.get_role_stt_config('volcengine-asr2', role, config)
        self.assertEqual(selected, {'api_key': 'fixture-service-key',
                                    'resource_id': 'volc.seedasr.sauc.duration', 'timeout': 55.0})

    def test_old_provider_still_uses_its_old_credentials(self):
        config, util = configuration({'STT_TYPE': 'volcengine-asr',
                                     'VOLCENGINE_ASR_APP_ID': 'fixture-old-app',
                                     'VOLCENGINE_ASR_ACCESS_TOKEN': 'fixture-old-token',
                                     'VOLCENGINE_ASR_CLUSTER_ID': 'volcengine_input_common',
                                     'VOLCENGINE_ASR2_API_KEY': 'fixture-new-key'})
        selected = util.get_role_stt_config('volcengine-asr', {}, config)
        self.assertEqual(config.STT_TYPE, 'volcengine-asr')
        self.assertEqual(selected['app_id'], 'fixture-old-app')
        self.assertEqual(selected['access_token'], 'fixture-old-token')
        self.assertNotIn('api_key', selected)


class ProductionWrapperTests(unittest.TestCase):
    def test_success_has_auditable_provider_and_no_old_authentication(self):
        response = {'text': 'fixture-private-transcript', 'final': True,
                    'request_id': 'request-fixture', 'resource_id': asr.RESOURCE_ID,
                    'elapsed_ms': 15}
        selected = {'api_key': 'fixture-private-key', 'resource_id': asr.RESOURCE_ID,
                    'timeout': 45, 'app_id': 'ignored-app', 'access_token': 'ignored-token'}
        mock = AsyncMock(return_value=response)
        with patch.object(asr, 'transcribe', mock), self.assertLogs(asr.LOGGER, level='INFO') as logs:
            result = asr.VolcengineAsr2Engine().sound_to_text('virtual.wav', selected)
        mock.assert_awaited_once_with('virtual.wav', api_key='fixture-private-key',
                                     timeout=45, resource_id=asr.RESOURCE_ID)
        self.assertEqual(result['provider'], 'volcengine-asr2')
        self.assertEqual(result['text'], 'fixture-private-transcript')
        self.assertTrue(result['final'])
        text = '\n'.join(logs.output)
        self.assertIn('request-fixture', text)
        self.assertNotIn('fixture-private-key', text)
        self.assertNotIn('fixture-private-transcript', text)
        self.assertNotIn('ignored-token', text)

    def test_auth_failure_returns_none_and_does_not_fallback(self):
        mock = AsyncMock(side_effect=asr.Asr2Error('asr2_handshake_rejected', status=403))
        with patch.object(asr, 'transcribe', mock), self.assertLogs(asr.LOGGER, level='WARNING') as logs:
            result = asr.VolcengineAsr2Engine().sound_to_text('virtual.wav', {'api_key': 'fixture-secret'})
        self.assertIsNone(result)
        self.assertEqual(mock.await_count, 1)
        text = '\n'.join(logs.output)
        self.assertIn('asr2_handshake_rejected', text)
        self.assertIn('403', text)
        self.assertNotIn('fixture-secret', text)

    def test_unexpected_exception_message_is_never_logged(self):
        mock = AsyncMock(side_effect=RuntimeError('fixture-private-key-in-provider-exception'))
        with patch.object(asr, 'transcribe', mock), self.assertLogs(asr.LOGGER, level='WARNING') as logs:
            result = asr.VolcengineAsr2Engine().sound_to_text('virtual.wav', {'api_key': 'fixture-secret'})
        self.assertIsNone(result)
        self.assertIn('RuntimeError', '\n'.join(logs.output))
        self.assertNotIn('fixture-private', '\n'.join(logs.output))

    def test_missing_new_key_cannot_use_old_config_credentials(self):
        old_config = types.SimpleNamespace(VOLCENGINE_ASR_APP_ID='old-app',
                                          VOLCENGINE_ASR_ACCESS_TOKEN='old-token')
        with self.assertLogs(asr.LOGGER, level='WARNING') as logs:
            result = asr.VolcengineAsr2Engine(old_config).sound_to_text('virtual.wav', {})
        self.assertIsNone(result)
        self.assertIn('asr2_missing_credentials', '\n'.join(logs.output))


class DispatchTests(unittest.TestCase):
    def make_sdk(self):
        package = types.ModuleType('sdk')
        package.__path__ = []
        subpackage = types.ModuleType('sdk.stt_sdk')
        subpackage.__path__ = []
        package.stt_sdk = subpackage
        config = types.ModuleType('config')
        config.Config = object
        modules = {'sdk': package, 'sdk.stt_sdk': subpackage, 'config': config}
        engines = {'aliyun_asr': 'AliyunAsrEngine', 'azure_stt': 'AzureSttEngine',
                   'azure_whisper': 'AzureWhisperEngine', 'cf_workers_ai_asr': 'CfWorkersAiAsrEngine',
                   'dify_stt': 'DifySttEngine', 'groq_stt': 'GroqSttEngine',
                   'openai_whisper': 'OpenaiWhisperEngine', 'volcengine_asr': 'VolcengineAsrEngine',
                   'volcengine_asr2': 'VolcengineAsr2Engine'}
        for name, class_name in engines.items():
            module = types.ModuleType('sdk.stt_sdk.' + name)
            def init(self, config, engine_name=name): self.engine_name = engine_name
            def sound(self, audio, options): return {'engine': self.engine_name, 'input': audio, 'options': options}
            setattr(module, class_name, type(class_name, (), {'__init__': init, 'sound_to_text': sound}))
            setattr(subpackage, name, module)
            modules[module.__name__] = module
        with patch.dict(sys.modules, modules):
            return load('sdk._asr2_dispatch_test', SRC / 'sdk/stt.py').SttSdk(object())

    def test_asr2_dispatch_receives_path_instead_of_open_file(self):
        sdk = self.make_sdk()
        result = sdk.transcribe('does-not-exist.wav', 'volcengine-asr2', {'api_key': 'fixture-key'})
        self.assertEqual(result['engine'], 'volcengine_asr2')
        self.assertEqual(result['input'], 'does-not-exist.wav')
        self.assertEqual(result['options'], {'api_key': 'fixture-key'})

    def test_old_asr_dispatch_remains_available_for_rollback(self):
        sdk = self.make_sdk()
        result = sdk.transcribe('does-not-exist.wav', 'volcengine-asr', {'app_id': 'fixture-app'})
        self.assertEqual(result['engine'], 'volcengine_asr')
        self.assertEqual(result['input'], 'does-not-exist.wav')


if __name__ == '__main__':
    unittest.main()
