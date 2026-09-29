package deerflow

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
)

type continuationACPConnection struct {
	peer       *protocol.Peer
	serverDone chan error
	clientDone chan error
	closeOnce  sync.Once
}

func newContinuationACPConnection(t *testing.T, f *continuationFixture, handler protocol.Handler) *continuationACPConnection {
	t.Helper()
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	c := &continuationACPConnection{peer: protocol.NewPeer(clientIn, clientOut, handler, protocol.Options{}), serverDone: make(chan error, 1), clientDone: make(chan error, 1)}
	go func() { c.serverDone <- f.c.ServeACP(f.ctx, serverIn, serverOut) }()
	go func() { c.clientDone <- c.peer.Serve(f.ctx) }()
	t.Cleanup(func() { c.close(t) })
	var init json.RawMessage
	if err := c.peer.Call(f.ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}, &init); err != nil || !strings.Contains(string(init), `"processMethod":"_deerflow/notifications/process"`) {
		t.Fatalf("real continuation capability=%s err=%v", init, err)
	}
	if err := c.peer.Call(f.ctx, "session/load", map[string]any{"sessionId": f.sess.ID, "cwd": f.sess.CWD, "mcpServers": []any{}}, nil); err != nil {
		t.Fatal(err)
	}
	return c
}

func (c *continuationACPConnection) close(t *testing.T) {
	t.Helper()
	c.closeOnce.Do(func() {
		_ = c.peer.Close()
		// Serve completion includes handler cleanup and owner detachment; Done
		// alone cannot authorize a replacement ACP connection.
		for _, done := range []chan error{c.clientDone, c.serverDone} {
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.ErrClosedPipe) {
					t.Error(err)
				}
			case <-time.After(10 * time.Second):
				t.Error("continuation ACP connection did not join")
			}
		}
	})
}

type continuationACPResponse struct {
	StopReason string `json:"stopReason"`
	Meta       struct {
		Deerflow struct {
			Execution *harness.ExecutionState `json:"execution"`
		} `json:"deerflow"`
	} `json:"_meta"`
}

func TestACPRealNotificationContinuationDisconnectResumeAndDeduplicate(t *testing.T) {
	f := newContinuationFixture(t, true)
	if err := f.c.CloseSession(f.ctx, f.sess.ID); err != nil {
		t.Fatal(err)
	}
	permissionStarted := make(chan json.RawMessage, 1)
	var updates, permissionCalls atomic.Int32
	first := newContinuationACPConnection(t, f, func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
		if method == "session/update" {
			updates.Add(1)
			return nil, nil
		}
		if method != "session/request_permission" {
			return nil, &protocol.Error{Code: protocol.MethodNotFound, Message: "unexpected callback"}
		}
		permissionCalls.Add(1)
		select {
		case permissionStarted <- append(json.RawMessage(nil), raw...):
		case <-ctx.Done():
		}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	params := map[string]any{"sessionId": f.sess.ID, "notificationId": f.note.ID}
	processDone := make(chan error, 1)
	go func() { processDone <- first.peer.Call(f.ctx, "_deerflow/notifications/process", params, nil) }()
	var rawPermission json.RawMessage
	select {
	case rawPermission = <-permissionStarted:
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
	assertPermission := func(raw json.RawMessage) {
		t.Helper()
		var p struct {
			SessionID string `json:"sessionId"`
			ToolCall  struct {
				ID       string            `json:"toolCallId"`
				Title    string            `json:"title"`
				RawInput map[string]string `json:"rawInput"`
			} `json:"toolCall"`
		}
		if err := json.Unmarshal(raw, &p); err != nil || p.SessionID != f.sess.ID || p.ToolCall.ID != "continuation-write" || p.ToolCall.Title != "write_file" || len(p.ToolCall.RawInput) != 2 || p.ToolCall.RawInput["path"] != "continued.txt" || p.ToolCall.RawInput["content"] != "one continuation effect" {
			t.Errorf("continuation ACP permission=%s err=%v", raw, err)
		}
	}
	assertPermission(rawPermission)
	var before harness.ExecutionState
	if err := first.peer.Call(f.ctx, "_deerflow/executions/get", map[string]any{"sessionId": f.sess.ID}, &before); err != nil || before.Status != harness.ExecutionWaitingInput || !before.Resumable || len(before.WaitingInputs) != 1 || before.RunID == f.parent.Execution.RunID || before.InputID == f.parent.Execution.InputID {
		t.Fatalf("waiting continuation=%+v err=%v", before, err)
	}
	file := filepath.Join(f.sess.CWD, "continued.txt")
	assertNoEffect := func() {
		t.Helper()
		if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("continuation wrote before ACP approval: %v", err)
		}
	}
	assertNoEffect()
	first.close(t)
	select {
	case err := <-processDone:
		if err == nil {
			t.Fatal("disconnected process unexpectedly returned a response")
		}
	case <-f.ctx.Done():
		t.Fatal("disconnected process did not finish")
	}
	assertNoEffect()
	modelCalls := f.calls.Load()
	second := newContinuationACPConnection(t, f, func(_ context.Context, method string, raw json.RawMessage) (any, error) {
		if method == "session/update" {
			updates.Add(1)
			return nil, nil
		}
		if method != "session/request_permission" {
			return nil, &protocol.Error{Code: protocol.MethodNotFound, Message: "unexpected callback"}
		}
		permissionCalls.Add(1)
		assertPermission(raw)
		return map[string]any{"outcome": map[string]string{"outcome": "selected", "optionId": "allow_once"}}, nil
	})
	var repeated continuationACPResponse
	if err := second.peer.Call(f.ctx, "_deerflow/notifications/process", params, &repeated); err != nil {
		t.Fatal(err)
	}
	waiting := repeated.Meta.Deerflow.Execution
	if waiting == nil || waiting.RunID != before.RunID || waiting.InputID != before.InputID || waiting.Status != harness.ExecutionWaitingInput || !waiting.Resumable || f.calls.Load() != modelCalls || f.models.Load() != 1 || permissionCalls.Load() != 1 {
		t.Fatalf("duplicate process replayed waiting continuation: %+v models=%d approvals=%d", repeated, f.models.Load(), permissionCalls.Load())
	}
	assertNoEffect()
	resumeParams := map[string]any{"sessionId": f.sess.ID, "runId": waiting.RunID, "expectedVersion": waiting.Version - 1}
	err := second.peer.Call(f.ctx, "_deerflow/executions/resume", resumeParams, nil)
	var wire *protocol.Error
	if !errors.As(err, &wire) || wire.Code != -32013 || f.calls.Load() != modelCalls || permissionCalls.Load() != 1 {
		t.Fatalf("stale resume version=%v models=%d approvals=%d", err, f.calls.Load(), permissionCalls.Load())
	}
	resumeParams["expectedVersion"] = waiting.Version
	var completed continuationACPResponse
	if err := second.peer.Call(f.ctx, "_deerflow/executions/resume", resumeParams, &completed); err != nil {
		t.Fatal(err)
	}
	state := completed.Meta.Deerflow.Execution
	if completed.StopReason != "end_turn" || state == nil || state.Status != harness.ExecutionCompleted || state.RunID != before.RunID || state.InputID != before.InputID || permissionCalls.Load() != 2 || f.models.Load() != 2 {
		t.Fatalf("resumed continuation=%+v approvals=%d models=%d", completed, permissionCalls.Load(), f.models.Load())
	}
	data, err := os.ReadFile(file)
	if err != nil || string(data) != "one continuation effect" {
		t.Fatalf("resumed continuation effect=%q err=%v", data, err)
	}
	modelCalls = f.calls.Load()
	if err := second.peer.Call(f.ctx, "_deerflow/notifications/process", params, &repeated); err != nil || repeated.Meta.Deerflow.Execution == nil || repeated.Meta.Deerflow.Execution.RunID != before.RunID || repeated.Meta.Deerflow.Execution.Status != harness.ExecutionCompleted || f.calls.Load() != modelCalls || permissionCalls.Load() != 2 {
		t.Fatalf("completed process replayed: %+v err=%v models=%d approvals=%d", repeated, err, f.calls.Load(), permissionCalls.Load())
	}
	var root, acceptedInput string
	if err := f.c.store.DB().QueryRowContext(f.ctx, "SELECT root_budget_id,input_id FROM harness_executions WHERE run_id=?", before.RunID).Scan(&root, &acceptedInput); err != nil || root != f.parent.Execution.RunID || acceptedInput != before.InputID {
		t.Fatalf("continuation identity root=%q input=%q err=%v", root, acceptedInput, err)
	}
	budget, err := f.c.budgets.Snapshot(f.ctx, root)
	if err != nil || budget.ModelCalls != 5 || budget.ToolCalls != 2 || budget.HeldTokens != 0 {
		t.Fatalf("continuation budget=%+v err=%v", budget, err)
	}
	if updates.Load() == 0 {
		t.Fatal("real continuation emitted no standard session/update")
	}
}
