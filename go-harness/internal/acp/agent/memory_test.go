package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
	"github.com/omengye/deerflow-acp/go-harness/internal/memory"
)

func TestACPMemoryManagementScopeOwnerAndVersions(t *testing.T) {
	f := newFixture(t, nil)
	store, err := memory.New(context.Background(), f.db.DB())
	if err != nil {
		t.Fatal(err)
	}
	f.service.Memory = store
	c := connect(t, f.service)
	init := c.success(t, c.request(t, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}))
	if !bytes.Contains(init, []byte(`"searchMethod":"_deerflow/memory/search"`)) || !bytes.Contains(init, []byte(`"flushMethod":"_deerflow/memory/flush"`)) || bytes.Contains(init, []byte(`"user"`)) {
		t.Fatalf("memory capabilities=%s", init)
	}
	sid := c.newSession(t, f.cwd)
	if flushed := c.success(t, c.request(t, "_deerflow/memory/flush", map[string]any{"sessionId": sid})); !bytes.Contains(flushed, []byte(`"flushed":true`)) {
		t.Fatalf("flush=%s", flushed)
	}
	if msg := c.response(t, c.request(t, "_deerflow/memory/flush", map[string]any{"sessionId": sid, "scope": "session"})); msg.Error == nil || msg.Error.Code != protocol.InvalidParams {
		t.Fatalf("unexpected flush fields accepted: %+v", msg)
	}
	other := c.newSession(t, f.cwd)
	base := map[string]any{"sessionId": sid, "scope": "session"}
	fact := map[string]any{"content": "Prefers concise Chinese answers", "category": "preference", "confidence": .9}
	var created harness.MemoryFact
	if err := json.Unmarshal(c.success(t, c.request(t, "_deerflow/memory/create", map[string]any{"sessionId": sid, "scope": "session", "fact": fact})), &created); err != nil || created.Revision != 1 {
		t.Fatalf("create=%+v err=%v", created, err)
	}
	var found struct {
		Facts []harness.MemoryFact `json:"facts"`
	}
	if err := json.Unmarshal(c.success(t, c.request(t, "_deerflow/memory/search", map[string]any{"sessionId": sid, "scope": "session", "query": "concise"})), &found); err != nil || len(found.Facts) != 1 || found.Facts[0].ID != created.ID {
		t.Fatalf("search=%+v err=%v", found, err)
	}
	if err := json.Unmarshal(c.success(t, c.request(t, "_deerflow/memory/search", map[string]any{"sessionId": other, "scope": "session", "query": "concise"})), &found); err != nil || len(found.Facts) != 0 {
		t.Fatalf("cross-session search=%+v err=%v", found, err)
	}
	stranger := connect(t, f.service)
	stranger.initialize(t)
	if msg := stranger.response(t, stranger.request(t, "_deerflow/memory/flush", map[string]any{"sessionId": sid})); msg.Error == nil || msg.Error.Code != protocol.InvalidParams {
		t.Fatalf("foreign owner flush=%+v", msg)
	}
	if msg := stranger.response(t, stranger.request(t, "_deerflow/memory/get", map[string]any{"sessionId": sid, "scope": "session", "factId": created.ID})); msg.Error == nil || msg.Error.Code != protocol.InvalidParams {
		t.Fatalf("foreign owner=%+v", msg)
	}
	var page harness.MemoryPage
	if err := json.Unmarshal(c.success(t, c.request(t, "_deerflow/memory/list", base)), &page); err != nil || page.ScopeRevision != 1 || len(page.Facts) != 1 {
		t.Fatalf("list=%+v err=%v", page, err)
	}
	if msg := c.response(t, c.request(t, "_deerflow/memory/replace", map[string]any{"sessionId": sid, "scope": "session", "factId": created.ID, "expectedRevision": 2, "fact": fact})); msg.Error == nil || msg.Error.Code != -32016 {
		t.Fatalf("stale revision=%+v", msg)
	}
	c.success(t, c.request(t, "_deerflow/memory/delete", map[string]any{"sessionId": sid, "scope": "session", "factId": created.ID, "expectedRevision": 1}))
	if err := json.Unmarshal(c.success(t, c.request(t, "_deerflow/memory/search", map[string]any{"sessionId": sid, "scope": "session", "query": "concise"})), &found); err != nil || len(found.Facts) != 0 {
		t.Fatalf("deleted search=%+v err=%v", found, err)
	}
	for _, params := range []map[string]any{
		{"sessionId": sid, "scope": "user"},
		{"sessionId": sid, "scope": "session", "extra": true},
		{"sessionId": sid, "scope": "session", "limit": 101},
	} {
		if msg := c.response(t, c.request(t, "_deerflow/memory/list", params)); msg.Error == nil || msg.Error.Code != protocol.InvalidParams {
			t.Fatalf("invalid list=%+v", msg)
		}
	}
	if msg := c.response(t, c.request(t, "_deerflow/memory/create", map[string]any{"sessionId": sid, "scope": "session", "fact": map[string]any{"content": "x", "category": "y", "confidence": .5, "source": "forged"}})); msg.Error == nil || msg.Error.Code != protocol.InvalidParams {
		t.Fatalf("forged source=%+v", msg)
	}
}
