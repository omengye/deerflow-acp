"""Local, offline safety checks for release preparation and recovery."""
import copy
from contextlib import closing
import importlib.util
import json
from pathlib import Path
import sqlite3
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import yaml

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location('learning_release', HERE / 'release.py')
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)


def fixture():
    default = HERE.parents[2] / 'desktop-app/resources/deerflow/default-config.yaml'
    value = yaml.safe_load(default.read_text(encoding='utf-8'))
    evolution = value['skill_evolution']
    evolution['enabled'], evolution['mode'] = False, 'review'
    evolution['behavior']['enabled'] = False
    evolution['behavior']['suite_path'] = ''
    evolution['generation']['enabled'] = False
    for group in ('validation', 'generation'):
        for key in ('timeout_seconds', 'max_input_bytes', 'max_output_tokens'):
            evolution[group][key] = 0
    for key in ('enabled', 'consolidation_enabled', 'matching_enabled'):
        evolution['learning'][key] = False
    evolution['learning']['max_daily_model_calls'] = 10
    evolution['learning']['max_daily_tokens'] = 50000
    evolution['budget'] = {'max_daily_model_calls': 30, 'max_daily_tokens': 100000,
                           'max_job_seconds': 300, 'workers': 1}
    evolution['auto_patch']['experience_patterns_enabled'] = False
    for key in evolution['discovery']:
        if key.endswith('_enabled'):
            evolution['discovery'][key] = False
    value['skills'] = {'enabled': True, 'path': '../skills', 'extensions_file': './extensions_config.json'}
    # Surround the two edited sections with content whose literal bytes must survive.
    before = b'# Provider formatting is private and must survive\nmodels: [{name: existing, api_key: "PRIVATE_SENTINEL"}]\n'
    middle = b'\n# existing memory\nmemory: {enabled: true, single_user: true, embedding_url: "http://memory-embedding:8080"}\n'
    after = b'# existing web proxy\ntools: [{name: web_search, proxy: "http://172.17.0.1:11055"}]\n'
    raw = before + yaml.safe_dump({'skills': value['skills']}).encode() + middle
    raw += yaml.safe_dump({'skill_evolution': evolution}, sort_keys=False).encode() + after
    return raw, before, middle, after


class ConfigurationTests(unittest.TestCase):
    def test_enables_pipeline_preserving_budget_and_other_configuration(self):
        raw, before, middle, after = fixture()
        output, changes = release.candidate_config(raw)
        old, new = release.load_yaml(raw), release.load_yaml(output)
        self.assertTrue(output.startswith(before))
        self.assertIn(middle, output)
        self.assertTrue(output.endswith(after))
        for key in ('models', 'memory', 'tools'):
            self.assertEqual(old[key], new[key])
        evolution = new['skill_evolution']
        self.assertEqual(evolution['budget'], old['skill_evolution']['budget'])
        self.assertEqual(evolution['learning']['max_daily_model_calls'], 10)
        self.assertEqual(evolution['learning']['max_daily_tokens'], 50000)
        self.assertFalse(evolution['learning']['cross_session_enabled'])
        self.assertTrue(evolution['discovery']['experience_new_skills_enabled'])
        self.assertTrue(evolution['auto_patch']['experience_patterns_enabled'])
        self.assertEqual(evolution['validation']['max_output_tokens'], 1024)
        self.assertEqual(evolution['generation']['max_output_tokens'], 4096)
        self.assertNotIn('/skill_evolution/budget', changes)

    def test_existing_positive_limits_are_not_overwritten(self):
        raw, *_ = fixture()
        doc = release.load_yaml(raw)
        doc['skill_evolution']['generation']['timeout_seconds'] = 120
        output, _ = release.candidate_config(yaml.safe_dump(doc).encode())
        self.assertEqual(release.load_yaml(output)['skill_evolution']['generation']['timeout_seconds'], 120)

    def test_duplicate_keys_and_conflicting_sources_rejected(self):
        raw, *_ = fixture()
        with self.assertRaisesRegex(RuntimeError, 'duplicate_yaml_key'):
            release.candidate_config(raw + b'\nmodels: []\n')
        document = release.load_yaml(raw)
        document['skills']['managed_sources'] = [{'id': 'managed-evolution', 'root': '/unrelated', 'scope': 'global'}]
        with self.assertRaisesRegex(RuntimeError, 'managed_source_conflict'):
            release.candidate_config(yaml.safe_dump(document).encode())

    def test_synthetic_stage_configuration_excludes_private_fields(self):
        raw, *_ = fixture()
        raw = raw + b'wechat: {enabled: true, token: CHANNEL_SECRET}\n'
        output, _ = release.candidate_config(raw)
        doc = release.load_yaml(output)
        doc['skill_evolution']['discovery']['legacy_token'] = 'LEGACY_DISCOVERY_SECRET'
        output = yaml.safe_dump(doc).encode()
        stage = release.synthetic_config(output)
        for private in (b'PRIVATE_SENTINEL', b'CHANNEL_SECRET', b'LEGACY_DISCOVERY_SECRET',
                        b'172.17.0.1', b'memory-embedding'):
            self.assertNotIn(private, stage)
        document = release.load_yaml(stage)
        self.assertNotIn('wechat', document)
        self.assertEqual(document['models'][0]['base_url'], 'http://127.0.0.1:9/v1')
        self.assertFalse(document['memory']['enabled'])
        self.assertEqual(document['tools'], [])

    def test_image_env_changes_only_single_image_setting(self):
        old = b'# comment\r\nWAKU_DAEMON_TOKEN=PRIVATE\r\nDEERFLOW_IMAGE=old\r\nPROXY=preserve\r\n'
        expected = old.replace(b'DEERFLOW_IMAGE=old', b'DEERFLOW_IMAGE=sha256:new')
        self.assertEqual(release.image_env(old, 'sha256:new'), expected)
        with self.assertRaisesRegex(RuntimeError, 'image_setting_ambiguous'):
            release.image_env(old + b'DEERFLOW_IMAGE=duplicate\n', 'new')


class SafetyTests(unittest.TestCase):
    def test_stage_copies_wrapper_and_mounts_only_isolated_copy(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / 'production-wrapper.sh'
            source.write_bytes(b'#!/bin/sh\nexec runtime --data-dir fixed "$@"\n')
            raw, *_ = fixture()
            candidate, _ = release.candidate_config(raw)
            data, wrapper = release.prepare_stage_profile(root / 'isolated', candidate, source.read_bytes())
            self.assertNotEqual(wrapper, source)
            self.assertEqual(wrapper.read_bytes(), source.read_bytes())
            args = release.stage_mount_arguments(data, wrapper)
            self.assertNotIn(str(source), ' '.join(args))
            self.assertNotIn(str(release.WRAPPER), ' '.join(args))
            self.assertEqual(args[-1], str(wrapper) + ':/opt/deerflow/bin/deerflow-acpd:ro')
            self.assertTrue((data / 'user-data/config/config.yaml').is_file())
            self.assertNotIn('PRIVATE_SENTINEL', (data / 'user-data/config/config.yaml').read_text())

    def test_stopped_candidate_recovery_needs_no_management(self):
        with patch.object(release, 'inspect', return_value={'State': {'Running': False}}), \
             patch.object(release, 'control') as control:
            self.assertFalse(release.prepare_running_recovery())
            control.assert_not_called()

    def test_live_recovery_defers_active_work_but_allows_durable_queue(self):
        summary = {'maintenance_jobs': {'queued': 4, 'retry': 1}, 'maintenance_calls': {}}
        state = {'draining': True, 'active_operations': 0, 'active_runs': 0, 'queued_runs': 0}
        with patch.object(release, 'inspect', return_value={'State': {'Running': True}}), \
             patch.object(release, 'control', return_value=state), \
             patch.object(release, 'database_summary', return_value=summary):
            self.assertTrue(release.prepare_running_recovery())
            state['active_runs'] = 1
            with self.assertRaisesRegex(RuntimeError, 'frontend_work_wait_required'):
                release.prepare_running_recovery()

    def test_terminal_reconciliation_does_not_block_idle(self):
        summary = {'runs': {'completed': 127, 'cancelled': 1, 'needs_reconciliation': 7},
                   'maintenance_jobs': {}, 'maintenance_calls': {}}
        release.frontend_idle({'active_operations': 0, 'active_runs': 0, 'queued_runs': 0})
        release.maintenance_idle(summary)
        for field, state in [('maintenance_jobs', 'running'), ('maintenance_jobs', 'queued'),
                             ('maintenance_jobs', 'retry'), ('maintenance_calls', 'reserved')]:
            changed = copy.deepcopy(summary)
            changed[field][state] = 1
            with self.assertRaisesRegex(RuntimeError, 'maintenance_work_wait_required'):
                release.maintenance_idle(changed)

    def test_database_summary_requires_correct_schema_and_preserves_contents(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'harness.db'
            with closing(sqlite3.connect(path)) as db:
                for table, column in [('harness_sessions', 'id'), ('harness_runs', 'status'),
                                      ('df_skill_maintenance_jobs', 'state'),
                                      ('df_skill_maintenance_calls', 'state'), ('harness_learning_outbox', 'state')]:
                    db.execute('CREATE TABLE ' + table + '(' + column + ' TEXT)')
                db.execute("INSERT INTO harness_sessions VALUES('private-id')")
                db.execute("INSERT INTO harness_runs VALUES('needs_reconciliation')")
                db.commit()
            prior = path.read_bytes()
            self.assertEqual(release.database_summary(path)['runs'], {'needs_reconciliation': 1})
            self.assertEqual(path.read_bytes(), prior)
        self.assertEqual(release.DB.relative_to(release.DATA).as_posix(),
                         'user-data/config/go-harness-state/harness.db')

    def test_management_not_found_envelope_survives_cli_exit_one(self):
        response = {'ok': False, 'code': 'not_found', 'error': 'private excluded'}
        process = subprocess.CompletedProcess(['docker'], 1, json.dumps(response), 'private stderr')
        with patch.object(release.subprocess, 'run', return_value=process):
            self.assertEqual(release.management('skill.learning.diagnostics')['code'], 'not_found')
            with self.assertRaisesRegex(RuntimeError, 'management_failed'):
                release.control('skill.learning.diagnostics')

    def test_release_gate_requires_same_tested_image_and_binaries(self):
        manifest = {'candidate_sha256': 'config', 'binaries': {'deerflow-acpd.real': 'hash'}}
        staged = {'passed': True, 'image_id': 'new-image', **manifest}
        tests = {'passed': True, 'image_id': 'old-image', 'binaries': manifest['binaries']}
        def read(path):
            return json.dumps(staged if path.name == 'stage.json' else tests).encode()
        with patch.object(release, 'read_manifest', return_value=manifest), \
             patch.object(release, 'image_id', return_value='new-image'), \
             patch.object(release, 'regular', side_effect=read):
            with self.assertRaisesRegex(RuntimeError, 'linux_test_image_identity_required'):
                release.gates()
            tests['image_id'] = 'new-image'
            self.assertEqual(release.gates(), (manifest, 'new-image'))
            tests['binaries'] = {'deerflow-acpd.real': 'wrong'}
            with self.assertRaisesRegex(RuntimeError, 'linux_test_binary_identity_required'):
                release.gates()

    def test_rollback_only_restores_configuration_and_rejects_concurrent_edits(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            backup = root / 'backup'
            backup.mkdir()
            files = {name: root / name for name in ('config', 'env', 'compose', 'wrapper')}
            original_config = b'old config'
            original_env = b'DEERFLOW_IMAGE=old\nPRIVATE=preserve\n'
            candidate_config = b'new config'
            files['config'].write_bytes(candidate_config)
            files['env'].write_bytes(release.image_env(original_env, 'new'))
            files['compose'].write_bytes(b'compose')
            files['wrapper'].write_bytes(b'wrapper')
            (backup / 'config.yaml').write_bytes(original_config)
            (backup / 'env').write_bytes(original_env)
            database = root / 'business.db'
            database.write_bytes(b'new business writes must survive')
            manifest = {'config_sha256': release.sha(original_config),
                        'candidate_sha256': release.sha(candidate_config),
                        'env_sha256': release.sha(original_env),
                        'compose_sha256': release.sha(b'compose'), 'wrapper_sha256': release.sha(b'wrapper')}
            with patch.multiple(release, BACKUP=backup, CONFIG=files['config'], ENV=files['env'],
                                COMPOSE=files['compose'], WRAPPER=files['wrapper'], DB=database):
                release.restore_configuration(manifest, 'new')
                self.assertEqual(files['config'].read_bytes(), original_config)
                self.assertEqual(files['env'].read_bytes(), original_env)
                self.assertEqual(database.read_bytes(), b'new business writes must survive')
                files['config'].write_bytes(b'concurrent user edit')
                with self.assertRaisesRegex(RuntimeError, 'config_changed_during_rollout'):
                    release.restore_configuration(manifest, 'new')
                self.assertEqual(files['config'].read_bytes(), b'concurrent user edit')


if __name__ == '__main__':
    unittest.main()
