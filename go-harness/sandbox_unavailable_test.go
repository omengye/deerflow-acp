package deerflow

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestNewConversationSurvivesMissingCommandExecutable(t *testing.T) {
	provider, offered := commandProvider(t, `{"executable":"unavailable"}`)
	workspace := t.TempDir()
	c, err := Open(context.Background(), Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", BaseURL: provider.URL + "/v1", APIKey: "fixture", DisableSubagents: true,
		Sandbox: harness.SandboxConfig{Enabled: true, Provider: harness.SandboxLocal, AllowedExecutables: []string{filepath.Join(workspace, "missing-runtime.exe")}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s, err := c.NewSession(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	approvals := 0
	result, err := c.Run(context.Background(), s.ID, []harness.Content{{Type: "text", Text: "hi"}}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		approvals++
		return harness.RejectOnce, nil
	})
	if err != nil || result.StopReason != "end_turn" || offered() || approvals != 0 {
		t.Fatalf("optional command runtime broke ordinary chat: %+v err=%v offered=%t approvals=%d", result, err, offered(), approvals)
	}
}
