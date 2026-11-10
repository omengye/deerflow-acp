import importlib.util
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch
import yaml

spec = importlib.util.spec_from_file_location('search_rollout', Path(__file__).with_name('release.py'))
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)


class PreservationTests(unittest.TestCase):
    def setUp(self):
        self.old_util = release.util
        release.util = SimpleNamespace(load_yaml=yaml.safe_load)

    def tearDown(self):
        release.util = self.old_util

    def test_migration_retains_models_proxy_other_tools_and_unknown_fields(self):
        old = {'models': [{'api_key': 'model-private'}], 'skills': {'enabled': True},
               'tools': [{'name': 'web_search', 'api_key': 'search-private', 'https_proxy': 'http://proxy',
                          'timeout': 17, 'max_results': 8, 'user_extension': 'keep'},
                         {'name': 'web_fetch', 'timeout': 11, 'https_proxy': 'http://fetch'}]}
        migrated, removed = release.migrated_document(old)
        self.assertEqual(migrated['models'], old['models'])
        self.assertEqual(migrated['tools'][1], old['tools'][1])
        self.assertEqual(migrated['tools'][0], {'name': 'web_search', 'max_results': 8, 'user_extension': 'keep'})
        self.assertEqual(set(removed), {'api_key', 'https_proxy', 'timeout'})
        self.assertEqual(old['tools'][0]['api_key'], 'search-private')

    def test_unrelated_configuration_change_is_rejected(self):
        before = b'models: [{api_key: old-key}]\ntools: [{name: web_search, api_key: search-key}]\n'
        after = b'models: [{api_key: changed-key}]\ntools: [{name: web_search}]\n'
        with self.assertRaisesRegex(RuntimeError, 'outside search migration'):
            release.verify_config(before, after)

    def test_exact_search_migration_passes(self):
        before = b'models: [{api_key: keep}]\ntools: [{name: web_search, api_key: old, timeout: 30, max_results: 5}]\n'
        after = b'models: [{api_key: keep}]\ntools: [{name: web_search, max_results: 5}]\n'
        self.assertEqual(set(release.verify_config(before, after)), {'api_key', 'timeout'})

    def test_provider_credentials_and_selection_cannot_be_overwritten(self):
        import json
        before = {'config': {'default_provider': 'searxng', 'fetch_provider': 'local',
                            'providers': {'brave': {'api_key': 'private'}, 'searxng': {'base_url': 'http://search'}}}}
        body = json.dumps(before).encode()
        release.verify_provider_store(body, body)
        changed = json.loads(body)
        changed['config']['providers']['brave']['api_key'] = 'changed'
        with self.assertRaisesRegex(RuntimeError, 'provider configuration'):
            release.verify_provider_store(body, json.dumps(changed).encode())
        changed = json.loads(body)
        changed['config']['default_provider'] = 'brave'
        with self.assertRaisesRegex(RuntimeError, 'search preference'):
            release.verify_provider_store(body, json.dumps(changed).encode())

    def test_explicitly_cleared_provider_key_stays_cleared(self):
        import json
        old = {'config': {'providers': {'brave': {'api_key': ''}}}}
        new = {'config': {'providers': {'brave': {'api_key': 'old-resurrected'}}}}
        with self.assertRaisesRegex(RuntimeError, 'provider configuration'):
            release.verify_provider_store(json.dumps(old).encode(), json.dumps(new).encode())

    def test_first_migration_checks_the_actual_persisted_legacy_key_and_proxy(self):
        import json
        config = b'tools: [{name: web_search, api_key: legacy-private, https_proxy: http://proxy, timeout: 17}]\n'
        store = {'config': {'providers': {'brave': {'api_key': 'legacy-private', 'https_proxy': 'http://proxy', 'timeout': 17}}}}
        release.verify_legacy_search_store(config, json.dumps(store).encode(), {})
        store['config']['providers']['brave']['api_key'] = 'wrong'
        with self.assertRaisesRegex(RuntimeError, 'legacy search setting'):
            release.verify_legacy_search_store(config, json.dumps(store).encode(), {})

    def test_existing_search_provider_setting_has_priority_over_legacy_value(self):
        import json
        config = b'search: {providers: {brave: {api_key: explicit-private}}}\ntools: [{name: web_search, api_key: old-private}]\n'
        store = {'config': {'providers': {'brave': {'api_key': 'explicit-private'}}}}
        release.verify_legacy_search_store(config, json.dumps(store).encode(), {})

    def rollback_fixture(self, busy):
        import json
        root = Path(tempfile.mkdtemp())
        self.addCleanup(lambda: __import__('shutil').rmtree(root))
        backup = root / 'backup'; backup.mkdir()
        original = b'models: [{api_key: keep}]\ntools: [{name: web_search, api_key: old}]\n'
        current = b'models: [{api_key: keep}]\ntools: [{name: web_search}]\n'
        (backup / 'config').write_bytes(original)
        (backup / 'env').write_bytes(b'DEERFLOW_IMAGE=previous\n')
        (backup / 'wrapper').write_bytes(b'old wrapper')
        (root / 'config').write_bytes(current)
        (root / 'env').write_bytes(b'DEERFLOW_IMAGE=candidate\n')
        (root / 'wrapper').write_bytes(b'current wrapper')
        (root / 'harness.db').write_bytes(b'new live business data')
        (backup / 'harness.db').write_bytes(b'old backup must never overwrite live data')
        (root / 'binding.json').write_bytes(b'{"data_dir":"config/go-harness-state"}')
        actions = []
        def idle(state):
            if busy:
                raise RuntimeError('busy')
        utility = SimpleNamespace(CONFIG=root / 'config', ENV=root / 'env', WRAPPER=root / 'wrapper',
            load_yaml=yaml.safe_load, inspect=lambda: {'Image': 'candidate', 'State': {'Running': True}},
            frontend_idle=idle, control=lambda op: actions.append(op) or {},
            management=lambda op: actions.append(op), maintenance_idle=lambda *a, **k: None,
            database_summary=lambda: {}, compose=lambda *args: ['compose', *args],
            run=lambda args, **kwargs: actions.append(args), wait_healthy=lambda: None,
            atomic=lambda path, data, **kwargs: path.write_bytes(data))
        previous_storage = release.storage
        release.storage = SimpleNamespace(BACKUP=backup, BASE='previous')
        self.addCleanup(setattr, release, 'storage', previous_storage)
        release.util = utility
        return root, actions

    def test_busy_rollback_resumes_requests_without_mutating_configuration(self):
        root, actions = self.rollback_fixture(True)
        old = (root / 'config').read_bytes()
        with patch.object(release, 'read', return_value={'image_id': 'candidate'}):
            with self.assertRaisesRegex(RuntimeError, 'busy'):
                release.rollback()
        self.assertEqual((root / 'config').read_bytes(), old)
        self.assertEqual(actions, ['daemon.drain', 'daemon.resume'])

    def test_rollback_replaces_program_selection_only_and_never_business_data(self):
        root, actions = self.rollback_fixture(False)
        calls = iter([{'Image': 'candidate', 'State': {'Running': True}}, {'Image': 'previous', 'State': {'Running': True}}])
        release.util.inspect = lambda: next(calls)
        with patch.object(release, 'read', return_value={'image_id': 'candidate'}), patch.object(release, 'save'):
            release.rollback()
        self.assertEqual((root / 'harness.db').read_bytes(), b'new live business data')
        self.assertEqual((root / 'binding.json').read_bytes(), b'{"data_dir":"config/go-harness-state"}')
        self.assertEqual((root / 'config').read_bytes(), (root / 'backup/config').read_bytes())
        self.assertIn(['compose', 'up', '-d', '--no-deps', '--no-build', 'deerflow'], actions)
        self.assertNotIn('restore_database', actions)


if __name__ == '__main__':
    unittest.main()
