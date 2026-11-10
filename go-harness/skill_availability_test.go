package deerflow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestInstalledInspectionDiagnosesUnregisteredSkillWithEmptySnapshot(t *testing.T) {
	workspace, cfg := skillFixture(t)
	cfg.Skills.ManagedSources = []string{"project"}
	cfg.Skills.ExportRoot = t.TempDir()
	cfg.PermissionMode = harness.PermissionModeOff
	var calls atomic.Int32
	var observed string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Stream   bool
			Messages []struct{ Role, Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if calls.Add(1) == 1 {
			skillToolResponse(w, request.Stream, "skill_inspect", `{"name":"research"}`, "")
			return
		}
		for _, msg := range request.Messages {
			if msg.Role == "tool" {
				observed = msg.Content
			}
		}
		skillToolResponse(w, request.Stream, "", "", "Installation needs host recovery.")
	}))
	defer server.Close()
	cfg.Provider, cfg.Model, cfg.BaseURL, cfg.APIKey = "openai", "fixture", server.URL+"/v1", "fixture"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	x, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Run(ctx, x.ID, []harness.Content{{Type: "text", Text: "Inspect the installed research skill"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(observed, "installation_missing") || strings.Contains(observed, "Target or operation is not authorized") {
		t.Fatalf("wrong diagnosis: %s", observed)
	}
	var count int
	if err = c.store.DB().QueryRow(`SELECT count(*) FROM df_skill_installs`).Scan(&count); err != nil || count != 0 {
		t.Fatal("inspection installed skill", count, err)
	}
}

func TestPortableFirstOpenBindsOneDatabaseAndMissingStoreFails(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "user-data")
	cfg := Config{RuntimeProfile: profile, DataDir: filepath.Join(profile, "config", "go-harness-state"), Engine: engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		return harness.RunResult{}, nil
	})}
	c, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(profile, "config", "runtime-data.json")); err != nil {
		t.Fatal("first startup did not persist binding", err)
	}
	if err = os.Remove(filepath.Join(cfg.DataDir, "harness.db")); err != nil {
		t.Fatal(err)
	}
	if c, err = Open(context.Background(), cfg); err == nil {
		_ = c.Close()
		t.Fatal("recreated missing bound database")
	}
	if _, err = os.Stat(filepath.Join(cfg.DataDir, "harness.db")); !os.IsNotExist(err) {
		t.Fatal("failed startup wrote a replacement database")
	}
	if _, err = os.Stat(filepath.Join(profile, "data", "go-harness", "harness.db")); !os.IsNotExist(err) {
		t.Fatal("created another runtime database")
	}
}
