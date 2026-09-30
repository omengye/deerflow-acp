package eino

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

type receiptEffectTool struct {
	run func(context.Context) (string, error)
}

func (*receiptEffectTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return (&recordingTool{}).Info(ctx)
}
func (t *receiptEffectTool) InvokableRun(ctx context.Context, _ string, _ ...tool.Option) (string, error) {
	return t.run(ctx)
}

type receiptStreamTool struct{ firstErr, lateErr error }

func (*receiptStreamTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return (&recordingTool{}).Info(ctx)
}
func (t *receiptStreamTool) StreamableRun(context.Context, string, ...tool.Option) (*schema.StreamReader[string], error) {
	r, w := schema.Pipe[string](1)
	go func() {
		defer w.Close()
		w.Send("partial output", nil)
		if t.firstErr != nil {
			w.Send("", t.firstErr)
		}
		if t.lateErr != nil {
			w.Send("", t.lateErr)
		}
	}()
	return r, nil
}

func runtimeWithReceiptTool(t *testing.T, underlying tool.BaseTool) (*hr.Service, harness.Session, *sqlite.Store) {
	return runtimeWithReceiptInvocation(t, underlying, `{"value":"x"}`)
}

func runtimeWithReceiptInvocation(t *testing.T, underlying tool.BaseTool, args string) (*hr.Service, harness.Session, *sqlite.Store) {
	t.Helper()
	native, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = native.Close() })
	store, err := hr.NewStore(context.Background(), native.DB())
	if err != nil {
		t.Fatal(err)
	}
	info, err := underlying.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	model := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call == 0 {
			return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "call-1", Type: "function", Function: schema.FunctionCall{Name: info.Name, Arguments: args}}}}}), nil
		}
		return textStream("done"), nil
	}}
	e := newTestEngine(t, Config{ChatModel: model, Tools: []tool.BaseTool{underlying}})
	s := hr.NewService(store, e, "test")
	x, err := s.NewSession(context.Background(), "owner", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s, x, native
}

type namedReceiptEffectTool struct {
	receiptEffectTool
	name string
}

func (t *namedReceiptEffectTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	info, err := t.receiptEffectTool.Info(ctx)
	if info != nil {
		info.Name = t.name
	}
	return info, err
}

func TestTypedToolFailureEvidenceSurvivesEinoWrappers(t *testing.T) {
	cause := errors.New("fixture failure")
	for _, test := range []struct {
		name string
		err  error
		want harness.ReceiptState
	}{
		{"pre_execution", fmt.Errorf("adapter: %w", harness.MarkToolNotExecuted(cause)), harness.ReceiptNotExecuted},
		{"native_read", fmt.Errorf("adapter: %w", harness.MarkToolNoEffect(cause)), harness.ReceiptNoEffect},
		{"unknown", cause, harness.ReceiptUncertain},
		{"uncertainty_wins", errors.Join(harness.MarkToolNotExecuted(cause), harness.ErrCommandUncertain), harness.ReceiptUncertain},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, x, _ := runtimeWithReceiptTool(t, &receiptEffectTool{run: func(context.Context) (string, error) { return "", test.err }})
			_, _ = s.Run(context.Background(), "owner", x.ID, []harness.Content{{Type: "text", Text: "run"}}, nil, approveReceiptTool)
			receipts, err := s.ListToolReceipts(context.Background(), "owner", x.ID)
			if err != nil || len(receipts) != 1 || receipts[0].State != test.want {
				t.Fatalf("receipt=%+v err=%v", receipts, err)
			}
			_, err = s.Run(context.Background(), "owner", x.ID, []harness.Content{{Type: "text", Text: "continue"}}, nil, approveReceiptTool)
			if errors.Is(err, harness.ErrReconciliationRequired) != (test.want == harness.ReceiptUncertain) {
				t.Fatalf("incorrect next-prompt gate: %v", err)
			}
		})
	}
}

func TestSDKToolWithNativeReadOnlyNameRetainsUncertainEffects(t *testing.T) {
	for _, name := range []string{"ls", "glob", "grep", "web_search", "web_fetch", "image_search"} {
		t.Run(name, func(t *testing.T) {
			underlying := &namedReceiptEffectTool{name: name}
			s, x, _ := runtimeWithReceiptTool(t, underlying)
			output := filepath.Join(x.CWD, "actual-effect.txt")
			underlying.run = func(context.Context) (string, error) {
				if err := os.WriteFile(output, []byte("effect occurred"), 0600); err != nil {
					return "", err
				}
				return "", errors.New("confirmation failed after writing")
			}
			_, _ = s.Run(context.Background(), "owner", x.ID, []harness.Content{{Type: "text", Text: "run"}}, nil, approveReceiptTool)
			body, err := os.ReadFile(output)
			if err != nil || string(body) != "effect occurred" {
				t.Fatalf("fixture did not write: %q %v", body, err)
			}
			receipts, err := s.ListToolReceipts(context.Background(), "owner", x.ID)
			if err != nil || len(receipts) != 1 || receipts[0].State != harness.ReceiptUncertain {
				t.Fatalf("SDK effect incorrectly downgraded: %+v %v", receipts, err)
			}
		})
	}
}

func approveReceiptTool(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
	return harness.AllowOnce, nil
}

func TestReceiptCommitPrecedesEffectsAndFailureDoesNotReplay(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	underlying := &receiptEffectTool{}
	s, x, _ := runtimeWithReceiptTool(t, underlying)
	outputPath := filepath.Join(x.CWD, "side-effect.txt")
	confirmationErr := errors.New("confirmation failed after writing")
	underlying.run = func(ctx context.Context) (string, error) {
		calls.Add(1)
		receipts, err := s.Store.ListToolReceipts(ctx, x.ID)
		if err != nil || len(receipts) != 1 || receipts[0].State != harness.ReceiptStarted {
			t.Errorf("effect preceded receipt commit: %+v %v", receipts, err)
		}
		if err := os.WriteFile(outputPath, []byte("effect happened"), 0600); err != nil {
			return "", err
		}
		return "file was written", confirmationErr
	}
	_, _ = s.Run(ctx, "owner", x.ID, []harness.Content{{Type: "text", Text: "write"}}, nil, approveReceiptTool)
	data, err := os.ReadFile(outputPath)
	if err != nil || string(data) != "effect happened" || calls.Load() != 1 {
		t.Fatalf("effect=%q calls=%d error=%v", data, calls.Load(), err)
	}
	receipts, err := s.ListToolReceipts(ctx, "owner", x.ID)
	if err != nil || len(receipts) != 1 || receipts[0].State != harness.ReceiptUncertain || !strings.Contains(receipts[0].Error, confirmationErr.Error()) {
		t.Fatalf("effect failure receipt=%+v error=%v", receipts, err)
	}
	if _, err := s.Run(ctx, "owner", x.ID, []harness.Content{{Type: "text", Text: "continue"}}, nil, approveReceiptTool); !errors.Is(err, harness.ErrReconciliationRequired) || calls.Load() != 1 {
		t.Fatalf("unconfirmed effect replayed: %v calls=%d", err, calls.Load())
	}
}

func TestFailedExecutionReceiptCommitPreventsToolCall(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	s, x, native := runtimeWithReceiptTool(t, &receiptEffectTool{run: func(context.Context) (string, error) { calls.Add(1); return "effect", nil }})
	_, err := native.DB().Exec(`CREATE TRIGGER fail_execution_event BEFORE INSERT ON harness_events WHEN json_extract(CAST(NEW.event AS TEXT),'$.kind')='tool_execute' BEGIN SELECT RAISE(ABORT,'execution receipt unavailable'); END`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Run(ctx, "owner", x.ID, []harness.Content{{Type: "text", Text: "write"}}, nil, approveReceiptTool)
	if err == nil || calls.Load() != 0 {
		t.Fatalf("tool escaped failed transaction: calls=%d error=%v", calls.Load(), err)
	}
	receipts, err := s.ListToolReceipts(ctx, "owner", x.ID)
	if err != nil || len(receipts) != 1 || receipts[0].State != harness.ReceiptNotExecuted {
		t.Fatalf("receipts=%+v error=%v", receipts, err)
	}
}

func TestStreamingToolReceiptPreservesLateFailureAndOutput(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "late_failure"}[fail], func(t *testing.T) {
			ctx := context.Background()
			underlying := &receiptStreamTool{}
			if fail {
				underlying.firstErr, underlying.lateErr = errors.New("first stream error"), errors.New("late teardown failure")
			}
			s, x, _ := runtimeWithReceiptTool(t, underlying)
			_, runErr := s.Run(ctx, "owner", x.ID, []harness.Content{{Type: "text", Text: "stream"}}, nil, approveReceiptTool)
			if fail && !errors.Is(runErr, underlying.lateErr) {
				t.Fatalf("late failure discarded: %v", runErr)
			}
			if !fail && runErr != nil {
				t.Fatal(runErr)
			}
			receipts, err := s.ListToolReceipts(ctx, "owner", x.ID)
			want := harness.ReceiptCompleted
			if fail {
				want = harness.ReceiptUncertain
			}
			if err != nil || len(receipts) != 1 || receipts[0].State != want {
				t.Fatalf("receipts=%+v error=%v", receipts, err)
			}
			var result strings.Builder
			for _, part := range receipts[0].Result {
				result.WriteString(part.Text)
			}
			if !strings.Contains(result.String(), "partial output") {
				t.Fatalf("stream evidence lost: %+v", receipts[0])
			}
			if fail && !strings.Contains(receipts[0].Error, underlying.lateErr.Error()) {
				t.Fatalf("false completion before source teardown: %+v", receipts[0])
			}
		})
	}
}
