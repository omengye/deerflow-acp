package deerflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/channels"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

// The real channel worker and Eino runtime share a temporary database. The
// provider, channel transport and write target are local test fixtures.
func TestWeChatTaskWaitFinishesOnceWithEitherOwner(t *testing.T) {
	for _, owner := range []string{"wechat", "desktop", "restart", "permission_changed"} {
		t.Run(owner, func(t *testing.T) {
			gate := make(chan struct{})
			releaseChild := sync.OnceFunc(func() { close(gate) })
			f := automaticUpdatesFixture(t, false, gate)
			defer releaseChild()
			d := setupChannelPluginFixture(t, f.c, f.sess.CWD, "wechat")
			if _, err := f.c.channels.SetPermissions(f.ctx, d.Instance, d.Revision, harness.ApprovalAllowAlways); err != nil {
				t.Fatal(err)
			}
			db := f.c.store.DB()
			tx, err := db.BeginTx(f.ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err = tx.Exec(`INSERT INTO harness_channel_messages(instance,account,id,conversation,sender,context,text,status,client_id,workspace,created_ms)
VALUES(?,?,'question',?,?,'reply-token','delegate then write','queued','final-answer',?,1)`, d.Instance, d.Account, d.Conversation, d.Sender, d.Workspace); err != nil {
				t.Fatal(err)
			}
			if _, err = tx.Exec(`INSERT INTO harness_channel_admissions VALUES(?,?,'question',?)`, d.Instance, d.Account, harness.ApprovalAllowAlways); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(); err != nil {
				t.Fatal(err)
			}
			var sid, rid, status, reply string
			until := func(want string) {
				t.Helper()
				for {
					err := db.QueryRowContext(f.ctx, `SELECT status,reply FROM harness_channel_messages WHERE instance=? AND account=? AND id='question'`, d.Instance, d.Account).Scan(&status, &reply)
					if err != nil {
						t.Fatal(status, reply, err)
					}
					if status == want {
						return
					}
					if status == "unknown" || status == "ignored" || want == "waiting_task" && status == "sent" {
						t.Fatalf("expected %s, got %s %s", want, status, reply)
					}
					select {
					case <-f.ctx.Done():
						t.Fatalf("expected %s, still %s %s", want, status, reply)
					case <-time.After(20 * time.Millisecond):
					}
				}
			}
			until("waiting_task")
			if err = db.QueryRow(`SELECT session_id,run_id FROM harness_channel_runs WHERE instance=? AND account=? AND id='question'`, d.Instance, d.Account).Scan(&sid, &rid); err != nil {
				t.Fatal(err)
			}
			before, err := f.c.service.Store.Execution(f.ctx, sid, rid)
			if err != nil || len(before.WaitingTasks) != 1 || before.Resumable {
				t.Fatalf("root wait %+v, %v", before, err)
			}
			var checkpoint string
			if err = db.QueryRow(`SELECT json_extract(CAST(manifest AS TEXT),'$.CheckpointSHA') FROM harness_executions WHERE run_id=?`, rid).Scan(&checkpoint); err != nil || checkpoint == "" {
				t.Fatal("waiting checkpoint", err)
			}
			if owner == "desktop" {
				for {
					_, err = f.c.LoadSession(f.ctx, sid, d.Workspace, false, nil)
					if !errors.Is(err, harness.ErrAttachedElsewhere) {
						break
					}
					select {
					case <-f.ctx.Done():
						t.Fatal(err)
					case <-time.After(20 * time.Millisecond):
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				live, cancel := context.WithCancel(interaction.WithUserInput(f.ctx, func(context.Context, harness.UserInputRequest) (harness.UserInputResponse, error) {
					return harness.UserInputResponse{}, errors.New("unexpected desktop question")
				}))
				done := make(chan error, 1)
				go func() { done <- f.c.ServeSessionUpdates(live, sid, harness.SessionUpdates{}) }()
				defer func() {
					cancel()
					if err := <-done; err != nil {
						t.Error(err)
					}
				}()
			}
			if owner == "permission_changed" {
				var busy *channels.UserError
				for {
					_, err = f.c.channels.SetPermissions(f.ctx, d.Instance, d.Revision+1, harness.ApprovalReadOnly)
					if err == nil {
						break
					}
					if !errors.As(err, &busy) || !strings.Contains(busy.Message, "任务正在运行") {
						t.Fatal(err)
					}
					select {
					case <-f.ctx.Done():
						t.Fatal(err)
					case <-time.After(20 * time.Millisecond):
					}
				}
				releaseChild()
				until("sent")
				after, err := f.c.service.Store.Execution(f.ctx, sid, rid)
				if err != nil || after.Status != harness.ExecutionWaitingInput || after.Attempt != before.Attempt || !strings.Contains(reply, "权限与原任务不一致") {
					t.Fatalf("revoked permission: %+v %q %v", after, reply, err)
				}
				if _, err = os.Stat(filepath.Join(d.Workspace, "auto-root.txt")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unexpected write: %v", err)
				}
				return
			}
			if owner == "restart" {
				f.c.channels.Close()
				releaseChild()
				for {
					var child string
					if err = db.QueryRowContext(f.ctx, `SELECT status FROM eino_background_tasks WHERE id=?`, before.WaitingTasks[0]).Scan(&child); err != nil {
						t.Fatal(err)
					}
					if child == "completed" {
						break
					}
					select {
					case <-f.ctx.Done():
						t.Fatalf("child status %s", child)
					case <-time.After(20 * time.Millisecond):
					}
				}
				if err = f.c.Close(); err != nil {
					t.Fatal(err)
				}
				f.c, err = Open(f.ctx, f.cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer f.c.Close()
				db = f.c.store.DB()
			}
			releaseChild()
			until("sent")
			after, err := f.c.service.Store.Execution(f.ctx, sid, rid)
			if err != nil || after.Status != harness.ExecutionCompleted || after.InputID != before.InputID || reply != "completed parent" {
				t.Fatalf("final state %+v, reply %q, %v", after, reply, err)
			}
			var runs, writes int
			var claims int
			if err = db.QueryRow(`SELECT count(*) FROM harness_execution_attempts WHERE run_id=? AND base_checkpoint_sha=?`, rid, checkpoint).Scan(&claims); err != nil || claims != 1 {
				t.Fatalf("wait resumed %d times: %v", claims, err)
			}
			if err = db.QueryRow(`SELECT (SELECT count(*) FROM harness_runs WHERE session_id=?),(SELECT count(*) FROM harness_tool_receipts WHERE run_id=? AND tool_call_id='auto-root-write')`, sid, rid).Scan(&runs, &writes); err != nil || runs != 1 || writes != 1 {
				t.Fatalf("runs=%d writes=%d: %v", runs, writes, err)
			}
			data, err := os.ReadFile(filepath.Join(d.Workspace, "auto-root.txt"))
			if err != nil || string(data) != "root once" {
				t.Fatalf("root write %q: %v", data, err)
			}
		})
	}
}
