package eino

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
)

type steeringRunResult struct {
	result harness.RunResult
	err    error
}

func awaitSteeringSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(15 * time.Second):
		t.Fatal("execution did not reach fixture boundary")
	}
}
func awaitSteeringResult(t *testing.T, ch <-chan steeringRunResult) steeringRunResult {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(15 * time.Second):
		t.Fatal("execution did not finish")
		return steeringRunResult{}
	}
}

func steeringToolMessage(id string) *schema.StreamReader[*schema.Message] {
	return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: id, Type: "function", Function: schema.FunctionCall{Name: "read_workspace", Arguments: `{"value":"x"}`}}}}})
}

func TestSteeringAppliesAtNextModelInputAndSurvivesReload(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	fake := &scriptedModel{stream: func(_ context.Context, call int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call < 2 {
			return steeringToolMessage(fmt.Sprintf("tool-%d", call)), nil
		}
		return textStream("done"), nil
	}}
	s, session, native := runtimeWithDurableInteractionMode(t, fake, &receiptEffectTool{run: func(ctx context.Context) (string, error) {
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		return "read", nil
	}}, false, harness.PermissionModeAll)
	var mu sync.Mutex
	notified := map[string]bool{}
	done := make(chan steeringRunResult, 1)
	go func() {
		result, err := s.Run(ctx, "owner", session.ID, []harness.Content{{Type: "text", Text: "original"}}, func(_ context.Context, event harness.RunEvent) error {
			if event.Steering != nil {
				mu.Lock()
				notified[event.Steering.Status] = true
				mu.Unlock()
			}
			return nil
		}, approveReceiptTool)
		done <- steeringRunResult{result, err}
	}()
	awaitSteeringSignal(t, started)
	state, err := s.Execution(ctx, "owner", session.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	req := harness.SteeringRequest{RunID: state.RunID, ExpectedVersion: state.Version, RequestID: "correct-1", Text: "Stop broad exploration and inspect only config.go."}
	accepted, err := s.SubmitSteering(ctx, "owner", session.ID, req)
	if err != nil || accepted.Status != "accepted" {
		t.Fatalf("accept=%+v err=%v", accepted, err)
	}
	again, err := s.SubmitSteering(ctx, "owner", session.ID, req)
	if err != nil || again.Sequence != accepted.Sequence {
		t.Fatalf("duplicate changed identity: %+v %v", again, err)
	}
	changed := req
	changed.Text = "different"
	if _, err = s.SubmitSteering(ctx, "owner", session.ID, changed); !errors.Is(err, harness.ErrExecutionConflict) {
		t.Fatalf("idempotency key rebound: %v", err)
	}
	if _, err = s.SubmitSteering(ctx, "other-owner", session.ID, req); !errors.Is(err, harness.ErrNotAttached) {
		t.Fatalf("unowned steering accepted: %v", err)
	}
	stale := req
	stale.RequestID = "stale"
	stale.ExpectedVersion++
	if rejected, err := s.SubmitSteering(ctx, "owner", session.ID, stale); err != nil || rejected.Status != "rejected" || rejected.Reason != "execution_changed" {
		t.Fatalf("stale=%+v %v", rejected, err)
	}
	close(release)
	finished := awaitSteeringResult(t, done)
	if finished.err != nil || finished.result.StopReason != "end_turn" {
		t.Fatalf("finish=%+v err=%v", finished.result, finished.err)
	}
	fake.mu.Lock()
	seen := append([][]*schema.Message(nil), fake.seen...)
	fake.mu.Unlock()
	if len(seen) != 3 {
		t.Fatalf("model calls=%d", len(seen))
	}
	for i, input := range seen {
		count := 0
		for _, message := range input {
			if message.Role == schema.User && message.Content == req.Text {
				count++
			}
		}
		want := 0
		if i > 0 {
			want = 1
		}
		if count != want {
			t.Fatalf("model input %d has %d copies, want %d", i, count, want)
		}
	}
	mu.Lock()
	applied := notified["applied"]
	mu.Unlock()
	if !applied {
		t.Fatal("applied notification missing")
	}
	// A new runtime/coordinator recovers status from the database, without
	// depending on the original process's cursor or notification delivery.
	reloadedStore, err := hr.NewStore(ctx, native.DB())
	if err != nil {
		t.Fatal(err)
	}
	reloadedStore.BudgetLedger, reloadedStore.BudgetLimits = s.Store.BudgetLedger, s.Store.BudgetLimits
	reloaded := hr.NewService(reloadedStore, s.Engine, "test")
	if _, err = reloaded.Load(ctx, "reconnected", session.ID, session.CWD, false, nil); err != nil {
		t.Fatal(err)
	}
	list, err := reloaded.ListSteering(ctx, "reconnected", session.ID, "")
	if err != nil || len(list.Items) != 2 || list.Items[0].Status != "applied" || list.Items[1].Status != "rejected" {
		t.Fatalf("reload=%+v %v", list, err)
	}
	req.RequestID = "too-late"
	if late, err := reloaded.SubmitSteering(ctx, "reconnected", session.ID, req); err != nil || late.Status != "rejected" {
		t.Fatalf("ended run accepted steering: %+v %v", late, err)
	}
}

func TestSteeringDuringFinalStreamIsRejectedWithoutNewTurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	started, release := make(chan struct{}), make(chan struct{})
	fake := &scriptedModel{stream: func(ctx context.Context, _ int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return textStream("done"), nil
	}}
	s, session, _ := runtimeWithDurableInteractionMode(t, fake, &receiptEffectTool{run: func(context.Context) (string, error) { return "unused", nil }}, false, harness.PermissionModeOff)
	done := make(chan steeringRunResult, 1)
	go func() {
		result, err := s.Run(ctx, "owner", session.ID, []harness.Content{{Type: "text", Text: "finish"}}, nil, nil)
		done <- steeringRunResult{result, err}
	}()
	awaitSteeringSignal(t, started)
	state, err := s.Execution(ctx, "owner", session.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.SubmitSteering(ctx, "owner", session.ID, harness.SteeringRequest{RunID: state.RunID, ExpectedVersion: state.Version, RequestID: "late-stream", Text: "new constraint"})
	if err != nil || item.Status != "accepted" {
		t.Fatalf("accept=%+v %v", item, err)
	}
	close(release)
	finished := awaitSteeringResult(t, done)
	if finished.err != nil {
		t.Fatal(finished.err)
	}
	list, err := s.ListSteering(ctx, "owner", session.ID, state.RunID)
	if err != nil || len(list.Items) != 1 || list.Items[0].Status != "rejected" || list.Items[0].Text != "new constraint" {
		t.Fatalf("unapplied text not retained: %+v %v", list, err)
	}
	fake.mu.Lock()
	count := fake.calls
	fake.mu.Unlock()
	if count != 1 {
		t.Fatalf("steering silently started a replacement turn: %d", count)
	}
}

func TestSteeringWaitingApprovalDoesNotGrantPermission(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	permissionReached, release := make(chan struct{}), make(chan struct{})
	var effects atomic.Int32
	fake := toolScript()
	s, session, _ := runtimeWithDurableInteractionMode(t, fake, &receiptEffectTool{run: func(context.Context) (string, error) { effects.Add(1); return "effect", nil }}, false, harness.PermissionModeAll)
	done := make(chan steeringRunResult, 1)
	go func() {
		result, err := s.Run(ctx, "owner", session.ID, []harness.Content{{Type: "text", Text: "inspect"}}, nil, func(ctx context.Context, _ harness.PermissionRequest) (harness.PermissionDecision, error) {
			close(permissionReached)
			select {
			case <-release:
				return harness.RejectOnce, nil
			case <-ctx.Done():
				return harness.PermissionCancelled, ctx.Err()
			}
		})
		done <- steeringRunResult{result, err}
	}()
	awaitSteeringSignal(t, permissionReached)
	state, err := s.Execution(ctx, "owner", session.ID, "")
	if err != nil || state.Status != harness.ExecutionWaitingInput {
		t.Fatalf("state=%+v %v", state, err)
	}
	item, err := s.SubmitSteering(ctx, "owner", session.ID, harness.SteeringRequest{RunID: state.RunID, ExpectedVersion: state.Version, RequestID: "waiting", Text: "Do not execute this tool; explain the findings."})
	if err != nil || item.Status != "accepted" || effects.Load() != 0 {
		t.Fatalf("waiting steering=%+v effects=%d %v", item, effects.Load(), err)
	}
	close(release)
	finished := awaitSteeringResult(t, done)
	if finished.err != nil || effects.Load() != 0 {
		t.Fatalf("steering execution: %+v err=%v effects=%d", finished.result, finished.err, effects.Load())
	}
	list, err := s.ListSteering(ctx, "owner", session.ID, state.RunID)
	if err != nil || len(list.Items) != 1 || list.Items[0].Status != "applied" {
		t.Fatalf("resumed directive missing: %+v %v", list, err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	count := 0
	for _, message := range fake.seen[len(fake.seen)-1] {
		if message.Content == item.Text {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("directive count after permission resume=%d", count)
	}
}

func TestSteeringCancelledBeforeNextModelIsRejectedAndNotified(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	permissionReached := make(chan struct{})
	fake := toolScript()
	s, session, _ := runtimeWithDurableInteractionMode(t, fake, &receiptEffectTool{run: func(context.Context) (string, error) {
		t.Error("cancelled permission executed its tool")
		return "effect", nil
	}}, false, harness.PermissionModeAll)
	notified := make(chan harness.SteeringRecord, 8)
	done := make(chan steeringRunResult, 1)
	go func() {
		result, err := s.Run(ctx, "owner", session.ID, []harness.Content{{Type: "text", Text: "inspect"}}, func(_ context.Context, event harness.RunEvent) error {
			if event.Steering != nil {
				notified <- *event.Steering
			}
			return nil
		}, func(ctx context.Context, _ harness.PermissionRequest) (harness.PermissionDecision, error) {
			close(permissionReached)
			<-ctx.Done()
			return harness.PermissionCancelled, ctx.Err()
		})
		done <- steeringRunResult{result, err}
	}()
	awaitSteeringSignal(t, permissionReached)
	state, err := s.Execution(ctx, "owner", session.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	item, err := s.SubmitSteering(ctx, "owner", session.ID, harness.SteeringRequest{RunID: state.RunID, ExpectedVersion: state.Version, RequestID: "cancel-before-safe-point", Text: "retain this instruction for review"})
	if err != nil || item.Status != "accepted" {
		t.Fatalf("accept: %+v %v", item, err)
	}
	if err = s.Cancel(ctx, "owner", session.ID); err != nil {
		t.Fatal(err)
	}
	awaitSteeringResult(t, done)
	list, err := s.ListSteering(ctx, "owner", session.ID, "")
	if err != nil || len(list.Items) != 1 || list.Items[0].Status != "rejected" || list.Items[0].Text != item.Text {
		t.Fatalf("cancel discarded instruction/status: %+v %v", list, err)
	}
	select {
	case event := <-notified:
		if event.Status != "rejected" || event.SessionID != session.ID || event.RunID != state.RunID {
			t.Fatalf("wrong cancellation notification: %+v", event)
		}
	default:
		t.Fatal("rejected notification missing after cancellation")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.calls != 1 {
		t.Fatalf("cancellation triggered another model request: %d", fake.calls)
	}
}
