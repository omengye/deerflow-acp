package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestMemoryStage1SyntheticInputsDoNotCaptureHumanAuthority(t *testing.T) {
	rejectCapture := func(context.Context, *sql.Tx, harness.RunRequest, int64, []byte) error {
		return errors.New("synthetic input attempted to capture human memory authority")
	}
	t.Run("delegated_child", func(t *testing.T) {
		f := newBackgroundHostFixture(t)
		f.store.CaptureMemoryInput = rejectCapture
		if err := f.create(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("background_continuation", func(t *testing.T) {
		f := newBackgroundTargetFixture(t, false)
		f.completeParent(t)
		f.owner.store.CaptureMemoryInput = rejectCapture
		ctx := context.WithValue(t.Context(), automaticNotificationKey{}, true)
		c, existing, err := f.service.PrepareBackgroundContinuation(ctx, "owner", f.owner.binding.RootSession(), f.note.ID)
		if err != nil || c == nil || existing != "" {
			t.Fatalf("prepare continuation: existing=%q err=%v", existing, err)
		}
		if err = admitContinuationFixture(ctx, f, c); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("foreground_notification", func(t *testing.T) {
		f := newExecutionWaitFixture(t)
		s := automaticService(t, f, &automaticWaitEngine{})
		s.ContinuationHostPolicy = f.child.spec.HostPolicy
		ctx := t.Context()
		if _, err := s.finishExecutionAttempt(ctx, f.lease, harness.RunResult{StopReason: "end_turn"}, nil, nil); err != nil {
			t.Fatal(err)
		}
		f.complete(t)
		task, err := f.child.tasks.Get(ctx, f.child.binding.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		n := bt.Notification{ID: "memory-source-note", TaskID: task.Spec.ID, SessionID: f.req.Session.ID, Kind: bt.NotificationCompleted, Version: task.Version, CreatedAt: time.Now().UTC()}
		raw, err := json.Marshal(n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.s.db.Exec("INSERT INTO harness_background_inbox(notification_id,parent_session_id,task_id,payload) VALUES(?,?,?,?)", n.ID, n.SessionID, n.TaskID, raw); err != nil {
			t.Fatal(err)
		}
		f.s.CaptureMemoryInput = rejectCapture
		mustExecutionTx(t, f.executionFixture, func(tx *sql.Tx) error {
			req, _, existing, err := s.beginContinuationTx(ctx, tx, "owner", n.SessionID, n.ID)
			if err == nil && (req.RunID == "" || existing != "") {
				return errors.New("notification fixture did not accept a new continuation")
			}
			return err
		})
	})
}
