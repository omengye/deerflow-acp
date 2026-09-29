package mcp

import (
	"context"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestGenerationChangesAfterReconnectWithIdenticalSchema(t *testing.T) {
	ctx := context.Background()
	cfg := processConfig(t, "one")
	cwd := t.TempDir()
	m := testManager(t, harness.MCPPolicy{AllowedCommands: []string{cfg.Command}})
	bind := func() {
		t.Helper()
		if err := m.Bind(ctx, "owner", "s", cwd, []harness.MCPServer{cfg}); err != nil {
			t.Fatal(err)
		}
	}
	version := func() string {
		t.Helper()
		v, err := m.Generation(ctx, "s")
		if err != nil || v == "" {
			t.Fatalf("generation=%q err=%v", v, err)
		}
		return v
	}
	bind()
	first := version()
	bind()
	if version() != first {
		t.Fatal("idempotent bind changed generation")
	}
	if err := m.Release(ctx, "owner", "s"); err != nil {
		t.Fatal(err)
	}
	bind()
	if version() == first {
		t.Fatal("new connection reused saved execution identity")
	}
	if err := m.Bind(ctx, "owner", "empty", cwd, nil); err != nil {
		t.Fatal(err)
	}
	if v, err := m.Generation(ctx, "empty"); err != nil || v != "" {
		t.Fatalf("empty resources have identity: %q %v", v, err)
	}
}
