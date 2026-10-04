package deerflow

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
)

func TestGlobalMemorySharingPromotionAndReopen(t *testing.T) {
	ctx := context.Background()
	cfg := Config{DataDir: t.TempDir(), MemoryScope: harness.MemoryWorkspace, Engine: engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		return harness.RunResult{StopReason: "end_turn"}, nil
	})}
	c, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if c != nil {
			c.Close()
		}
	}()
	a, err := c.NewSession(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.NewSession(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	profile := c.service.MemoryGlobalID
	local, err := c.CreateMemoryFact(ctx, a.ID, harness.MemoryWorkspace, harness.MemoryCandidate{Content: "Prefers concise Chinese answers", Category: "preference", Confidence: .9})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.MemoryFact(ctx, b.ID, harness.MemoryWorkspace, local.ID); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("project isolation: %v", err)
	}
	if _, err = c.PromoteMemoryFact(ctx, a.ID, harness.MemoryWorkspace, local.ID, local.Revision+1); !errors.Is(err, harness.ErrExecutionConflict) {
		t.Fatalf("stale promotion: %v", err)
	}
	global, err := c.PromoteMemoryFact(ctx, a.ID, harness.MemoryWorkspace, local.ID, local.Revision)
	if err != nil || global.Scope != harness.MemoryGlobal || global.Source.InputID != local.ID {
		t.Fatalf("promotion: %+v %v", global, err)
	}
	duplicate, err := c.PromoteMemoryFact(ctx, a.ID, harness.MemoryWorkspace, local.ID, local.Revision)
	if err != nil || duplicate.ID != global.ID {
		t.Fatalf("duplicate: %+v %v", duplicate, err)
	}
	if _, err := c.MemoryFact(ctx, a.ID, harness.MemoryWorkspace, local.ID); err != nil {
		t.Fatalf("source lost: %v", err)
	}
	if _, err := c.MemoryFact(ctx, b.ID, harness.MemoryGlobal, global.ID); err != nil {
		t.Fatalf("cross-project global: %v", err)
	}
	readCfg := cfg
	readCfg.memoryGlobalID = profile
	req := harness.RunRequest{Session: b, Input: []harness.Content{{Type: "text", Text: "unrelated astronomy question"}}}
	snapshots, err := selectMemory(ctx, c.service.Memory, req, readCfg)
	if err != nil || len(snapshots) != 2 || len(snapshots[0].Facts) != 0 || len(snapshots[1].Facts) != 1 {
		t.Fatalf("global preference injection: %+v %v", snapshots, err)
	}
	hits, err := searchMemory(ctx, c.service.Memory, req, readCfg, "concise", 6)
	if err != nil || len(hits) != 1 || hits[0].Scope != harness.MemoryGlobal {
		t.Fatalf("global retrieval: %+v %v", hits, err)
	}
	hits, err = searchMemory(ctx, c.service.Memory, req, readCfg, "astronomy", 6)
	if err != nil || len(hits) != 0 {
		t.Fatalf("explicit search must match: %+v %v", hits, err)
	}
	include := false
	readCfg.MemoryIncludeGlobal = &include
	snapshots, err = selectMemory(ctx, c.service.Memory, req, readCfg)
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("sharing disabled: %+v %v", snapshots, err)
	}
	manage := func(raw string) (any, error) {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &fields); err != nil {
			t.Fatal(err)
		}
		var op string
		json.Unmarshal(fields["operation"], &op)
		return c.ManageLocal(ctx, localhost.ManagementRequest{Operation: op, Fields: fields})
	}
	data, err := manage(`{"operation":"memory.get","scope":"global"}`)
	if err != nil || len(data.(map[string]any)["memory"].(map[string]any)["facts"].([]map[string]any)) != 1 {
		t.Fatalf("sessionless global management: %v", err)
	}
	if _, err := manage(`{"operation":"memory.promote","scope":"workspace","session_id":"` + a.ID + `","fact_id":"` + local.ID + `"}`); err == nil {
		t.Fatal("unreviewed promotion accepted")
	}
	data, err = manage(`{"operation":"memory.promote","scope":"workspace","session_id":"` + a.ID + `","fact_id":"` + local.ID + `","expected_revision":1}`)
	if err != nil || data.(map[string]any)["promoted"].(map[string]any)["id"] != global.ID {
		t.Fatalf("reviewed management promotion: %v", err)
	}
	_, err = manage(`{"operation":"memory.delete","scope":"global","fact_id":"` + global.ID + `","expected_revision":2}`)
	var conflict *localhost.ManagementError
	if !errors.As(err, &conflict) || conflict.Code != "memory_conflict" {
		t.Fatalf("global management stale delete: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = nil
	c, err = Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c.service.MemoryGlobalID != profile {
		t.Fatal("profile changed after restart")
	}
	if _, err := c.LoadSession(ctx, b.ID, b.CWD, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.MemoryFact(ctx, b.ID, harness.MemoryGlobal, global.ID); err != nil {
		t.Fatalf("global memory lost after restart: %v", err)
	}
	other, err := Open(ctx, Config{DataDir: t.TempDir(), Engine: cfg.Engine})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if other.service.MemoryGlobalID == profile {
		t.Fatal("different profiles share identity")
	}
}
