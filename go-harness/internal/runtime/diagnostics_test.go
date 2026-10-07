package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestSessionDiagnosticsIsBoundedAndExcludesContent(t *testing.T) {
	ctx := context.Background()
	native, err := sqlite.Open(filepath.Join(t.TempDir(), "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	store, err := NewStore(ctx, native.DB())
	if err != nil {
		t.Fatal(err)
	}
	engine := testEngine(func(ctx context.Context, r harness.RunRequest, emit harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		for i := 0; i < 20; i++ {
			if err := emit(ctx, harness.RunEvent{Kind: "model_timing", Status: "completed", Text: "private-response", Timing: &harness.ModelTiming{StartedAt: time.Now(), DurationMS: 42}}); err != nil {
				return harness.RunResult{}, err
			}
		}
		return harness.RunResult{StopReason: "end_turn"}, emit(ctx, harness.RunEvent{Kind: "text_delta", Text: "private-response"})
	})
	service := NewService(store, engine, "fixture")
	session, err := service.NewSession(ctx, "owner", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Run(ctx, "owner", session.ID, []harness.Content{{Type: "text", Text: "private-prompt"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	diag, err := store.SessionDiagnostics(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(diag)
	if len(diag["model_timings"].([]any)) != 16 || strings.Contains(string(encoded), "private-") || diag["status"] != "completed" {
		t.Fatalf("unsafe diagnostics %s", encoded)
	}
	if diag["spent_tokens"] != nil {
		t.Fatal("historical usage was invented")
	}
	queue, _ := newRunQueue(RunQueueConfig{MaxActiveRuns: 1})
	service.runQueue = queue
	release, err := queue.acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	wait, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { _, err := queue.acquire(wait, session.ID); done <- err }()
	deadline := time.Now().Add(time.Second)
	for !queue.isWaiting(session.ID) && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	diag, err = service.SessionDiagnostics(ctx, session.ID)
	cancel()
	<-done
	if err != nil || diag["phase"] != "queued" || queue.isWaiting(session.ID) {
		t.Fatalf("queue diagnostics=%v err=%v", diag, err)
	}
}
