package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestLegacyMigrationRequiresExplicitMappingAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	dataDir, workspace := t.TempDir(), t.TempDir()
	client, err := deerflow.Open(ctx, deerflow.Config{DataDir: dataDir, Model: "fixture", Engine: disabledEngine{}})
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "memory.json")
	if err := os.WriteFile(source, []byte(`{"version":"1.0","facts":[{"id":"fact_1","content":"Prefers concise Chinese answers","category":"preference","confidence":0.9}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run([]string{"--source", source}, &out); err != nil {
		t.Fatal(err)
	}
	var preview harness.LegacyImportReport
	if err := json.Unmarshal(out.Bytes(), &preview); err != nil || preview.Read != 1 || preview.Imported != 0 {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	if err := run([]string{"--source", source, "--apply"}, &out); err == nil {
		t.Fatal("apply accepted an implicit destination")
	}
	args := []string{"--source", source, "--data-dir", dataDir, "--workspace", workspace, "--session-id", session.ID, "--scope", "workspace", "--apply"}
	for attempt := 0; attempt < 2; attempt++ {
		out.Reset()
		if err := run(args, &out); err != nil {
			t.Fatal(err)
		}
		var report harness.LegacyImportReport
		if err := json.Unmarshal(out.Bytes(), &report); err != nil || attempt == 0 && report.Imported != 1 || attempt == 1 && report.Existing != 1 {
			t.Fatalf("attempt %d report=%+v err=%v", attempt, report, err)
		}
	}
	client, err = deerflow.Open(ctx, deerflow.Config{DataDir: dataDir, Model: "fixture", Engine: disabledEngine{}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.LoadSession(ctx, session.ID, workspace, false, nil); err != nil {
		t.Fatal(err)
	}
	page, err := client.MemoryFacts(ctx, session.ID, harness.MemoryWorkspace, "", 10)
	if err != nil || len(page.Facts) != 1 || page.Facts[0].Source.ActorID != "legacy-deermem" {
		t.Fatalf("imported facts=%+v err=%v", page, err)
	}
}
