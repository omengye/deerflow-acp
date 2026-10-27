import unittest
import deploy

class ConfigTest(unittest.TestCase):
    def test_preserves_other_sections_and_is_repeatable(self):
        original='models:\n  - name: example\nmemory:\n  enabled: false\n  extraction_enabled: true\nsandbox:\n  provider: local\nlocal_acp:\n  memory_scope: session\n  enable_bash: true\n'
        value=deploy.memory_config(original)
        self.assertIn('  extraction_enabled: true\n',value)
        self.assertIn('sandbox:\n  provider: local\n',value)
        self.assertIn('  memory_scope: workspace\n',value)
        self.assertIn('  enable_bash: true\n',value)
        self.assertEqual(deploy.memory_config(value),value)
        self.assertEqual(value.count('single_user:'),1)
    def test_ambiguous_section_fails(self):
        for value in ('models: {}\n','memory:\n  enabled: true\nmemory:\n'):
            with self.assertRaises(RuntimeError):deploy.memory_config(value)
    def test_four_space_go_generated_configuration(self):
        raw='memory:\n    enabled: false\n    include_global: false\nlocal_acp:\n    memory_scope: session\n    enable_bash: true\n'
        value=deploy.memory_config(raw,embedding=False)
        self.assertEqual(value.count('enabled:'),3)
        self.assertNotIn('false',value)
        self.assertIn('    memory_scope: workspace\n',value)
        self.assertEqual(value,deploy.memory_config(value,embedding=False))
if __name__=='__main__':unittest.main()
