"""Offline checks for byte-preserving edits and rollback ordering."""

import importlib.util
from pathlib import Path
import json
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('mobile_rollout', Path(__file__).with_name('rollout.py'))
rollout = importlib.util.module_from_spec(spec)
spec.loader.exec_module(rollout)


class ConfigEditsTest(unittest.TestCase):
    def test_env_change_preserves_crlf_quotes_comments_and_other_values(self):
        raw = b'# preserved\r\nWAKU_DAEMON_TOKEN=PRIVATE_SENTINEL\r\nexport DEERFLOW_IMAGE="old:tag" # keep\r\nOTHER=a=b\r\n'
        changed = rollout.update_image_env(raw, 'new:tag')
        self.assertEqual(raw.replace(b'"old:tag"', b'"new:tag"'), changed)

    def test_missing_or_duplicate_image_assignment_is_rejected(self):
        for raw in (b'OTHER=1\n', b'DEERFLOW_IMAGE=one\nDEERFLOW_IMAGE=two\n'):
            with self.assertRaises(rollout.RolloutError):
                rollout.update_image_env(raw, 'new:tag')

    def test_ports_append_keeps_original_bytes_and_other_services(self):
        raw = b'services:\r\n  deerflow:\r\n    image: old\r\n    ports:\r\n      - "${DEERFLOW_BIND_ADDRESS:-127.0.0.1}:18787:18787"\r\n    volumes:\r\n      - ./data:/data\r\n  other:\r\n    ports:\r\n      - "80:80"\r\n'
        addition = b'      - "100.80.205.120:18787:18787"\r\n'
        changed = rollout.add_tail_port(raw)
        self.assertEqual(raw.replace(b'    volumes:\r\n', addition + b'    volumes:\r\n'), changed)

    def test_indentless_port_list_is_supported(self):
        raw = b'services:\n  deerflow:\n    ports:\n    - "192.168.1.190:18787:18787"\n    image: old\n'
        changed = rollout.add_tail_port(raw)
        self.assertIn(b'    - "100.80.205.120:18787:18787"\n    image: old', changed)

    def test_existing_tailscale_or_inline_ports_are_rejected(self):
        for raw in (b'services:\n  deerflow:\n    ports:\n      - "100.80.205.120:18787:18787"\n',
                    b'services:\n  deerflow:\n    ports: ["192.168.1.190:18787:18787"]\n'):
            with self.assertRaises(rollout.RolloutError):
                rollout.add_tail_port(raw)

    def test_folotoy_image_replacement_is_scoped_and_preserves_comments(self):
        raw = b'services:\n  folotoy:\n    image: "old:tag" # retain\n    environment:\n      TOKEN: PRIVATE_SENTINEL\n  another:\n    image: old:tag\n'
        changed = rollout.update_literal_image(raw, 'old:tag', 'new:tag')
        self.assertEqual(raw.replace(b'"old:tag"', b'"new:tag"'), changed)

    def test_unknown_folotoy_image_cannot_be_overwritten(self):
        with self.assertRaises(rollout.RolloutError):
            rollout.update_literal_image(b'services:\n  folotoy:\n    image: changed:tag\n', 'old:tag', 'new:tag')

    def test_rollback_stops_both_then_restores_and_starts_deerflow_first(self):
        calls = []
        preflight = {rollout.DEER: {'imageId': 'old-deer'}, rollout.FOLO: {'imageId': 'old-folo'}}
        with patch.object(rollout, 'stop', side_effect=lambda name: calls.append(('stop', name))), \
             patch.object(rollout, 'restore_configs', side_effect=lambda *args: calls.append(('restore',))), \
             patch.object(rollout, 'up', side_effect=lambda name: calls.append(('up', name))), \
             patch.object(rollout, 'verify_health', side_effect=lambda name, image: calls.append(('health', name, image))), \
             patch.object(rollout, 'probe_waku', side_effect=lambda version: calls.append(('probe', version))):
            rollout.rollback(preflight, {})
        self.assertEqual(calls, [('stop', 'folotoy'), ('stop', 'deerflow'), ('restore',),
            ('up', 'deerflow'), ('health', rollout.DEER, 'old-deer'), ('probe', 9),
            ('up', 'folotoy'), ('health', rollout.FOLO, 'old-folo')])

    def test_native_status_requires_an_explicit_nonnegative_integer(self):
        invalid = [None, {}, {'ok': False}, {'ok': True, 'data': {}},
                   {'ok': True, 'data': {'active_operations': True}},
                   {'ok': True, 'data': {'active_operations': '0'}},
                   {'ok': True, 'data': {'active_operations': -1}}]
        for value in invalid:
            with self.subTest(value=value), patch.object(rollout, 'command', return_value=json.dumps(value)):
                with self.assertRaises(rollout.RolloutError):
                    rollout.native_active_operations()
        with patch.object(rollout, 'command', return_value='{"ok":true,"data":{"active_operations":0}}') as command:
            self.assertEqual(0, rollout.native_active_operations())
            self.assertEqual({'operation': 'daemon.status'}, json.loads(command.call_args.kwargs['payload']))

    def test_native_busy_preflight_is_rejected(self):
        with patch.object(rollout, 'native_active_operations', return_value=1):
            with self.assertRaises(rollout.RolloutError):
                rollout.require_native_idle()

    def test_native_wait_requires_observed_idle_after_busy(self):
        with patch.object(rollout, 'native_active_operations', side_effect=[2, 1, 0]) as status, \
             patch.object(rollout.time, 'sleep') as sleep:
            rollout.wait_native_idle()
        self.assertEqual(3, status.call_count)
        self.assertEqual(2, sleep.call_count)

    def test_native_wait_deadline_does_not_guess_idle(self):
        with patch.object(rollout.time, 'monotonic', side_effect=[0, 0, 61]), \
             patch.object(rollout, 'native_active_operations', return_value=1):
            with self.assertRaises(rollout.RolloutError):
                rollout.wait_native_idle(timeout=60)

    def test_busy_or_unreadable_native_after_folotoy_stop_leaves_deerflow_running(self):
        for reason in ('Native daemon stayed busy', 'Native status unreadable'):
            with self.subTest(reason=reason):
                calls = []
                baseline = {rollout.DEER: {'imageId': 'old-deer'}, rollout.FOLO: {'imageId': 'old-folo'}}
                originals = {path: b'original' for path in rollout.CHANGED_PATHS}
                candidates = {path: b'candidate' for path in rollout.CHANGED_PATHS}
                with patch.object(rollout, 'load_preflight', return_value=baseline), \
                     patch.object(rollout, 'verify_images', return_value={rollout.DEER: 'new-deer', rollout.FOLO: 'new-folo'}), \
                     patch.object(rollout, 'prepare_changes', return_value=(originals, candidates)), \
                     patch.object(rollout, 'probe_waku'), patch.object(rollout, 'require_native_idle'), \
                     patch.object(rollout, 'wait_native_idle', side_effect=rollout.RolloutError(reason)), \
                     patch.object(rollout, 'report'), patch.object(rollout, 'read_file', return_value=b'original'), \
                     patch.object(rollout, 'stop', side_effect=lambda name: calls.append(('stop', name))), \
                     patch.object(rollout, 'up', side_effect=lambda name: calls.append(('up', name))), \
                     patch.object(rollout, 'verify_health'), patch.object(rollout, 'archive_data') as archive:
                    with self.assertRaisesRegex(rollout.RolloutError, 'status=rolled_back'):
                        rollout.apply()
                    archive.assert_not_called()
                self.assertEqual([('stop', 'folotoy'), ('up', 'folotoy')], calls)


if __name__ == '__main__':
    unittest.main()
