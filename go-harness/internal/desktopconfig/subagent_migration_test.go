package desktopconfig

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/configschema"
	"github.com/omengye/deerflow-acp/go-harness/internal/launch"
)

func TestSubagentMigrationRequiresReviewedRevisionAndPreservesNativeBehavior(t *testing.T) {
	for _, gate := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[gate], func(t *testing.T) {
			options := setup(t)
			run(t, options, "init", nil)
			raw, err := readYAML(options.Config)
			if err != nil {
				t.Fatal(err)
			}
			raw["config_version"] = 16
			local := object(raw, "local_acp")
			local["subagent_enabled"], local["max_concurrent_subagents"] = gate, 5
			roles := object(raw, "subagents")
			roles["enabled"] = true
			delete(roles, "max_concurrent")
			if err := writeYAMLAtomic(options.Config, raw); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(options.Config)
			doc := run(t, options, "snapshot", nil)
			if readObject(doc, "subagents")["enabled"] != gate || readObject(doc, "subagents")["max_concurrent"] != 5 {
				t.Fatal("snapshot did not fold legacy controls")
			}
			if _, old := readObject(doc, "runtime")["subagent_enabled"]; old {
				t.Fatal("legacy editor remains")
			}
			for _, ack := range []any{nil, "", "stale-revision", true} {
				doc["config_migration_ack"] = ack
				if _, err := Run(options, "save", mustJSON(t, doc)); err == nil {
					t.Fatal("unreviewed migration was saved")
				}
				after, _ := os.ReadFile(options.Config)
				if !bytes.Equal(before, after) {
					t.Fatal("rejected migration modified file")
				}
			}
			run(t, options, "validate", doc)
			doc["config_migration_ack"] = doc["config_revision"]
			saved := run(t, options, "save", doc)
			backup, _ := os.ReadFile(filepath.Join(saved["backup"].(string), filepath.Base(options.Config)))
			if !bytes.Equal(backup, before) {
				t.Fatal("original legacy bytes not backed up")
			}
			after, err := readYAML(options.Config)
			if err != nil {
				t.Fatal(err)
			}
			if after["config_version"] != configschema.NativeConfigVersion {
				t.Fatal("native version missing")
			}
			for _, key := range []string{"subagent_enabled", "max_concurrent_subagents"} {
				if _, old := readObject(after, "local_acp")[key]; old {
					t.Fatal("shadow alias remains")
				}
			}
			if readObject(saved, "config_migration")["required"] != false || saved["config_migration_ack"] != "" {
				t.Fatal("migration acknowledgement not cleared")
			}
			var cfg deerflow.Config
			connections := 0
			if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil {
				t.Fatal(err)
			}
			if cfg.DisableSubagents == gate || *cfg.Subagents.MaxConcurrent != 5 {
				t.Fatal("migration changed original default execution policy")
			}
			// A native toggle is no longer shadowed by the deleted local key.
			readObject(saved, "subagents")["enabled"] = !gate
			run(t, options, "save", saved)
			if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil {
				t.Fatal(err)
			}
			if cfg.DisableSubagents != gate {
				t.Fatal("single native gate did not control launch")
			}
		})
	}
}

func TestSubagentMigrationRejectsChangedSourceAfterReview(t *testing.T) {
	options := setup(t)
	run(t, options, "init", nil)
	raw, _ := readYAML(options.Config)
	delete(raw, "config_version")
	delete(readObject(raw, "subagents"), "max_concurrent")
	if err := writeYAMLAtomic(options.Config, raw); err != nil {
		t.Fatal(err)
	}
	doc := run(t, options, "snapshot", nil)
	doc["config_migration_ack"] = doc["config_revision"]
	readObject(raw, "subagents")["enabled"] = true
	if err := writeYAMLAtomic(options.Config, raw); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(options.Config)
	if _, err := Run(options, "save", mustJSON(t, doc)); err == nil {
		t.Fatal("stale source was migrated")
	}
	after, _ := os.ReadFile(options.Config)
	if !bytes.Equal(before, after) {
		t.Fatal("stale review changed file")
	}
}
