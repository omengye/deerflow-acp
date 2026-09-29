package deerflow

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSDKReceiptRecoveryRequiresLoadAndExplicitReview(t *testing.T) {
	ctx := context.Background()
	var effects atomic.Int32
	engine := engineFunc(func(ctx context.Context, req harness.RunRequest, emit harness.EventHandler, permission harness.PermissionHandler) (harness.RunResult, error) {
		if req.Input[0].Text == "new request" {
			return harness.RunResult{StopReason: "end_turn"}, nil
		}
		args := json.RawMessage(`{"path":"result.txt"}`)
		if err := emit(ctx, harness.RunEvent{Kind: "tool_start", ToolCallID: "write", ToolName: "write_file", Status: "pending", Arguments: args}); err != nil {
			return harness.RunResult{}, err
		}
		decision, err := permission(ctx, harness.PermissionRequest{ToolCallID: "write", ToolName: "write_file", Arguments: args})
		if err != nil {
			return harness.RunResult{}, err
		}
		if decision != harness.AllowOnce {
			return harness.RunResult{}, harness.ErrPermissionDenied
		}
		if err := emit(ctx, harness.RunEvent{Kind: "tool_execute", ToolCallID: "write", ToolName: "write_file", Status: "in_progress"}); err != nil {
			return harness.RunResult{}, err
		}
		effects.Add(1)
		return harness.RunResult{}, errors.New("provider confirmation unavailable after effect")
	})
	cfg := Config{DataDir: t.TempDir(), Engine: engine}
	c, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	x, err := c.NewSession(ctx, cwd)
	if err != nil {
		c.Close()
		t.Fatal(err)
	}
	if _, err := c.Run(ctx, x.ID, []harness.Content{{Type: "text", Text: "write"}}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		return harness.AllowOnce, nil
	}); err == nil {
		c.Close()
		t.Fatal("fixture should have failed after effect")
	}
	receipts, err := c.ListToolReceipts(ctx, x.ID)
	if err != nil || len(receipts) != 1 || receipts[0].State != harness.ReceiptUncertain {
		c.Close()
		t.Fatalf("receipts=%+v error=%v", receipts, err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListToolReceipts(ctx, x.ID); err == nil {
		t.Fatal("closed SDK accepted receipt query")
	}
	reopened, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	r := receipts[0]
	review := harness.ToolReconciliation{RunID: r.RunID, ToolCallID: r.ToolCallID, ExpectedVersion: r.Version, Outcome: harness.ReceiptCompleted, Reviewer: "operator", Note: "Verified result.txt exists with the intended contents.", Result: []harness.Content{{Type: "resource_link", URI: "file:///workspace/result.txt"}}}
	if _, err := reopened.ListToolReceipts(ctx, x.ID); !errors.Is(err, harness.ErrNotAttached) {
		t.Fatalf("query before load=%v", err)
	}
	if _, err := reopened.ReconcileToolReceipt(ctx, x.ID, review); !errors.Is(err, harness.ErrNotAttached) {
		t.Fatalf("review before load=%v", err)
	}
	if _, err := reopened.LoadSession(ctx, x.ID, cwd, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Run(ctx, x.ID, []harness.Content{{Type: "text", Text: "new request"}}, nil, nil); !errors.Is(err, harness.ErrReconciliationRequired) {
		t.Fatalf("SDK skipped unresolved effect: %v", err)
	}
	reconciled, err := reopened.ReconcileToolReceipt(ctx, x.ID, review)
	if err != nil || reconciled.State != harness.ReceiptCompleted || reconciled.Review == nil || reconciled.Review.Note != review.Note || effects.Load() != 1 {
		t.Fatalf("reconcile=%+v effects=%d error=%v", reconciled, effects.Load(), err)
	}
	if _, err := reopened.ReconcileToolReceipt(ctx, x.ID, review); !errors.Is(err, harness.ErrReceiptConflict) {
		t.Fatalf("stale SDK review=%v", err)
	}
	if _, err := reopened.Run(ctx, x.ID, []harness.Content{{Type: "text", Text: "new request"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if effects.Load() != 1 {
		t.Fatal("SDK review re-executed a tool")
	}
	if err := reopened.CloseSession(ctx, x.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.ListToolReceipts(ctx, x.ID); !errors.Is(err, harness.ErrNotAttached) {
		t.Fatalf("closed session query=%v", err)
	}
}

func TestSDKReceiptMethodsRespectActiveRun(t *testing.T) {
	started := make(chan struct{})
	engine := engineFunc(func(ctx context.Context, _ harness.RunRequest, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		close(started)
		<-ctx.Done()
		return harness.RunResult{}, ctx.Err()
	})
	c, err := Open(context.Background(), Config{DataDir: t.TempDir(), Engine: engine})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	x, err := c.NewSession(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := c.Run(context.Background(), x.ID, []harness.Content{{Type: "text", Text: "wait"}}, nil, nil)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not start")
	}
	if _, err := c.ListToolReceipts(context.Background(), x.ID); !errors.Is(err, harness.ErrBusy) {
		t.Fatalf("active query=%v", err)
	}
	if _, err := c.ReconcileToolReceipt(context.Background(), x.ID, harness.ToolReconciliation{}); !errors.Is(err, harness.ErrBusy) {
		t.Fatalf("active review=%v", err)
	}
	if err := c.Cancel(x.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not finish")
	}
}
