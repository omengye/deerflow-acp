package desktopconfig

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/configschema"
	"github.com/omengye/deerflow-acp/go-harness/internal/launch"
)

func TestSettingsContractSaveIsConsumedByNativeLaunch(t *testing.T) {
	options := setup(t)
	doc := run(t, options, "snapshot", nil)
	runtime := readObject(doc, "runtime")
	runtime["model_name"] = "second"
	readObject(doc, "subagents")["enabled"] = true
	readObject(doc, "subagents")["max_concurrent"] = 64
	runtime["max_active_connections"] = 128
	runtime["max_active_runs"] = 3
	runtime["run_timeout_seconds"] = .125
	runtime["queue_timeout_seconds"] = .5
	runtime["session_cleanup_enabled"] = true
	runtime["inactive_session_retention_days"] = 7
	runtime["closed_session_retention_days"] = 0
	runtime["session_cleanup_interval_seconds"] = 60.5
	runtime["permission_mode"] = "all"
	runtime["memory_scope"] = "session"
	runtime["tool_allowlist"] = []string{}
	runtime["tool_denylist"] = []string{"bash"}
	runtime["prompt_overlay"] = strings.Repeat("界", 33000) // Characters, not bytes.
	memory := readObject(doc, "memory")
	memory["injection_enabled"] = false
	memory["retrieval_enabled"] = false
	saved := run(t, options, "save", doc)
	if saved["config_schema"].(configschema.Schema).Hash != configschema.Describe().Hash {
		t.Fatal("snapshot does not carry current schema")
	}
	var cfg deerflow.Config
	connections := 0
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "gpt-b" || cfg.DisableSubagents || cfg.Subagents == nil || cfg.Subagents.MaxConcurrent == nil || *cfg.Subagents.MaxConcurrent != 64 {
		t.Fatal("saved model/role configuration not consumed")
	}
	if connections != 128 || cfg.MaxActiveRuns != 3 || cfg.Budget.Timeout != 125*time.Millisecond || cfg.QueueTimeout != 500*time.Millisecond {
		t.Fatal("saved limits not consumed")
	}
	if !cfg.Retention.Enabled || cfg.Retention.InactiveDays != 7 || cfg.Retention.ClosedDays != 0 || cfg.Retention.CheckInterval != 60500*time.Millisecond {
		t.Fatal("saved retention not consumed")
	}
	if cfg.PermissionMode != harness.PermissionModeAll || cfg.MemoryScope != harness.MemorySession || cfg.MemoryMode != "middleware" || cfg.MemoryEnabled == nil || !*cfg.MemoryEnabled || !cfg.MemoryExtraction || cfg.MemoryInjectionEnabled == nil || *cfg.MemoryInjectionEnabled || cfg.MemoryRetrievalEnabled == nil || *cfg.MemoryRetrievalEnabled {
		t.Fatal("saved permissions/memory not consumed")
	}
	if cfg.ToolPolicy.Allowlist == nil || len(cfg.ToolPolicy.Allowlist) != 0 || !reflect.DeepEqual(cfg.ToolPolicy.Denylist, []string{"bash"}) || !strings.Contains(cfg.Instruction, runtime["prompt_overlay"].(string)) {
		t.Fatal("saved tool policy or Unicode overlay changed")
	}
	raw, err := readYAML(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	if _, shadow := readObject(readObject(raw, "memory"), "backend_config")["retrieval_enabled"]; shadow {
		t.Fatal("legacy retrieval alias shadows native setting")
	}
}

func TestSettingsContractNativeSaveDoesNotInjectLegacyDefaults(t *testing.T) {
	options := setup(t)
	run(t, options, "init", nil)
	raw, err := readYAML(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	raw["memory"] = map[string]any{"enabled": true}
	if err := writeYAMLAtomic(options.Config, raw); err != nil {
		t.Fatal(err)
	}
	doc := run(t, options, "snapshot", nil)
	if len(readObject(doc, "memory")) != len(configschema.Defaults("memory")) {
		t.Fatal("snapshot exposes unsupported memory options")
	}
	for _, key := range []string{"memory", "memory_index"} {
		if _, exists := readObject(doc, "paths")[key]; exists {
			t.Fatal("snapshot exposes a non-runtime memory file")
		}
	}
	run(t, options, "save", doc)
	raw, err = readYAML(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	if len(readObject(raw, "memory")) != len(configschema.Defaults("memory")) {
		t.Fatal("native save injected legacy memory defaults")
	}
	for _, key := range []string{"agent_name", "thinking_enabled", "plan_mode", "goal_auto_continue", "goal_max_continuations", "goal_max_no_progress_continuations"} {
		if _, exists := readObject(raw, "local_acp")[key]; exists {
			t.Fatalf("save injected %s", key)
		}
	}
}

func TestSettingsContractPreservesOriginalUnknownsButRejectsDraftInjection(t *testing.T) {
	options := setup(t)
	run(t, options, "init", nil)
	original, err := readYAML(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	before := readObject(readObject(original, "memory"), "backend_config")
	delete(before, "retrieval_enabled")
	doc := run(t, options, "snapshot", nil)
	run(t, options, "save", doc)
	saved, err := readYAML(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, readObject(readObject(saved, "memory"), "backend_config")) {
		t.Fatal("unrelated save changed original migration data")
	}
	for _, tc := range []struct{ section, key string }{{"runtime", "goal_auto_continue"}, {"memory", "storage_class"}, {"memory", "advanced"}} {
		doc = run(t, options, "snapshot", nil)
		readObject(doc, tc.section)[tc.key] = "injected"
		request, _ := json.Marshal(doc)
		if _, err := Run(options, "save", request); err == nil || !strings.Contains(err.Error(), tc.section+"/"+tc.key) {
			t.Fatalf("unsupported draft %s/%s: %v", tc.section, tc.key, err)
		}
	}
}

func TestSettingsContractRejectsSameScalarValuesOnSaveAndLaunch(t *testing.T) {
	for _, tc := range []struct {
		section, key string
		value        any
	}{
		{"runtime", "max_active_runs", 1.5},
		{"subagents", "max_concurrent", 65},
		{"runtime", "run_timeout_seconds", .00000000001},
		{"runtime", "queue_timeout_seconds", -1},
		{"subagents", "enabled", "true"},
		{"runtime", "tool_allowlist", []string{"bad\nname"}},
		{"runtime", "prompt_overlay", strings.Repeat("界", 65537)},
		{"memory", "enabled", "false"},
		{"memory", "mode", "unknown"},
	} {
		t.Run(tc.section+"/"+tc.key, func(t *testing.T) {
			options := setup(t)
			doc := run(t, options, "snapshot", nil)
			readObject(doc, tc.section)[tc.key] = tc.value
			request, _ := json.Marshal(doc)
			before, _ := os.ReadFile(options.Config)
			if _, err := Run(options, "save", request); err == nil {
				t.Fatal("save accepted invalid scalar")
			}
			after, _ := os.ReadFile(options.Config)
			if string(before) != string(after) {
				t.Fatal("invalid save changed configuration")
			}
			raw, err := readYAML(options.Config)
			if err != nil {
				t.Fatal(err)
			}
			section := tc.section
			if section == "runtime" {
				section = "local_acp"
			}
			object(raw, section)[tc.key] = tc.value
			if err := writeYAMLAtomic(options.Config, raw); err != nil {
				t.Fatal(err)
			}
			var cfg deerflow.Config
			connections := 0
			if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err == nil {
				t.Fatal("launch accepted scalar rejected by save")
			}
		})
	}
}

func TestSettingsContractStaleSchemaDoesNotWrite(t *testing.T) {
	options := setup(t)
	doc := run(t, options, "snapshot", nil)
	doc["config_schema"] = map[string]any{"version": configschema.Version, "hash": "stale"}
	request, _ := json.Marshal(doc)
	before, _ := os.ReadFile(options.Config)
	if _, err := Run(options, "save", request); err == nil {
		t.Fatal("stale schema accepted")
	}
	after, _ := os.ReadFile(options.Config)
	if string(before) != string(after) {
		t.Fatal("stale schema wrote configuration")
	}
}

func TestSettingsContractLegacyFallbacksSurviveUnrelatedSave(t *testing.T) {
	options := setup(t)
	run(t, options, "init", nil)
	raw, err := readYAML(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	object(raw, "api")["chat_request_timeout"] = .75
	local := object(raw, "local_acp")
	delete(local, "run_timeout_seconds")
	local["permission_mode"], local["memory_scope"] = "", ""
	object(raw, "memory")["mode"] = ""
	object(readObject(raw, "memory"), "backend_config")["retrieval_enabled"] = false
	if err := writeYAMLAtomic(options.Config, raw); err != nil {
		t.Fatal(err)
	}
	doc := run(t, options, "snapshot", nil)
	if readObject(doc, "runtime")["run_timeout_seconds"] != .75 || readObject(doc, "memory")["retrieval_enabled"] != false {
		t.Fatal("snapshot does not preserve native fallback semantics")
	}
	run(t, options, "save", doc)
	var cfg deerflow.Config
	connections := 0
	if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	if cfg.Budget.Timeout != 750*time.Millisecond || cfg.PermissionMode != harness.PermissionModeDangerous || cfg.MemoryScope != harness.MemoryWorkspace || cfg.MemoryMode != "middleware" || cfg.MemoryRetrievalEnabled == nil || *cfg.MemoryRetrievalEnabled {
		t.Fatal("unrelated save changed fallback values")
	}
}
