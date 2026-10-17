package channels

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/channelproto"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/automation"
)

func TestFullAccessAdmissionRevocationAndRestart(t *testing.T) {
	ctx := context.Background()
	var modes []string
	m, i := fixture(t, func(_ context.Context, _, _, _, mode string, bind func(string) error) (automation.Outcome, error) {
		modes = append(modes, mode)
		return automation.Outcome{Status: "completed", Text: "ok"}, bind("session")
	})
	p := &fakeTransport{call: func(_ context.Context, _ string, _, out any) error { return assign(out, map[string]bool{"sent": true}) }}
	accept := func(id, text string) {
		msg := message(id, "alice")
		msg.Text = text
		if err := m.accept(ctx, i, i.auth, i.cfg, channelproto.Batch{Messages: []channelproto.Message{msg}}); err != nil {
			t.Fatal(err)
		}
	}
	process := func() {
		if _, err := m.process(ctx, i, p, i.auth, i.cfg); err != nil {
			t.Fatal(err)
		}
	}
	accept("no-grant", "/permission full")
	process()
	if effectiveApproval(i.cfg, i.auth) != harness.ApprovalGlobal {
		t.Fatal("chat self-elevated")
	}
	accept("old", "hello")
	if _, err := m.SetPermissions(ctx, i.id, i.revision, harness.ApprovalAllowAlways); err != nil {
		t.Fatal(err)
	}
	process()
	accept("new", "hello")
	process()
	if strings.Join(modes, ",") != "global,allow_always" {
		t.Fatal(modes)
	}
	accept("switch", "/permission global")
	process()
	accept("switch-back", "/permission full")
	process()
	if !fullAccessAuthorized(i.cfg, i.auth) || effectiveApproval(i.cfg, i.auth) != harness.ApprovalAllowAlways {
		t.Fatal("trusted toggle lost")
	}
	reopened, err := New(ctx, m.db, m.run)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	j := reopened.items[i.id]
	if effectiveApproval(j.cfg, j.auth) != harness.ApprovalAllowAlways {
		t.Fatal("grant lost on restart")
	}
	changed := j.auth
	changed.Owner = "stranger"
	if effectiveApproval(j.cfg, changed) == harness.ApprovalAllowAlways {
		t.Fatal("grant crossed identity")
	}
	accept("revoked-queued", "hello")
	if _, err = m.SetPermissions(ctx, i.id, i.revision, harness.ApprovalGlobal); err != nil {
		t.Fatal(err)
	}
	process()
	accept("self-upgrade", "/permission full")
	process()
	if modes[len(modes)-1] != harness.ApprovalGlobal || fullAccessAuthorized(i.cfg, i.auth) {
		t.Fatal("revocation failed")
	}
	if _, err = m.SetPermissions(ctx, i.id, i.revision, harness.ApprovalAllowAlways); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Configure(ctx, i.id, i.revision, t.TempDir(), nil, false); err != nil {
		t.Fatal(err)
	}
	if i.cfg.FullAccess != nil || i.cfg.Approval != harness.ApprovalGlobal {
		t.Fatal("workspace carried grant")
	}
}

func TestChannelConfirmationScopeExpiryDigestAndSingleUse(t *testing.T) {
	ctx := context.Background()
	m, i := fixture(t, nil)
	d := Dispatch{i.id, i.auth.Account, "alice", "alice", i.cfg.Workspace, "msg", i.revision}
	state := harness.ExecutionState{SessionID: "session", RunID: "run", Version: 3}
	p := harness.ExecutionInteraction{ID: "intent", Version: 1, ArgumentsDigest: "digest", ConfigVersion: 2}
	code, err := m.Confirmation(ctx, d, state, p)
	if err != nil {
		t.Fatal(err)
	}
	other := d
	other.Conversation = "another"
	if m.Decide(ctx, other, state, code, harness.AllowOnce) == nil {
		t.Fatal("cross conversation confirmation")
	}
	if err = m.Decide(ctx, d, state, code, harness.AllowOnce); err != nil {
		t.Fatal(err)
	}
	if m.Decide(ctx, d, state, code, harness.AllowOnce) == nil {
		t.Fatal("duplicate confirmation")
	}
	drift := p
	drift.ArgumentsDigest = "changed"
	if _, ready, err := m.TakeDecisions(ctx, d, state, []harness.ExecutionInteraction{drift}); err != nil || ready {
		t.Fatal("digest mismatch allowed", ready, err)
	}
	decisions, ready, err := m.TakeDecisions(ctx, d, state, []harness.ExecutionInteraction{p})
	if err != nil || !ready || decisions[p.ID] != harness.AllowOnce {
		t.Fatal(decisions, ready, err)
	}
	if _, ready, err = m.TakeDecisions(ctx, d, state, []harness.ExecutionInteraction{p}); err != nil || ready {
		t.Fatal("reused approval", ready, err)
	}
	code, err = m.Confirmation(ctx, d, state, p)
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.db.Exec(`UPDATE harness_channel_confirmations SET expires_ms=0 WHERE code=?`, code)
	if err != nil {
		t.Fatal(err)
	}
	if m.Decide(ctx, d, state, code, harness.AllowOnce) == nil {
		t.Fatal("expired code approved")
	}
	d.Revision++
	if _, err = m.Confirmation(ctx, d, state, p); err == nil {
		t.Fatal("stale policy")
	}
}

func TestChannelControlsDuringLongRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	began := make(chan struct{})
	finished := make(chan struct{})
	m, i := fixture(t, func(ctx context.Context, _, _, _, _ string, bind func(string) error) (automation.Outcome, error) {
		if err := bind("session"); err != nil {
			return automation.Outcome{}, err
		}
		close(began)
		<-ctx.Done()
		close(finished)
		return automation.Outcome{Text: "stopped", Status: "needs_attention"}, ctx.Err()
	})
	var mu sync.Mutex
	var sent []string
	polls := 0
	i.proc = &fakeTransport{call: func(_ context.Context, op string, in, out any) error {
		mu.Lock()
		defer mu.Unlock()
		if op == "send" {
			sent = append(sent, in.(channelproto.Send).Text)
			return assign(out, map[string]bool{"sent": true})
		}
		polls++
		text := "/status"
		if polls > 1 {
			text = "/stop"
		}
		msg := message(text, "alice")
		msg.Text = text
		return assign(out, channelproto.Batch{Messages: []channelproto.Message{msg}})
	}}
	if err := m.accept(ctx, i, i.auth, i.cfg, channelproto.Batch{Messages: []channelproto.Message{message("work", "alice")}}); err != nil {
		t.Fatal(err)
	}
	if err := m.workerStep(ctx, i); err != nil {
		t.Fatal(err)
	}
	select {
	case <-began:
	case <-time.After(time.Second):
		t.Fatal("no dispatch")
	}
	for range 4 {
		if err := m.workerStep(ctx, i); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("stop blocked behind model")
	}
	m.joinActive(i)
	mu.Lock()
	defer mu.Unlock()
	if len(sent) < 2 || !strings.Contains(sent[0], "正在运行") || !strings.Contains(sent[1], "请求停止") {
		t.Fatal(sent)
	}
	var status string
	if err := m.db.QueryRow(`SELECT status FROM harness_channel_messages WHERE id='work'`).Scan(&status); err != nil || status == "running" {
		t.Fatal(status, err)
	}
}
