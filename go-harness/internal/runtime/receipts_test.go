package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func receiptFixture(t *testing.T) (*Service, harness.RunRequest) {
	t.Helper()
	s := newSessionTestService(t)
	x, err := s.NewSession(context.Background(), "owner", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	req := harness.RunRequest{Session: x, RunID: "fixture-run", InputID: "fixture-input", Input: []harness.Content{{Type: "text", Text: "go"}}}
	if err := s.Store.BeginRun(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	return s, req
}

func receiptEvent(req harness.RunRequest, kind, callID, name string) harness.RunEvent {
	e := harness.RunEvent{SessionID: req.Session.ID, RunID: req.RunID, Kind: kind, ToolCallID: callID, ToolName: name}
	switch kind {
	case "tool_start":
		e.Status, e.Arguments = "pending", json.RawMessage(`{"path":"secret-path","token":"secret-token","count":3}`)
	case "tool_execute", "tool_update":
		e.Status = "in_progress"
	case "tool_end":
		e.Status = "completed"
	}
	return e
}

func appendReceiptEvent(t *testing.T, s *Service, req harness.RunRequest, kind, callID, name string) harness.RunEvent {
	t.Helper()
	e, err := s.Store.Append(context.Background(), receiptEvent(req, kind, callID, name))
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestReceiptExecutionBoundaryAndAtomicEvents(t *testing.T) {
	ctx := context.Background()
	s, req := receiptFixture(t)
	// Failed event persistence must roll the new receipt back as well.
	if _, err := s.Store.db.Exec(`CREATE TRIGGER fail_events BEFORE INSERT ON harness_events BEGIN SELECT RAISE(ABORT,'fixture event failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.Append(ctx, receiptEvent(req, "tool_start", "call", "write_file")); err == nil {
		t.Fatal("event insert unexpectedly succeeded")
	}
	receipts, err := s.Store.ListToolReceipts(ctx, req.Session.ID)
	if err != nil || len(receipts) != 0 {
		t.Fatalf("non-atomic new receipt: %+v %v", receipts, err)
	}
	if _, err = s.Store.db.Exec(`DROP TRIGGER fail_events`); err != nil {
		t.Fatal(err)
	}
	start := appendReceiptEvent(t, s, req, "tool_start", "call", "write_file")
	if start.Receipt == nil || start.Receipt.State != harness.ReceiptPending || start.Receipt.ConfigVersion != req.Session.ConfigVersion || start.Receipt.Attempt != 1 {
		t.Fatalf("start=%+v", start)
	}
	if strings.Contains(start.Receipt.ArgumentsSummary, "secret") || start.Receipt.ArgumentsDigest != fmt.Sprintf("%x", sha256.Sum256(start.Arguments)) {
		t.Fatalf("unsafe/incorrect argument evidence: %+v", start.Receipt)
	}
	if _, err = s.Store.Append(ctx, receiptEvent(req, "tool_start", "call", "write_file")); !errors.Is(err, harness.ErrReceiptConflict) {
		t.Fatalf("duplicate start=%v", err)
	}
	if _, err = s.Store.db.Exec(`CREATE TRIGGER fail_events BEFORE INSERT ON harness_events BEGIN SELECT RAISE(ABORT,'fixture event failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Store.Append(ctx, receiptEvent(req, "tool_execute", "call", "write_file")); err == nil {
		t.Fatal("execution boundary unexpectedly succeeded")
	}
	r, err := readReceipt(ctx, s.Store.db, req.Session.ID, req.RunID, "call")
	if err != nil || r.State != harness.ReceiptPending || r.Version != 1 {
		t.Fatalf("non-atomic transition: %+v %v", r, err)
	}
	if _, err = s.Store.db.Exec(`DROP TRIGGER fail_events`); err != nil {
		t.Fatal(err)
	}
	execute := appendReceiptEvent(t, s, req, "tool_execute", "call", "write_file")
	if execute.Receipt.State != harness.ReceiptStarted {
		t.Fatal(execute.Receipt)
	}
	if _, err = s.Store.Append(ctx, receiptEvent(req, "tool_execute", "call", "write_file")); !errors.Is(err, harness.ErrReceiptConflict) {
		t.Fatalf("duplicate execution accepted: %v", err)
	}
	for range 3 {
		appendReceiptEvent(t, s, req, "tool_update", "call", "write_file")
	}
	end := receiptEvent(req, "tool_end", "call", "write_file")
	end.Content = []harness.Content{{Type: "resource_link", URI: "file:///workspace/result.txt"}}
	end, err = s.Store.Append(ctx, end)
	if err != nil || end.Receipt.State != harness.ReceiptCompleted || len(end.Receipt.Result) != 1 {
		t.Fatalf("end=%+v %v", end, err)
	}
	history, err := s.Store.History(ctx, req.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 7 || history[len(history)-1].Receipt.Version != end.Receipt.Version {
		t.Fatalf("receipt/event history diverged: %+v", history)
	}
}

func TestReceiptPermissionBindsArgumentsAndConfiguration(t *testing.T) {
	s, req := receiptFixture(t)
	if err := s.Store.Approval(context.Background(), harness.PermissionRequest{ID: "missing", SessionID: req.Session.ID, RunID: req.RunID, ToolCallID: "absent", ToolName: "write_file", ConfigVersion: req.Session.ConfigVersion, Arguments: json.RawMessage(`{}`)}); !errors.Is(err, harness.ErrReceiptConflict) {
		t.Fatalf("approval without durable intent: %v", err)
	}
	start := appendReceiptEvent(t, s, req, "tool_start", "call", "write_file")
	permission := harness.PermissionRequest{ID: "approval", SessionID: req.Session.ID, RunID: req.RunID, ToolCallID: "call", ToolName: "write_file", ConfigVersion: req.Session.ConfigVersion, Arguments: start.Arguments}
	for _, mismatch := range []string{"arguments", "configuration", "name"} {
		changed := permission
		switch mismatch {
		case "arguments":
			changed.Arguments = json.RawMessage(`{"path":"changed"}`)
		case "configuration":
			changed.ConfigVersion++
		case "name":
			changed.ToolName = "execute_command"
		}
		if err := s.Store.Approval(context.Background(), changed); !errors.Is(err, harness.ErrReceiptConflict) {
			t.Fatalf("%s mismatch: %v", mismatch, err)
		}
	}
	if err := s.Store.Approval(context.Background(), permission); err != nil {
		t.Fatal(err)
	}
}

func TestReceiptRecoveryAfterDatabaseReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	native, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(ctx, native.DB())
	if err != nil {
		native.Close()
		t.Fatal(err)
	}
	s := NewService(store, nil, "test")
	x, err := s.NewSession(ctx, "original", t.TempDir())
	if err != nil {
		native.Close()
		t.Fatal(err)
	}
	req := harness.RunRequest{Session: x, RunID: "interrupted", InputID: "input", Input: []harness.Content{{Type: "text", Text: "write"}}}
	if err = store.BeginRun(ctx, req); err != nil {
		native.Close()
		t.Fatal(err)
	}
	appendReceiptEvent(t, s, req, "tool_start", "effect", "write_file")
	appendReceiptEvent(t, s, req, "tool_execute", "effect", "write_file")
	if err = native.Close(); err != nil {
		t.Fatal(err)
	}
	// No final run or tool receipt was written before process state disappeared.
	native, err = sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	store, err = NewStore(ctx, native.DB())
	if err != nil {
		t.Fatal(err)
	}
	if err = store.ReconcileInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	restarted := NewService(store, testEngine(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		t.Error("interrupted tool was replayed")
		return harness.RunResult{}, nil
	}), "test")
	if _, err = restarted.Load(ctx, "new-owner", x.ID, x.CWD, false, nil); err != nil {
		t.Fatal(err)
	}
	receipts, err := restarted.ListToolReceipts(ctx, "new-owner", x.ID)
	if err != nil || len(receipts) != 1 || receipts[0].State != harness.ReceiptUncertain || receipts[0].ConfigVersion != x.ConfigVersion {
		t.Fatalf("reopened receipts=%+v error=%v", receipts, err)
	}
	if _, err := restarted.Run(ctx, "new-owner", x.ID, []harness.Content{{Type: "text", Text: "continue"}}, nil, nil); !errors.Is(err, harness.ErrReconciliationRequired) {
		t.Fatalf("restarted run=%v", err)
	}
}

func TestReceiptRecoveryDistinguishesUnexecutedAndUncertain(t *testing.T) {
	ctx := context.Background()
	s, req := receiptFixture(t)
	appendReceiptEvent(t, s, req, "tool_start", "pending", "write_file")
	appendReceiptEvent(t, s, req, "tool_start", "started", "write_file")
	appendReceiptEvent(t, s, req, "tool_execute", "started", "write_file")
	appendReceiptEvent(t, s, req, "tool_start", "local-read", "read_file")
	appendReceiptEvent(t, s, req, "tool_execute", "local-read", "read_file")
	appendReceiptEvent(t, s, req, "tool_start", "snapshot-read", "read_tool_output")
	appendReceiptEvent(t, s, req, "tool_execute", "snapshot-read", "read_tool_output")
	appendReceiptEvent(t, s, req, "tool_start", "mcp-read", "mcp/files/read_file")
	appendReceiptEvent(t, s, req, "tool_execute", "mcp-read", "mcp/files/read_file")
	if err := s.Store.ReconcileInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	want := map[string]harness.ReceiptState{"pending": harness.ReceiptNotExecuted, "started": harness.ReceiptUncertain, "local-read": harness.ReceiptNoEffect, "snapshot-read": harness.ReceiptNoEffect, "mcp-read": harness.ReceiptUncertain}
	receipts, err := s.ListToolReceipts(ctx, "owner", req.Session.ID)
	if err != nil || len(receipts) != len(want) {
		t.Fatalf("receipts=%+v %v", receipts, err)
	}
	for _, receipt := range receipts {
		if receipt.State != want[receipt.ToolCallID] {
			t.Fatalf("receipt=%+v", receipt)
		}
	}
	history, err := s.Store.History(ctx, req.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.ReconcileInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := s.Store.History(ctx, req.Session.ID)
	if err != nil || len(after) != len(history) {
		t.Fatalf("recovery repeated terminal events: %d -> %d (%v)", len(history), len(after), err)
	}
	engineCalls := 0
	s.Engine = testEngine(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		engineCalls++
		return harness.RunResult{}, nil
	})
	if _, err := s.Run(ctx, "owner", req.Session.ID, []harness.Content{{Type: "text", Text: "continue"}}, nil, nil); !errors.Is(err, harness.ErrReconciliationRequired) || engineCalls != 0 {
		t.Fatalf("uncertain effects replayed: %v calls=%d", err, engineCalls)
	}
}

func TestReceiptReviewRequiresOwnerIdleAndVersionAndNeverExecutes(t *testing.T) {
	ctx := context.Background()
	s, req := receiptFixture(t)
	appendReceiptEvent(t, s, req, "tool_start", "call", "execute_command")
	appendReceiptEvent(t, s, req, "tool_execute", "call", "execute_command")
	progress := receiptEvent(req, "tool_update", "call", "execute_command")
	progress.Content = []harness.Content{{Type: "text", Text: "partial operation result"}}
	if _, err := s.Store.Append(ctx, progress); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.Finish(ctx, req.RunID, "cancelled", context.Canceled); err != nil {
		t.Fatal(err)
	}
	r, err := readReceipt(ctx, s.Store.db, req.Session.ID, req.RunID, "call")
	if err != nil || r.State != harness.ReceiptUncertain {
		t.Fatalf("receipt=%+v %v", r, err)
	}
	review := harness.ToolReconciliation{RunID: req.RunID, ToolCallID: "call", ExpectedVersion: r.Version, Outcome: harness.ReceiptNoEffect, Reviewer: "operator", Note: "Verified the external destination; no change occurred."}
	if _, err := s.ListToolReceipts(ctx, "other", req.Session.ID); !errors.Is(err, harness.ErrNotAttached) {
		t.Fatalf("foreign query=%v", err)
	}
	if _, err := s.ReconcileToolReceipt(ctx, "other", req.Session.ID, review); !errors.Is(err, harness.ErrNotAttached) {
		t.Fatalf("foreign review=%v", err)
	}
	_, release, err := s.Coordinator.Begin(ctx, req.Session.ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileToolReceipt(ctx, "owner", req.Session.ID, review); !errors.Is(err, harness.ErrBusy) {
		t.Fatalf("busy review=%v", err)
	}
	release()
	if _, err = s.Store.db.Exec(`CREATE TRIGGER fail_review_event BEFORE INSERT ON harness_events BEGIN SELECT RAISE(ABORT,'fixture review failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileToolReceipt(ctx, "owner", req.Session.ID, review); err == nil {
		t.Fatal("review unexpectedly succeeded")
	}
	r, err = readReceipt(ctx, s.Store.db, req.Session.ID, req.RunID, "call")
	if err != nil || r.State != harness.ReceiptUncertain || r.Review != nil {
		t.Fatalf("review not atomic: %+v %v", r, err)
	}
	var audits int
	if err := s.Store.db.QueryRow(`SELECT COUNT(*) FROM harness_tool_reconciliations`).Scan(&audits); err != nil || audits != 0 {
		t.Fatalf("orphan review audit=%d %v", audits, err)
	}
	if _, err = s.Store.db.Exec(`DROP TRIGGER fail_review_event`); err != nil {
		t.Fatal(err)
	}
	engineCalls := 0
	s.Engine = testEngine(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		engineCalls++
		return harness.RunResult{}, nil
	})
	reconciled, err := s.ReconcileToolReceipt(ctx, "owner", req.Session.ID, review)
	if err != nil || reconciled.State != harness.ReceiptNoEffect || reconciled.Review == nil || reconciled.Review.Note != review.Note || engineCalls != 0 {
		t.Fatalf("review=%+v calls=%d error=%v", reconciled, engineCalls, err)
	}
	if len(reconciled.Result) != 1 || reconciled.Result[0].Text != "partial operation result" || reconciled.Error == "" {
		t.Fatalf("review destroyed original evidence: %+v", reconciled)
	}
	if _, err := s.ReconcileToolReceipt(ctx, "owner", req.Session.ID, review); !errors.Is(err, harness.ErrReceiptConflict) {
		t.Fatalf("stale review=%v", err)
	}
	if _, err := s.Run(ctx, "owner", req.Session.ID, []harness.Content{{Type: "text", Text: "new request"}}, nil, nil); err != nil || engineCalls != 1 {
		t.Fatalf("explicit new run blocked: %v calls=%d", err, engineCalls)
	}
}

func TestFailedToolAfterExecutionRemainsUncertain(t *testing.T) {
	ctx := context.Background()
	for _, started := range []bool{false, true} {
		t.Run(fmt.Sprint(started), func(t *testing.T) {
			s, req := receiptFixture(t)
			appendReceiptEvent(t, s, req, "tool_start", "call", "write_file")
			if started {
				appendReceiptEvent(t, s, req, "tool_execute", "call", "write_file")
			}
			end := receiptEvent(req, "tool_end", "call", "write_file")
			end.Status = "failed"
			end.Content = []harness.Content{{Type: "text", Text: "operation returned an error"}}
			end, err := s.Store.Append(ctx, end)
			want := harness.ReceiptNotExecuted
			if started {
				want = harness.ReceiptUncertain
			}
			if err != nil || end.Receipt.State != want {
				t.Fatalf("end=%+v error=%v", end, err)
			}
		})
	}
}

func TestStartedReceiptUsesOnlyExplicitTerminalEvidence(t *testing.T) {
	for _, test := range []struct {
		name     string
		evidence harness.ReceiptState
		want     harness.ReceiptState
	}{
		{"rejected", harness.ReceiptNotExecuted, harness.ReceiptNotExecuted},
		{"native_read", harness.ReceiptNoEffect, harness.ReceiptNoEffect},
		{"unknown_failure", "", harness.ReceiptUncertain},
		{"unsupported_claim", harness.ReceiptCompleted, harness.ReceiptUncertain},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, req := receiptFixture(t)
			appendReceiptEvent(t, s, req, "tool_start", "call", "execute")
			appendReceiptEvent(t, s, req, "tool_execute", "call", "execute")
			end := receiptEvent(req, "tool_end", "call", "execute")
			end.Status = "failed"
			end.Receipt = &harness.ToolReceipt{State: test.evidence, Error: "fixture failure"}
			result, err := s.Store.Append(context.Background(), end)
			if err != nil || result.Receipt.State != test.want || result.Receipt.Error != "fixture failure" {
				t.Fatalf("receipt=%+v err=%v", result.Receipt, err)
			}
			err = s.Store.requireReconciled(context.Background(), req.Session.ID)
			if errors.Is(err, harness.ErrReconciliationRequired) != (test.want == harness.ReceiptUncertain) {
				t.Fatalf("incorrect next-run gate: %v", err)
			}
		})
	}
}

func TestMigratedToolNamesDoNotCertifyUnknownEffects(t *testing.T) {
	for _, name := range []string{"ls", "glob", "grep", "web_search", "web_fetch", "image_search"} {
		t.Run(name, func(t *testing.T) {
			s, req := receiptFixture(t)
			appendReceiptEvent(t, s, req, "tool_start", "call", name)
			appendReceiptEvent(t, s, req, "tool_execute", "call", name)
			end := receiptEvent(req, "tool_end", "call", name)
			end.Status = "failed"
			result, err := s.Store.Append(context.Background(), end)
			if err != nil || result.Receipt.State != harness.ReceiptUncertain {
				t.Fatalf("same-name unknown tool downgraded: %+v %v", result.Receipt, err)
			}
		})
	}
}
