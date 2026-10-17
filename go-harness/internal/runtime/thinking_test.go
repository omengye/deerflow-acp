package runtime

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestThinkingAvailabilityAndEffortAreIndependent(t *testing.T) {
	s := &Service{Model: "unknown", Settings: ConfigSettings{
		Models:                []harness.ConfigValue{{Value: "unknown"}, {Value: "fixed"}, {Value: "switchable"}},
		ModelReasoningEfforts: map[string][]string{"fixed": {"low", "high"}, "switchable": {"high"}},
		ModelThinking:         map[string]harness.ThinkingConfig{"fixed": {Parameter: "unsupported"}, "switchable": {Parameter: "thinking_type"}},
	}}
	for _, model := range []string{"unknown", "fixed", "switchable"} {
		options := s.ConfigOptions(harness.Session{Model: model, ThinkingMode: "off", ReasoningEffort: "high"})
		meta := options[0].Meta["deerflow"].(map[string]any)
		availability := meta["thinkingAvailability"].(map[string]string)
		if availability["unknown"] != "unconfigured" || availability["fixed"] != "unsupported" || availability["switchable"] != "configured" {
			t.Fatalf("capabilities=%v", availability)
		}
		hasThinking, hasEffort := false, false
		for _, option := range options {
			if option.ID == "thinking_enabled" {
				hasThinking = true
			}
			if option.ID == "reasoning_effort" {
				hasEffort = true
				if option.Category != "thought_level" || option.CurrentValue != "high" {
					t.Fatal("effort changed by thinking switch")
				}
			}
		}
		if hasThinking != (model == "switchable") || hasEffort != (model != "unknown") {
			t.Fatalf("%s thinking=%v effort=%v", model, hasThinking, hasEffort)
		}
	}
}

func TestThinkingMigrationPreservesLegacySession(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	_, err = db.DB().ExecContext(ctx, `CREATE TABLE harness_session_configs (session_id TEXT PRIMARY KEY,approval_mode TEXT NOT NULL,subagents INTEGER NOT NULL,version INTEGER NOT NULL,reasoning_effort TEXT NOT NULL);
INSERT INTO harness_session_configs VALUES('old','ask',1,4,'high');`)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrateSessionThinking(ctx, db.DB()); err != nil {
			t.Fatal(err)
		}
	}
	var effort, mode string
	var version int
	if err := db.DB().QueryRowContext(ctx, `SELECT reasoning_effort,thinking_mode,version FROM harness_session_configs WHERE session_id='old'`).Scan(&effort, &mode, &version); err != nil {
		t.Fatal(err)
	}
	if effort != "high" || mode != "" || version != 4 {
		t.Fatalf("migration changed legacy settings: %s %s %d", effort, mode, version)
	}
	x := harness.Session{ID: "old", ReasoningEffort: effort, ConfigVersion: 4}
	before := executionConfigDigest(x)
	x.ThinkingMode = "off"
	if executionConfigDigest(x) == before {
		t.Fatal("thinking not bound to durable execution")
	}
}
