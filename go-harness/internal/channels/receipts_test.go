package channels

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/channelproto"
	"github.com/omengye/deerflow-acp/go-harness/internal/automation"
)

func receiptFixture(t *testing.T, run Runner) (*Manager, *instance) {
	t.Helper()
	m, i := fixture(t, run)
	i.manifest.ID = "wechat"
	i.manifest.Descriptor.ID = "wechat"
	if err := m.persist(context.Background(), i); err != nil {
		t.Fatal(err)
	}
	return m, i
}

func TestWeChatReceiptBeforeModelAndDedupeAcrossRestart(t *testing.T) {
	ctx := context.Background()
	var sent []channelproto.Send
	runs := 0
	m, i := receiptFixture(t, func(_ context.Context, _, _, prompt, _ string, bind func(string) error) (automation.Outcome, error) {
		runs++
		if len(sent) != 1 || sent[0].Text != receiptText || prompt != "hello" {
			t.Fatal("receipt must precede the model without changing its prompt", sent, prompt)
		}
		return automation.Outcome{Status: "completed", Text: "answer"}, bind("session")
	})
	batch := channelproto.Batch{Cursor: "committed", Messages: []channelproto.Message{message("a", "alice"), message("a", "alice"), message("stranger", "stranger")}}
	p := &fakeTransport{call: func(_ context.Context, op string, in, out any) error {
		if op == "poll" {
			return assign(out, batch)
		}
		var cursor string
		if err := m.db.QueryRow(`SELECT cursor FROM harness_channels WHERE id=?`, i.id).Scan(&cursor); err != nil || cursor != "committed" {
			t.Fatal("sent before admission committed", cursor, err)
		}
		sent = append(sent, in.(channelproto.Send))
		return assign(out, map[string]bool{"sent": true})
	}}
	i.proc = p
	if err := m.step(ctx, i); err != nil || len(sent) != 1 || runs != 0 {
		t.Fatal("no immediate receipt after poll", err, sent, runs)
	}
	if err := m.step(ctx, i); err != nil || len(sent) != 2 || runs != 1 {
		t.Fatal(err, sent, runs)
	}
	if sent[0].Key == sent[1].Key || sent[0].Message.Context != "reply-token" || sent[1].Text != "answer" {
		t.Fatal("receipt must use its own key and preserve the reply context", sent)
	}
	restarted, err := New(ctx, m.db, m.run)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	j := restarted.items[i.id]
	j.proc = p
	if err := restarted.step(ctx, j); err != nil || len(sent) != 2 || runs != 1 {
		t.Fatal("restart or duplicate poll replayed delivery", err, sent, runs)
	}
}

func TestWeChatReceiptWhileEarlierRunIsBlocked(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	began := make(chan struct{})
	var runs atomic.Int32
	m, i := receiptFixture(t, func(ctx context.Context, _, _, _, _ string, _ func(string) error) (automation.Outcome, error) {
		runs.Add(1)
		close(began)
		<-ctx.Done()
		return automation.Outcome{}, ctx.Err()
	})
	t.Cleanup(func() { cancel(); m.joinActive(i) })
	var sent []channelproto.Send
	i.proc = &fakeTransport{call: func(_ context.Context, op string, in, out any) error {
		if op == "poll" {
			return assign(out, channelproto.Batch{Messages: []channelproto.Message{message("followup", "alice")}})
		}
		sent = append(sent, in.(channelproto.Send))
		return assign(out, map[string]bool{"sent": true})
	}}
	if err := m.accept(ctx, i, i.auth, i.cfg, channelproto.Batch{Messages: []channelproto.Message{message("first", "alice")}}); err != nil {
		t.Fatal(err)
	}
	if err := m.workerStep(ctx, i); err != nil {
		t.Fatal(err)
	}
	select {
	case <-began:
	case <-time.After(time.Second):
		t.Fatal("model did not start")
	}
	if err := m.workerStep(ctx, i); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 || sent[1].Message.ID != "followup" || sent[1].Text != receiptText || runs.Load() != 1 {
		t.Fatal("queued message did not receive an immediate receipt", sent, runs.Load())
	}
	var state string
	if err := m.db.QueryRow(`SELECT status FROM harness_channel_messages WHERE id='followup'`).Scan(&state); err != nil || state != "queued" {
		t.Fatal("receipt changed the queued task", state, err)
	}
}

func TestWeChatReceiptFailureDoesNotFenceAnswerOrRetry(t *testing.T) {
	for _, failure := range []string{"network", "unacknowledged", "timeout"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			runs, attempts, answers := 0, 0, 0
			m, i := receiptFixture(t, func(_ context.Context, _, _, _, _ string, _ func(string) error) (automation.Outcome, error) {
				runs++
				return automation.Outcome{Status: "completed", Text: "answer"}, nil
			})
			i.proc = &fakeTransport{call: func(ctx context.Context, op string, in, out any) error {
				attempts++
				if op != "send" || in.(channelproto.Send).Text != receiptText {
					t.Fatal("expected receipt", op, in)
				}
				switch failure {
				case "network":
					return errors.New("ambiguous delivery")
				case "timeout":
					<-ctx.Done()
					return ctx.Err()
				default:
					return assign(out, map[string]bool{"sent": false})
				}
			}}
			if err := m.accept(ctx, i, i.auth, i.cfg, channelproto.Batch{Messages: []channelproto.Message{message("a", "alice")}}); err != nil {
				t.Fatal(err)
			}
			if stopped, err := m.sendReceipts(ctx, i, i.proc, i.auth, i.cfg); err != nil || !stopped || attempts != 1 || i.proc != nil {
				t.Fatal("receipt failure must retire transport without delivery backoff", err, attempts)
			}
			var requestState, receiptState string
			if err := m.db.QueryRow(`SELECT m.status,r.status FROM harness_channel_messages m JOIN harness_channel_receipts r USING(instance,account,id) WHERE m.id='a'`).Scan(&requestState, &receiptState); err != nil || requestState != "queued" || receiptState != "unknown" {
				t.Fatal("receipt failure affected task state", requestState, receiptState, err)
			}
			restarted, err := New(ctx, m.db, m.run)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			j := restarted.items[i.id]
			restarted.start = func(context.Context, Manifest) (transport, channelproto.Descriptor, error) {
				return &fakeTransport{call: func(_ context.Context, op string, in, out any) error {
					if op != "send" || in.(channelproto.Send).Text != "answer" {
						t.Fatal("receipt retried or answer blocked", op, in)
					}
					answers++
					return assign(out, map[string]bool{"sent": true})
				}}, j.manifest.Descriptor, nil
			}
			if err := restarted.step(ctx, j); err != nil || runs != 1 || answers != 1 {
				t.Fatal("failed receipt blocked answer", err, runs, answers)
			}
		})
	}
}

func TestWeChatFailedReceiptsDoNotStarveQueuedWork(t *testing.T) {
	ctx := context.Background()
	runs, attempts, answers := 0, 0, 0
	m, i := receiptFixture(t, func(_ context.Context, _, _, _, _ string, _ func(string) error) (automation.Outcome, error) {
		runs++
		return automation.Outcome{Status: "completed", Text: "answer"}, nil
	})
	newTransport := func() transport {
		return &fakeTransport{call: func(_ context.Context, op string, in, out any) error {
			if op != "send" {
				t.Fatal(op)
			}
			if in.(channelproto.Send).Text == receiptText {
				attempts++
				return errors.New("receipt unavailable")
			}
			answers++
			return assign(out, map[string]bool{"sent": true})
		}}
	}
	i.proc = newTransport()
	m.start = func(context.Context, Manifest) (transport, channelproto.Descriptor, error) {
		return newTransport(), i.manifest.Descriptor, nil
	}
	batch := channelproto.Batch{Messages: []channelproto.Message{message("a", "alice"), message("b", "alice"), message("c", "alice")}}
	if err := m.accept(ctx, i, i.auth, i.cfg, batch); err != nil {
		t.Fatal(err)
	}
	for step := 1; step <= 3; step++ {
		if err := m.step(ctx, i); err != nil || runs != step || answers != step || attempts != step {
			t.Fatal("receipt backlog delayed normal dispatch", err, runs, answers, attempts)
		}
	}
}

func TestWeChatPendingReceiptSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	m, i := receiptFixture(t, nil)
	if err := m.accept(ctx, i, i.auth, i.cfg, channelproto.Batch{Messages: []channelproto.Message{message("a", "alice")}}); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(ctx, m.db, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	j := restarted.items[i.id]
	sent := 0
	j.proc = &fakeTransport{call: func(_ context.Context, op string, in, out any) error {
		if op != "send" || in.(channelproto.Send).Text != receiptText {
			t.Fatal(op, in)
		}
		sent++
		return assign(out, map[string]bool{"sent": true})
	}}
	if _, err := restarted.sendReceipts(ctx, j, j.proc, j.auth, j.cfg); err != nil || sent != 1 {
		t.Fatal("committed receipt was lost on restart", err, sent)
	}
}

func TestWeChatReceiptAdmissionAndCommandExclusions(t *testing.T) {
	ctx := context.Background()
	m, i := receiptFixture(t, nil)
	batch := channelproto.Batch{Messages: []channelproto.Message{message("ordinary", "alice"), message("stranger", "stranger")}}
	for _, text := range []string{"/new", "/help", "/status", "/permission full", "/resume", "/approve CODE", "/deny CODE", "/stop", "/pair CODE"} {
		msg := message(text, "alice")
		msg.Text = "  " + text + "\n"
		batch.Messages = append(batch.Messages, msg)
	}
	group, unsupported, noContext, empty, oversized := message("group", "alice"), message("unsupported", "alice"), message("no-context", "alice"), message("empty", "alice"), message("oversized", "alice")
	group.Group, unsupported.Unsupported, noContext.Context, empty.Text, oversized.Text = true, true, "", " ", strings.Repeat("x", 16385)
	batch.Messages = append(batch.Messages, group, unsupported, noContext, empty, oversized)
	if err := m.accept(ctx, i, i.auth, i.cfg, batch); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := m.db.QueryRow(`SELECT count(*) FROM harness_channel_receipts`).Scan(&count); err != nil || count != 1 {
		t.Fatal("non-admitted messages or host commands got a receipt", count, err)
	}
	other, j := fixture(t, nil)
	if err := other.accept(ctx, j, j.auth, j.cfg, batch); err != nil {
		t.Fatal(err)
	}
	if err := other.db.QueryRow(`SELECT count(*) FROM harness_channel_receipts`).Scan(&count); err != nil || count != 0 {
		t.Fatal("another channel received WeChat feedback", count, err)
	}
}

func TestWeChatReceiptAdmissionRollbackAndInvalidation(t *testing.T) {
	ctx := context.Background()
	m, i := receiptFixture(t, nil)
	if _, err := m.db.Exec(`CREATE TRIGGER reject_cursor BEFORE UPDATE OF cursor ON harness_channels BEGIN SELECT RAISE(ABORT, 'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	batch := channelproto.Batch{Messages: []channelproto.Message{message("a", "alice")}}
	if err := m.accept(ctx, i, i.auth, i.cfg, batch); err == nil {
		t.Fatal("expected failed admission")
	}
	var count int
	if err := m.db.QueryRow(`SELECT count(*) FROM harness_channel_receipts`).Scan(&count); err != nil || count != 0 {
		t.Fatal("receipt survived admission rollback", count, err)
	}
	if _, err := m.db.Exec(`DROP TRIGGER reject_cursor`); err != nil {
		t.Fatal(err)
	}
	if err := m.accept(ctx, i, i.auth, i.cfg, batch); err != nil {
		t.Fatal(err)
	}
	i.proc = &fakeTransport{call: func(context.Context, string, any, any) error {
		t.Fatal("invalidated request received a receipt")
		return nil
	}}
	for _, state := range []string{"ignored", "unknown", "ready", "sent"} {
		if _, err := m.db.Exec(`UPDATE harness_channel_messages SET status=?`, state); err != nil {
			t.Fatal(err)
		}
		if _, err := m.sendReceipts(ctx, i, i.proc, i.auth, i.cfg); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.db.Exec(`UPDATE harness_channel_messages SET status='queued'`); err != nil {
		t.Fatal(err)
	}
	oldConfig := i.cfg
	i.cfg.Workspace += "-changed"
	if _, err := m.sendReceipts(ctx, i, i.proc, i.auth, oldConfig); err != nil {
		t.Fatal(err)
	}
	i.cfg = oldConfig
	i.cfg.Enabled = false
	if _, err := m.sendReceipts(ctx, i, i.proc, i.auth, i.cfg); err != nil {
		t.Fatal(err)
	}
}
