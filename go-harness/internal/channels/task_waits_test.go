package channels

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/channelproto"
	"github.com/omengye/deerflow-acp/go-harness/internal/automation"
)

func TestWeChatStopWhileCheckingDependency(t *testing.T) {
	ctx := context.Background()
	m, i := receiptFixture(t, nil)
	defer m.joinActive(i)
	batch := channelproto.Batch{Messages: []channelproto.Message{message("original", "alice"), message("stop", "alice")}}
	batch.Messages[1].Text = "/stop"
	if err := m.accept(ctx, i, i.auth, i.cfg, batch); err != nil {
		t.Fatal(err)
	}
	_, err := m.db.Exec(`UPDATE harness_channel_messages SET status='waiting_task',session_id='s' WHERE id='original';
INSERT INTO harness_channel_runs(instance,account,id,session_id,run_id) VALUES('test','bot','original','s','r');
INSERT INTO harness_channel_threads VALUES('test','bot','alice','alice',?,'s')`, i.cfg.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	m.Continue = func(ctx context.Context, _ Dispatch, wait TaskWait) (*automation.Outcome, error) {
		if wait.StopRequested {
			return &automation.Outcome{Text: "cancelled"}, nil
		}
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	i.proc = &fakeTransport{call: func(_ context.Context, op string, _, out any) error {
		if op == "poll" {
			return assign(out, channelproto.Batch{})
		}
		return assign(out, map[string]bool{"sent": true})
	}}
	if ok, err := m.continueWaiting(ctx, i, i.auth, i.cfg, i.revision); err != nil || !ok {
		t.Fatal(ok, err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("inspection did not start")
	}
	if err = m.workerStep(ctx, i); err != nil {
		t.Fatal(err)
	}
	select {
	case <-i.active.done:
	case <-time.After(time.Second):
		t.Fatal("inspection did not stop")
	}
	if err = m.workerStep(ctx, i); err != nil {
		t.Fatal(err)
	}
	select {
	case <-i.active.done:
	case <-time.After(time.Second):
		t.Fatal("no durable stop")
	}
	if err = m.workerStep(ctx, i); err != nil {
		t.Fatal(err)
	}
	var status, reply string
	if err = m.db.QueryRow(`SELECT status,reply FROM harness_channel_messages WHERE id='original'`).Scan(&status, &reply); err != nil || status != "sent" || reply != "cancelled" {
		t.Fatal(status, reply, err)
	}
}

func TestWeChatWaitDeliveryAcrossRestart(t *testing.T) {
	for _, phase := range []string{"waiting_task", "running", "sending"} {
		t.Run(phase, func(t *testing.T) {
			ctx := context.Background()
			var m *Manager
			var i *instance
			runs := 0
			m, i = receiptFixture(t, func(ctx context.Context, _, _, _, _ string, bind func(string) error) (automation.Outcome, error) {
				runs++
				if err := bind("session"); err != nil {
					return automation.Outcome{}, err
				}
				tx, err := m.db.BeginTx(ctx, nil)
				if err != nil {
					return automation.Outcome{}, err
				}
				defer tx.Rollback()
				d, _ := DispatchFromContext(ctx)
				if err = BindRunTx(ctx, tx, d, "session", "original-run"); err != nil {
					return automation.Outcome{}, err
				}
				return automation.Outcome{Status: "needs_attention", RunID: "original-run", WaitForTasks: true}, tx.Commit()
			})
			resumed := 0
			m.Continue = func(_ context.Context, d Dispatch, w TaskWait) (*automation.Outcome, error) {
				if w.SessionID != "session" || w.RunID != "original-run" || d.MessageID != "question" || d.Account != "bot" {
					return nil, errors.New("unexpected task binding")
				}
				resumed++
				return &automation.Outcome{Text: "final answer"}, nil
			}
			var sends []channelproto.Send
			p := &fakeTransport{call: func(_ context.Context, op string, in, out any) error {
				if op == "poll" {
					return assign(out, channelproto.Batch{})
				}
				sends = append(sends, in.(channelproto.Send))
				return assign(out, map[string]bool{"sent": true})
			}}
			i.proc = p
			batch := channelproto.Batch{Messages: []channelproto.Message{message("question", "alice")}}
			if err := m.accept(ctx, i, i.auth, i.cfg, batch); err != nil {
				t.Fatal(err)
			}
			if err := m.step(ctx, i); err != nil {
				t.Fatal(err)
			}
			if _, err := m.sendReceipts(ctx, i, p, i.auth, i.cfg); err != nil {
				t.Fatal(err)
			}
			if len(sends) != 2 || sends[0].Text != receiptText || sends[1].Text != "正在等待后台任务，完成后会继续。" {
				t.Fatal(sends)
			}
			// The running window has a committed run but no outbound attempt. An
			// interrupted send, with the same binding, must still require review.
			if _, err := m.db.Exec(`UPDATE harness_channel_messages SET status=? WHERE id='question'`, phase); err != nil {
				t.Fatal(err)
			}
			fresh, err := New(ctx, m.db, m.run)
			if err != nil {
				t.Fatal(err)
			}
			defer fresh.Close()
			j := fresh.items[i.id]
			fresh.Continue = m.Continue
			j.proc = p
			if err := fresh.accept(ctx, j, j.auth, j.cfg, batch); err != nil {
				t.Fatal(err)
			}
			if err := fresh.workerStep(ctx, j); err != nil {
				t.Fatal(err)
			}
			if phase != "sending" {
				if j.active == nil {
					t.Fatal("no recovery")
				}
				select {
				case <-j.active.done:
				case <-time.After(2 * time.Second):
					t.Fatal("recovery did not finish")
				}
			}
			for range 2 {
				if err := fresh.workerStep(ctx, j); err != nil {
					t.Fatal(err)
				}
			}
			var status string
			if err = m.db.QueryRow(`SELECT status FROM harness_channel_messages WHERE id='question'`).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if phase == "sending" {
				if status != "unknown" || resumed != 0 || len(sends) != 2 {
					t.Fatalf("ambiguous send replayed: %s %d %+v", status, resumed, sends)
				}
			} else if status != "sent" || resumed != 1 || len(sends) != 3 || sends[2].Text != "final answer" || sends[2].Message.Context != "reply-token" || sends[2].Key == sends[1].Key {
				t.Fatalf("delivery: %s %d %+v", status, resumed, sends)
			}
			if runs != 1 {
				t.Fatal("initial prompt repeated", runs)
			}
		})
	}
}
