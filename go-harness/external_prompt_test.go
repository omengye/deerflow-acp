package deerflow

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSDKExternalPromptAcknowledgementRequiresReviewedReceipt(t *testing.T) {
	ctx := context.Background()
	args := json.RawMessage(`{"agent":"fixture","prompt":"work"}`)
	engine := engineFunc(func(ctx context.Context, _ harness.RunRequest, emit harness.EventHandler, permission harness.PermissionHandler) (harness.RunResult, error) {
		if err := emit(ctx, harness.RunEvent{Kind: "tool_start", ToolCallID: "delegate", ToolName: "invoke_acp_agent", Status: "pending", Arguments: args}); err != nil {
			return harness.RunResult{}, err
		}
		if _, err := permission(ctx, harness.PermissionRequest{ToolCallID: "delegate", ToolName: "invoke_acp_agent", Arguments: args}); err != nil {
			return harness.RunResult{}, err
		}
		if err := emit(ctx, harness.RunEvent{Kind: "tool_execute", ToolCallID: "delegate", ToolName: "invoke_acp_agent", Status: "in_progress"}); err != nil {
			return harness.RunResult{}, err
		}
		return harness.RunResult{}, errors.New("remote result unavailable")
	})
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	c, err := Open(ctx, Config{DataDir: root, Engine: engine, ACPAgents: map[string]harness.ACPAgentConfig{"fixture": {Command: executable}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	session, err := c.NewSession(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "delegate"}}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		return harness.AllowOnce, nil
	}); err == nil {
		t.Fatal("fixture did not leave a remote result uncertain")
	}
	receipts, err := c.ListToolReceipts(ctx, session.ID)
	if err != nil || len(receipts) != 1 || receipts[0].State != harness.ReceiptUncertain {
		t.Fatalf("receipts=%+v err=%v", receipts, err)
	}
	r := receipts[0]
	identity := sha256.Sum256([]byte(session.ID))
	path := filepath.Join(root, "acp-agent-sessions", fmt.Sprintf("%x", identity[:]), "fixture.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	state := map[string]any{"session_id": "remote-1", "policy": "pinned", "pending": map[string]any{"id": "prompt-1", "prompt_sha": "task-digest", "run_id": r.RunID, "call_id": r.ToolCallID, "arguments_sha": r.ArgumentsDigest}}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	pending, err := c.PendingExternalPrompt(ctx, session.ID, "fixture")
	if err != nil || pending.PromptID != "prompt-1" || pending.ArgumentsSHA != r.ArgumentsDigest {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	if err := c.AcknowledgeExternalPrompt(ctx, session.ID, "fixture", pending.PromptID); !errors.Is(err, harness.ErrReconciliationRequired) {
		t.Fatalf("unreviewed prompt was cleared: %v", err)
	}
	review := harness.ToolReconciliation{RunID: r.RunID, ToolCallID: r.ToolCallID, ExpectedVersion: r.Version, Outcome: harness.ReceiptCompleted, Reviewer: "operator", Note: "Verified remote session transcript and effect."}
	if _, err := c.ReconcileToolReceipt(ctx, session.ID, review); err != nil {
		t.Fatal(err)
	}
	if err := c.AcknowledgeExternalPrompt(ctx, session.ID, "fixture", "wrong-id"); !errors.Is(err, harness.ErrReconciliationRequired) {
		t.Fatalf("wrong prompt ID was accepted: %v", err)
	}
	if err := c.AcknowledgeExternalPrompt(ctx, session.ID, "fixture", pending.PromptID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PendingExternalPrompt(ctx, session.ID, "fixture"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("acknowledged prompt remains pending: %v", err)
	}
}
