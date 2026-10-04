package desktopconfig

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/launch"
)

const behaviorSuiteFixture = `{"version":"skill-behavior-v1","id":"source-verification","source_id":"python-custom","name":"research","cases":[{"id":"verify","kind":"targeted","prompt":"Read input.txt and answer done.","files":{"input.txt":"fixture"},"fixtures":[],"assertions":[{"id":"answer","kind":"output_equals","expected":"done"}],"repetitions":1}]}`

func behaviorDocument(t *testing.T) (Options, map[string]any) {
	t.Helper()
	options, doc := generationDocument(t)
	if err := os.WriteFile(filepath.Join(filepath.Dir(options.Config), "behavior-suite.json"), []byte(behaviorSuiteFixture), 0600); err != nil {
		t.Fatal(err)
	}
	behavior := readObject(readObject(doc, "skill_evolution"), "behavior")
	if behavior["enabled"] != false {
		t.Fatal("behavior enabled by default")
	}
	behavior["enabled"], behavior["suite_path"] = true, "behavior-suite.json"
	return options, doc
}

func TestBehaviorSettingsSaveValidateLaunchAndPinnedSuite(t *testing.T) {
	options, doc := behaviorDocument(t)
	settings := readObject(readObject(doc, "skill_evolution"), "behavior")
	settings["timeout_seconds"], settings["max_steps"], settings["max_input_bytes"], settings["max_output_tokens"] = 7, 4, 8192, 512
	before, _ := os.ReadFile(options.Config)
	if run(t, options, "validate", doc)["valid"] != true {
		t.Fatal("valid fixture rejected")
	}
	after, _ := os.ReadFile(options.Config)
	if !bytes.Equal(before, after) {
		t.Fatal("validation changed config")
	}
	doc = run(t, options, "save", doc)
	if !reflect.DeepEqual(readObject(readObject(doc, "skill_evolution"), "behavior"), settings) {
		t.Fatal("behavior settings changed on save")
	}
	var cfg deerflow.Config
	connections := 0
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	if cfg.SkillBehavior == nil || cfg.SkillBehaviorEvaluator == nil || len(cfg.SkillBehavior.Suites) != 1 || cfg.SkillBehavior.Suites[0].SourceID != "python-custom" {
		t.Fatal("behavior was not applied")
	}
	identity := cfg.SkillBehaviorEvaluator.Identity()
	if identity == "" {
		t.Fatal("unversioned evaluator")
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(options.Config), "behavior-suite.json"), []byte(`{"invalid":"replacement"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if cfg.SkillBehavior.Suites[0].Cases[0].Prompt != "Read input.txt and answer done." {
		t.Fatal("loaded fixture changed after disk edit")
	}
	if _, err := launch.ApplyConfig(options.Config, &deerflow.Config{}, &connections, nil); err == nil {
		t.Fatal("changed invalid suite loaded")
	}
	readObject(readObject(doc, "skill_evolution"), "behavior")["enabled"] = false
	doc = run(t, options, "save", doc)
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil || cfg.SkillBehavior != nil || cfg.SkillBehaviorEvaluator != nil || cfg.SkillGeneration == nil {
		t.Fatalf("behavior disable retained evaluator: %v", err)
	}
}

func TestBehaviorSettingsRejectInvalidSuiteBeforeSaving(t *testing.T) {
	for _, test := range []struct{ name, body string }{
		{"unknown-source", strings.Replace(behaviorSuiteFixture, "python-custom", "ungranted", 1)},
		{"duplicate-key", strings.Replace(behaviorSuiteFixture, `"id":"verify"`, `"id":"verify","id":"other"`, 1)},
		{"executable-oracle", strings.Replace(behaviorSuiteFixture, `"output_equals"`, `"shell_command"`, 1)},
		{"unsafe-file", strings.Replace(behaviorSuiteFixture, `"input.txt":"fixture"`, `"../outside.txt":"fixture"`, 1)},
		{"oversized", strings.Repeat("x", (1<<20)+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			options, doc := behaviorDocument(t)
			if err := os.WriteFile(filepath.Join(filepath.Dir(options.Config), "behavior-suite.json"), []byte(test.body), 0600); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(options.Config)
			for _, op := range []string{"validate", "save"} {
				if _, err := Run(options, op, mustJSON(t, doc)); err == nil || !strings.Contains(err.Error(), "behavior.suite_path") {
					t.Fatalf("%s accepted invalid suite: %v", op, err)
				}
			}
			after, _ := os.ReadFile(options.Config)
			if !bytes.Equal(before, after) {
				t.Fatal("rejected suite changed config")
			}
		})
	}
}

func TestBehaviorSettingsRejectMissingFileAndDependencyConflicts(t *testing.T) {
	for _, mode := range []string{"missing", "directory", "generation-disabled", "evolution-disabled", "fraction", "null"} {
		t.Run(mode, func(t *testing.T) {
			options, doc := behaviorDocument(t)
			review := readObject(doc, "skill_evolution")
			behavior := readObject(review, "behavior")
			switch mode {
			case "missing":
				behavior["suite_path"] = "absent.json"
			case "directory":
				behavior["suite_path"] = "."
			case "generation-disabled":
				readObject(review, "generation")["enabled"] = false
			case "evolution-disabled":
				review["enabled"] = false
			case "fraction":
				behavior["max_steps"] = 2.5
			case "null":
				behavior["max_steps"] = nil
			}
			for _, op := range []string{"validate", "save"} {
				if _, err := Run(options, op, mustJSON(t, doc)); err == nil || !strings.Contains(err.Error(), "behavior") {
					t.Fatalf("%s accepted %s: %v", op, mode, err)
				}
			}
			raw, err := readYAML(options.Config)
			if err != nil {
				t.Fatal(err)
			}
			raw["skill_evolution"] = review
			if err := writeYAMLAtomic(options.Config, raw); err != nil {
				t.Fatal(err)
			}
			connections := 0
			if _, err := launch.ApplyConfig(options.Config, &deerflow.Config{}, &connections, nil); err == nil || !strings.Contains(err.Error(), "behavior") {
				t.Fatalf("launch accepted %s: %v", mode, err)
			}
		})
	}
}
