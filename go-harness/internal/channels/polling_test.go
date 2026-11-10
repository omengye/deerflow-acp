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

func waitChannel(t *testing.T, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out:", label)
	}
}

func waitMessageState(t *testing.T, m *Manager, id, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var state string
		err := m.db.QueryRow(`SELECT status FROM harness_channel_messages WHERE id=?`, id).Scan(&state)
		if err == nil && state == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("message %s: state=%s want=%s err=%v", id, state, want, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// startPollFixture installs a second, deliberately serial transport. Any poll
// on the sending process or send on the polling process is a test failure.
func startPollFixture(m *Manager, i *instance, poll func(context.Context, channelproto.Poll, any) error) {
	i.manifest.Descriptor.IndependentPoll = true
	descriptor := i.manifest.Descriptor
	m.start = func(context.Context, Manifest) (transport, channelproto.Descriptor, error) {
		return &fakeTransport{call: func(ctx context.Context, method string, in, out any) error {
			if method != "poll" {
				return errors.New("send used the polling process")
			}
			return poll(ctx, in.(channelproto.Poll), out)
		}}, descriptor, nil
	}
}

func TestReadyReplyWakesWorkerDuringLongPoll(t *testing.T) {
	finishModel, began, pollEntered, pollCancelled, sent := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	m, i := receiptFixture(t, func(ctx context.Context, _, _, _, _ string, _ func(string) error) (automation.Outcome, error) {
		close(began)
		select {
		case <-finishModel:
			return automation.Outcome{Status: "completed", Text: "final answer"}, nil
		case <-ctx.Done():
			return automation.Outcome{}, ctx.Err()
		}
	})
	i.proc = &fakeTransport{call: func(_ context.Context, method string, in, out any) error {
		if method != "send" {
			return errors.New("long poll used the sending process")
		}
		if in.(channelproto.Send).Text == "final answer" {
			close(sent)
		}
		return assign(out, map[string]bool{"sent": true})
	}}
	startPollFixture(m, i, func(ctx context.Context, _ channelproto.Poll, _ any) error {
		close(pollEntered)
		<-ctx.Done()
		close(pollCancelled)
		return ctx.Err()
	})
	if err := m.accept(context.Background(), i, i.auth, i.cfg, channelproto.Batch{Messages: []channelproto.Message{message("a", "alice")}}); err != nil {
		t.Fatal(err)
	}
	m.startWorker(i)
	waitChannel(t, began, "model start")
	waitChannel(t, pollEntered, "provider long poll")
	close(finishModel)
	waitChannel(t, sent, "answer while poll remains blocked")
	waitMessageState(t, m, "a", "sent")
	m.stopWorker(i)
	waitChannel(t, pollCancelled, "poll cancelled on worker shutdown")
}

func TestQueuedQuickRunsDoNotStarvePollingStop(t *testing.T) {
	ctx := context.Background()
	var runs atomic.Int32
	m, i := receiptFixture(t, func(_ context.Context, _, _, prompt, _ string, _ func(string) error) (automation.Outcome, error) {
		if prompt != "/stop" {
			runs.Add(1)
		}
		return automation.Outcome{Status: "completed", Text: "answer"}, nil
	})
	i.proc = &fakeTransport{call: func(_ context.Context, method string, _, out any) error {
		if method != "send" {
			return errors.New("poll used the sending process")
		}
		return assign(out, map[string]bool{"sent": true})
	}}
	allowStop := make(chan struct{})
	startPollFixture(m, i, func(ctx context.Context, _ channelproto.Poll, out any) error {
		select {
		case <-allowStop:
		case <-ctx.Done():
			return ctx.Err()
		}
		stop := message("stop", "alice")
		stop.Text = "/stop"
		return assign(out, channelproto.Batch{Cursor: "stopped", Messages: []channelproto.Message{stop}})
	})
	if err := m.accept(ctx, i, i.auth, i.cfg, channelproto.Batch{Messages: []channelproto.Message{message("a", "alice"), message("b", "alice"), message("c", "alice")}}); err != nil {
		t.Fatal(err)
	}
	if err := m.workerStep(ctx, i); err != nil {
		t.Fatal(err)
	}
	i.mu.Lock()
	active := i.active
	i.mu.Unlock()
	if active == nil {
		t.Fatal("first task did not launch")
	}
	waitChannel(t, active.done, "quick model completion")
	close(allowStop)
	// No queue-draining workerStep is needed to receive the pending /stop.
	waitMessageState(t, m, "stop", "queued")
	if err := m.workerStep(ctx, i); err != nil {
		t.Fatal(err)
	}
	waitMessageState(t, m, "stop", "sent")
	waitMessageState(t, m, "b", "ignored")
	waitMessageState(t, m, "c", "ignored")
	if runs.Load() != 1 {
		t.Fatal("queued work ran before stop", runs.Load())
	}
}

func TestStopPreservesLaterPromptsAndOtherControls(t *testing.T) {
	ctx := context.Background()
	m, i := fixture(t, func(context.Context, string, string, string, string, func(string) error) (automation.Outcome, error) {
		return automation.Outcome{Status: "completed", Text: "stopped"}, nil
	})
	stop, help := message("stop", "alice"), message("help", "alice")
	stop.Text, help.Text = "/stop", "/help"
	if err := m.accept(ctx, i, i.auth, i.cfg, channelproto.Batch{Messages: []channelproto.Message{message("old", "alice"), help, stop, message("new", "alice")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.db.Exec(`UPDATE harness_channel_messages SET status='running' WHERE id='stop'`); err != nil {
		t.Fatal(err)
	}
	ctx = WithDispatch(ctx, Dispatch{i.id, i.auth.Account, stop.Conversation, stop.Sender, i.cfg.Workspace, stop.ID, i.revision})
	if err := m.control(ctx, i, i.auth, i.cfg, stop, ""); err != nil {
		t.Fatal(err)
	}
	waitMessageState(t, m, "old", "ignored")
	waitMessageState(t, m, "new", "queued")
	waitMessageState(t, m, "help", "queued")
}

func TestPollFailureBackoffDoesNotDelayReadyReply(t *testing.T) {
	finishModel, began, sent := make(chan struct{}), make(chan struct{}), make(chan struct{})
	m, i := receiptFixture(t, func(ctx context.Context, _, _, _, _ string, _ func(string) error) (automation.Outcome, error) {
		close(began)
		select {
		case <-finishModel:
			return automation.Outcome{Status: "completed", Text: "answer"}, nil
		case <-ctx.Done():
			return automation.Outcome{}, ctx.Err()
		}
	})
	i.proc = &fakeTransport{call: func(_ context.Context, method string, in, out any) error {
		if method != "send" {
			return errors.New("poll used sending process")
		}
		if in.(channelproto.Send).Text == "answer" {
			close(sent)
		}
		return assign(out, map[string]bool{"sent": true})
	}}
	startPollFixture(m, i, func(context.Context, channelproto.Poll, any) error {
		return errors.New("provider poll unavailable")
	})
	if err := m.accept(context.Background(), i, i.auth, i.cfg, channelproto.Batch{Messages: []channelproto.Message{message("a", "alice")}}); err != nil {
		t.Fatal(err)
	}
	m.startWorker(i)
	waitChannel(t, began, "model start")
	deadline := time.Now().Add(3 * time.Second)
	for {
		i.mu.Lock()
		failed := i.lastError != ""
		i.mu.Unlock()
		if failed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("poll did not enter backoff")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(finishModel)
	waitChannel(t, sent, "answer during poll backoff")
}

func TestIndependentPollCommitsCursorBeforeNextRequest(t *testing.T) {
	ctx := context.Background()
	m, i := receiptFixture(t, nil)
	i.proc = &fakeTransport{call: func(_ context.Context, _ string, _, out any) error {
		return assign(out, map[string]bool{"sent": true})
	}}
	nextPoll := make(chan struct{})
	var polls atomic.Int32
	startPollFixture(m, i, func(ctx context.Context, in channelproto.Poll, out any) error {
		if polls.Add(1) == 1 {
			if in.Cursor != "" {
				t.Error("unexpected initial cursor", in.Cursor)
			}
			return assign(out, channelproto.Batch{Cursor: "committed", Messages: []channelproto.Message{message("a", "alice")}})
		}
		var cursor string
		var count int
		if err := m.db.QueryRow(`SELECT cursor FROM harness_channels WHERE id=?`, i.id).Scan(&cursor); err != nil || cursor != "committed" || in.Cursor != cursor {
			t.Error("polled uncommitted cursor", in.Cursor, cursor, err)
		}
		if err := m.db.QueryRow(`SELECT count(*) FROM harness_channel_messages WHERE id='a'`).Scan(&count); err != nil || count != 1 {
			t.Error("cursor advanced before durable admission", count, err)
		}
		close(nextPoll)
		<-ctx.Done()
		return ctx.Err()
	})
	// Start the receiver without a sending worker so inbox activity cannot be
	// confused with execution. The receiver must progress on its own.
	i.mu.Lock()
	m.startPollerLocked(ctx, i)
	i.mu.Unlock()
	waitChannel(t, nextPoll, "polling with committed cursor")
}

func TestIndependentPollShutdownRejectsLateBatch(t *testing.T) {
	for _, operation := range []string{"disable", "logout"} {
		t.Run(operation, func(t *testing.T) {
			m, i := receiptFixture(t, nil)
			entered := make(chan struct{})
			i.proc = &fakeTransport{call: func(context.Context, string, any, any) error {
				t.Error("no messages should be sent")
				return nil
			}}
			startPollFixture(m, i, func(ctx context.Context, _ channelproto.Poll, out any) error {
				close(entered)
				<-ctx.Done()
				// A provider response can race cancellation. It must not commit.
				return assign(out, channelproto.Batch{Cursor: "late", Messages: []channelproto.Message{message("late", "alice")}})
			})
			workspace, revision := i.cfg.Workspace, i.revision
			m.startWorker(i)
			waitChannel(t, entered, "provider poll")
			var err error
			if operation == "disable" {
				_, err = m.Configure(context.Background(), i.id, revision, workspace, nil, false)
			} else {
				_, err = m.Login(context.Background(), i.id, "logout", "", "")
			}
			if err != nil {
				t.Fatal(err)
			}
			var count int
			if err = m.db.QueryRow(`SELECT count(*) FROM harness_channel_messages`).Scan(&count); err != nil || count != 0 {
				t.Fatal("late batch was admitted", count, err)
			}
			i.mu.Lock()
			defer i.mu.Unlock()
			if i.poller != nil || i.proc != nil || i.cursor != "" || i.cfg.Enabled {
				t.Fatal("receiver survived shutdown")
			}
		})
	}
}

func TestIndependentPollAuthExpiryCancelsActiveRun(t *testing.T) {
	began, cancelled, expire := make(chan struct{}), make(chan struct{}), make(chan struct{})
	m, i := receiptFixture(t, func(ctx context.Context, _, _, _, _ string, _ func(string) error) (automation.Outcome, error) {
		close(began)
		<-ctx.Done()
		close(cancelled)
		return automation.Outcome{}, ctx.Err()
	})
	i.proc = &fakeTransport{call: func(_ context.Context, _ string, _, out any) error {
		return assign(out, map[string]bool{"sent": true})
	}}
	startPollFixture(m, i, func(ctx context.Context, _ channelproto.Poll, _ any) error {
		select {
		case <-expire:
			return channelproto.ErrAuthExpired
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if err := m.accept(context.Background(), i, i.auth, i.cfg, channelproto.Batch{Messages: []channelproto.Message{message("a", "alice")}}); err != nil {
		t.Fatal(err)
	}
	m.startWorker(i)
	waitChannel(t, began, "model start")
	close(expire)
	waitChannel(t, cancelled, "expired authentication cancelling active run")
	i.mu.Lock()
	defer i.mu.Unlock()
	if len(i.auth.Secret) != 0 || i.lastError != "Login expired; sign in again" {
		t.Fatal("authentication expiry not propagated")
	}
}

func TestMissingReplyContextCannotRunOrFenceConversation(t *testing.T) {
	for _, missing := range []string{"", " \t\n"} {
		t.Run("context="+missing, func(t *testing.T) {
			ctx := context.Background()
			runs, sends := 0, 0
			m, i := receiptFixture(t, func(context.Context, string, string, string, string, func(string) error) (automation.Outcome, error) {
				runs++
				return automation.Outcome{Status: "completed", Text: "answer"}, nil
			})
			i.manifest.Descriptor.RequiresReplyContext = true
			p := &fakeTransport{call: func(_ context.Context, method string, in, out any) error {
				if method != "send" || in.(channelproto.Send).Message.ID != "valid" {
					t.Error("unsendable message reached transport")
					return errors.New("invalid send")
				}
				sends++
				return assign(out, map[string]bool{"sent": true})
			}}
			i.proc = p
			first := message("missing", "alice")
			first.Context = missing
			batch := channelproto.Batch{Cursor: "next", Messages: []channelproto.Message{first, message("valid", "alice")}}
			for range 2 {
				if err := m.accept(ctx, i, i.auth, i.cfg, batch); err != nil {
					t.Fatal(err)
				}
			}
			waitMessageState(t, m, "missing", "ignored")
			if _, err := m.process(ctx, i, p, i.auth, i.cfg); err != nil || runs != 1 || sends != 1 {
				t.Fatal("valid follow-up blocked", err, runs, sends)
			}
			waitMessageState(t, m, "valid", "sent")
			var receipts int
			if err := m.db.QueryRow(`SELECT COUNT(*) FROM harness_channel_receipts WHERE id='missing'`).Scan(&receipts); err != nil || receipts != 0 {
				t.Fatal("invalid input got a receipt", receipts, err)
			}
			if i.cursor != "next" {
				t.Fatal("invalid input prevented cursor progress")
			}
		})
	}
}

func TestStoredMissingReplyContextIsIgnoredBeforeDispatch(t *testing.T) {
	for _, state := range []string{"queued", "ready"} {
		t.Run(state, func(t *testing.T) {
			m, i := fixture(t, func(context.Context, string, string, string, string, func(string) error) (automation.Outcome, error) {
				t.Error("legacy invalid input ran model")
				return automation.Outcome{}, nil
			})
			first := message("missing", "alice")
			first.Context = ""
			if err := m.accept(context.Background(), i, i.auth, i.cfg, channelproto.Batch{Messages: []channelproto.Message{first}}); err != nil {
				t.Fatal(err)
			}
			i.manifest.Descriptor.RequiresReplyContext = true
			if _, err := m.db.Exec(`UPDATE harness_channel_messages SET status=?`, state); err != nil {
				t.Fatal(err)
			}
			p := &fakeTransport{call: func(context.Context, string, any, any) error {
				t.Error("legacy invalid input sent")
				return nil
			}}
			if handled, err := m.process(context.Background(), i, p, i.auth, i.cfg); !handled || err != nil {
				t.Fatal(handled, err)
			}
			waitMessageState(t, m, "missing", "ignored")
		})
	}
}

func TestQuotedTextPersistsAndReachesModel(t *testing.T) {
	ctx := context.Background()
	var prompt string
	m, i := receiptFixture(t, func(_ context.Context, _, _, text, _ string, _ func(string) error) (automation.Outcome, error) {
		prompt = text
		return automation.Outcome{Status: "completed", Text: "answer"}, nil
	})
	msg := message("quote", "alice")
	msg.Text, msg.QuotedText = "照这个处理", "【引用消息】\n> 消息 ID：previous\n> 只检查测试环境，不修改生产环境"
	if err := m.accept(ctx, i, i.auth, i.cfg, channelproto.Batch{Messages: []channelproto.Message{msg}}); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(ctx, m.db, m.run)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	p := &fakeTransport{call: func(_ context.Context, _ string, _, out any) error {
		return assign(out, map[string]bool{"sent": true})
	}}
	j := restarted.items[i.id]
	if handled, err := restarted.process(ctx, j, p, j.auth, j.cfg); !handled || err != nil {
		t.Fatal(handled, err)
	}
	if prompt != msg.Text+"\n\n"+msg.QuotedText {
		t.Fatal("quote context was lost", prompt)
	}
}

func TestQuotedCommandsStayCommandsAndIncompleteQuotesDoNotRun(t *testing.T) {
	ctx := context.Background()
	m, i := receiptFixture(t, nil)
	for _, command := range []string{"/stop", "/status", "/new", "/approve CODE", "/permission read_only"} {
		msg := message(command, "alice")
		msg.Text, msg.QuotedText = command, "【引用消息】\n> earlier text"
		if err := m.accept(ctx, i, i.auth, i.cfg, channelproto.Batch{Messages: []channelproto.Message{msg}}); err != nil {
			t.Fatal(err)
		}
		var text string
		if err := m.db.QueryRow(`SELECT text FROM harness_channel_messages WHERE id=?`, command).Scan(&text); err != nil || text != command {
			t.Fatal("quote changed command semantics", text, err)
		}
	}
	for _, oversized := range []bool{false, true} {
		msg := message("incomplete", "alice")
		msg.Unsupported, msg.QuotedText = true, "【引用内容不可用，请粘贴原文】"
		if oversized {
			msg.ID, msg.Unsupported, msg.QuotedText = "oversized", false, strings.Repeat("x", 16384)
		}
		if err := m.accept(ctx, i, i.auth, i.cfg, channelproto.Batch{Messages: []channelproto.Message{msg}}); err != nil {
			t.Fatal(err)
		}
		var text, state, reply string
		if err := m.db.QueryRow(`SELECT text,status,reply FROM harness_channel_messages WHERE id=?`, msg.ID).Scan(&text, &state, &reply); err != nil || text != "" || state != "ready" || !strings.Contains(reply, "quoted text") {
			t.Fatal("incomplete quote admitted for execution", text, state, reply, err)
		}
	}
	var receipts int
	if err := m.db.QueryRow(`SELECT COUNT(*) FROM harness_channel_receipts`).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatal("command or incomplete quote got receipt", receipts, err)
	}
}
