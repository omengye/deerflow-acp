package channels

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/channelproto"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/automation"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
	"github.com/omengye/deerflow-acp/go-harness/internal/testutil"
)

type fakeTransport struct {
	call func(context.Context, string, any, any) error
}

func (f *fakeTransport) Call(ctx context.Context, op string, in, out any) error {
	return f.call(ctx, op, in, out)
}
func (f *fakeTransport) Close()   {}
func assign(out, value any) error { raw, _ := json.Marshal(value); return json.Unmarshal(raw, out) }
func fixture(t *testing.T, run Runner) (*Manager, *instance) {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(context.Background(), db.DB(), run)
	if err != nil {
		t.Fatal(err)
	}
	// Directly seed the already-attached state normally produced by Configure.
	i := &instance{id: "test", manifest: Manifest{ID: "fixture", Descriptor: channelproto.Descriptor{ID: "fixture", Name: "Fixture", Protocol: 1}}, cfg: Config{Enabled: true, Workspace: testutil.CanonicalTempDir(t)}, revision: 1, auth: channelproto.Auth{Account: "bot", Owner: "alice", Secret: json.RawMessage(`{"token":"private"}`)}}
	m.items[i.id] = i
	if err = m.persist(context.Background(), i); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close(); db.Close() })
	return m, i
}
func message(id, sender string) channelproto.Message {
	return channelproto.Message{ID: id, Sender: sender, Conversation: sender, Context: "reply-token", Text: "hello"}
}
func TestDedupeAndHostIdentityAdmission(t *testing.T) {
	ctx := context.Background()
	runs, sends := 0, 0
	m, i := fixture(t, func(ctx context.Context, existing, workspace, prompt, approval string, bind func(string) error) (automation.Outcome, error) {
		runs++
		if approval != harness.ApprovalGlobal {
			t.Fatal("not inheriting host policy")
		}
		if err := bind("session"); err != nil {
			return automation.Outcome{}, err
		}
		return automation.Outcome{Status: "completed", Text: "reply"}, nil
	})
	p := &fakeTransport{call: func(_ context.Context, op string, in, out any) error {
		if op != "send" {
			t.Fatal(op)
		}
		sends++
		return assign(out, map[string]bool{"sent": true})
	}}
	batch := channelproto.Batch{Cursor: "next", Messages: []channelproto.Message{message("a", "alice"), message("x", "stranger"), message("a", "alice")}}
	for range 2 {
		if err := m.accept(ctx, i, i.auth, i.cfg, batch); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if _, err := m.process(ctx, i, p, i.auth, i.cfg); err != nil {
			t.Fatal(err)
		}
	}
	if runs != 1 || sends != 1 || i.cursor != "next" {
		t.Fatal(runs, sends, i.cursor)
	}
}

func TestChannelDefaultGlobalPreservesExplicitLegacyApproval(t *testing.T) {
	for _, policy := range []string{"", "read_only", "ask", "allow_always"} {
		t.Run("policy="+policy, func(t *testing.T) {
			ctx := context.Background()
			calls := 0
			m, i := fixture(t, func(_ context.Context, _, _, _, approval string, bind func(string) error) (automation.Outcome, error) {
				calls++
				want := policy
				if want == "" {
					want = harness.ApprovalGlobal
				}
				if approval != want {
					t.Fatalf("approval=%q want=%q", approval, want)
				}
				return automation.Outcome{Status: "completed", Text: "reply"}, bind("session")
			})
			i.cfg.LegacyApproval = policy
			p := &fakeTransport{call: func(_ context.Context, op string, _, out any) error {
				if op != "send" {
					t.Fatal(op)
				}
				return assign(out, map[string]bool{"sent": true})
			}}
			if err := m.accept(ctx, i, i.auth, i.cfg, channelproto.Batch{Messages: []channelproto.Message{message("a", "alice")}}); err != nil {
				t.Fatal(err)
			}
			if handled, err := m.process(ctx, i, p, i.auth, i.cfg); err != nil || !handled || calls != 1 {
				t.Fatal(handled, err, calls)
			}
		})
	}
}

func TestNewCommandMappingDedupeAndRestart(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(map[bool]string{false: "sent", true: "unknown"}[uncertain], func(t *testing.T) {
			ctx := context.Background()
			commands, runs := 0, 0
			m, i := fixture(t, func(_ context.Context, existing, _, prompt, approval string, bind func(string) error) (automation.Outcome, error) {
				if approval != harness.ApprovalGlobal {
					t.Fatal(approval)
				}
				id := existing
				if strings.TrimSpace(prompt) == "/new" {
					commands++
					if existing != "old" {
						t.Fatal("lost prior conversation", existing)
					}
					id = "fresh"
				} else {
					runs++
					if existing != "fresh" {
						t.Fatal("follow-up did not reuse fresh session", existing)
					}
				}
				return automation.Outcome{Status: "completed", Text: "reply"}, bind(id)
			})
			for _, row := range []struct{ account, conversation, sender, workspace, session string }{
				{"bot", "alice", "alice", i.cfg.Workspace, "old"},
				{"bot", "other-chat", "alice", i.cfg.Workspace, "other-chat-session"},
				{"bot", "alice", "alice", "other-workspace", "other-workspace-session"},
				{"other-bot", "alice", "alice", i.cfg.Workspace, "other-account-session"},
			} {
				if _, err := m.db.Exec(`INSERT INTO harness_channel_threads VALUES(?,?,?,?,?,?)`, i.id, row.account, row.conversation, row.sender, row.workspace, row.session); err != nil {
					t.Fatal(err)
				}
			}
			command, stranger, followup := message("command", "alice"), message("stranger", "stranger"), message("followup", "alice")
			command.Text, stranger.Text = " /new\n", "/new"
			batch := channelproto.Batch{Messages: []channelproto.Message{command, stranger, command, followup}}
			if err := m.accept(ctx, i, i.auth, i.cfg, batch); err != nil {
				t.Fatal(err)
			}
			sends := 0
			p := &fakeTransport{call: func(_ context.Context, _ string, in, out any) error {
				sends++
				if uncertain && in.(channelproto.Send).Message.ID == "command" {
					return errors.New("uncertain send")
				}
				return assign(out, map[string]bool{"sent": true})
			}}
			if _, err := m.process(ctx, i, p, i.auth, i.cfg); (err != nil) != uncertain {
				t.Fatal(err)
			}
			restarted, err := New(ctx, m.db, m.run)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			j := restarted.items[i.id]
			if err = restarted.accept(ctx, j, j.auth, j.cfg, batch); err != nil {
				t.Fatal(err)
			}
			handled, err := restarted.process(ctx, j, p, j.auth, j.cfg)
			if err != nil || handled == uncertain || commands != 1 {
				t.Fatal(handled, err, commands)
			}
			if uncertain {
				if runs != 0 || sends != 1 {
					t.Fatal("uncertain command was replayed", runs, sends)
				}
				if err = restarted.Review(ctx, j.id, command.ID); err != nil {
					t.Fatal(err)
				}
				if _, err = restarted.process(ctx, j, p, j.auth, j.cfg); err != nil {
					t.Fatal(err)
				}
			}
			if commands != 1 || runs != 1 || sends != 2 {
				t.Fatal(commands, runs, sends)
			}
			var untouched int
			if err = m.db.QueryRow(`SELECT count(*) FROM harness_channel_threads WHERE session_id LIKE 'other-%'`).Scan(&untouched); err != nil || untouched != 3 {
				t.Fatal("unrelated mapping changed", untouched, err)
			}
			var bound string
			if err = m.db.QueryRow(`SELECT session_id FROM harness_channel_messages WHERE id='command'`).Scan(&bound); err != nil || bound != "fresh" {
				t.Fatal("missing command receipt", bound, err)
			}
		})
	}
}

func TestChannelAttentionNoticeReplacesGenericError(t *testing.T) {
	ctx := context.Background()
	m, i := fixture(t, func(_ context.Context, _, _, _, _ string, bind func(string) error) (automation.Outcome, error) {
		return automation.Outcome{Status: "needs_attention", Text: "tool summary", Attention: "web_fetch needs review; /new"}, errors.Join(bind("session"), errors.New("SECRET error"))
	})
	p := &fakeTransport{call: func(_ context.Context, _ string, in, out any) error {
		text := in.(channelproto.Send).Text
		if !strings.Contains(text, "web_fetch") || !strings.Contains(text, "tool summary") || strings.Contains(text, "Execution needs attention") || strings.Contains(text, "SECRET") {
			t.Fatal(text)
		}
		return assign(out, map[string]bool{"sent": true})
	}}
	if err := m.accept(ctx, i, i.auth, i.cfg, channelproto.Batch{Messages: []channelproto.Message{message("a", "alice")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.process(ctx, i, p, i.auth, i.cfg); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownSendBlocksReplayButNotAnotherConversation(t *testing.T) {
	ctx := context.Background()
	calls := 0
	m, i := fixture(t, func(context.Context, string, string, string, string, func(string) error) (automation.Outcome, error) {
		calls++
		return automation.Outcome{Status: "completed", Text: "reply"}, nil
	})
	p := &fakeTransport{call: func(context.Context, string, any, any) error { return errors.New("timeout after acceptance") }}
	if err := m.accept(ctx, i, i.auth, i.cfg, channelproto.Batch{Messages: []channelproto.Message{message("a", "alice"), message("b", "alice")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.process(ctx, i, p, i.auth, i.cfg); err == nil {
		t.Fatal("expected uncertain delivery")
	}
	if handled, err := m.process(ctx, i, p, i.auth, i.cfg); err != nil || handled || calls != 1 {
		t.Fatal(handled, err, calls)
	}
	m2, err := New(ctx, m.db, m.run)
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	if handled, err := m2.process(ctx, m2.items["test"], p, i.auth, i.cfg); err != nil || handled {
		t.Fatal("replayed after restart")
	}
}
func TestConfigureFencesOldWorkspaceAndChecksRevision(t *testing.T) {
	m, i := fixture(t, nil)
	ctx := context.Background()
	if err := m.accept(ctx, i, i.auth, i.cfg, channelproto.Batch{Messages: []channelproto.Message{message("a", "alice")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Configure(ctx, i.id, 99, t.TempDir(), nil, false); err == nil {
		t.Fatal("stale save accepted")
	}
	workspace := t.TempDir()
	if _, err := m.Configure(ctx, i.id, 1, workspace, nil, false); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := m.db.QueryRow(`SELECT status FROM harness_channel_messages WHERE id='a'`).Scan(&state); err != nil || state != "unknown" {
		t.Fatal(state, err)
	}
	if len(i.auth.Secret) == 0 {
		t.Fatal("disable deleted binding")
	}
	if _, err := m.Login(ctx, i.id, "logout", "", ""); err != nil {
		t.Fatal(err)
	}
	if len(i.auth.Secret) > 0 || i.cfg.Enabled {
		t.Fatal("logout retained credentials or enablement")
	}
}
func TestPairingRequiresOneTimeCodeNotFirstSender(t *testing.T) {
	m, i := fixture(t, nil)
	ctx := context.Background()
	i.auth.Owner = ""
	i.pairCode = "secret-one-time-code"
	i.pairExpires = time.Now().Add(time.Minute)
	status, err := m.Status(ctx, i.id)
	if err != nil || status.(map[string]any)["bound"] != false || status.(map[string]any)["authenticated"] != true {
		t.Fatal("token alone is not an owner binding", status, err)
	}
	stranger := message("a", "stranger")
	owner := message("b", "alice")
	owner.Text = "/pair secret-one-time-code"
	if err := m.accept(ctx, i, i.auth, i.cfg, channelproto.Batch{Messages: []channelproto.Message{stranger, owner, stranger}}); err != nil {
		t.Fatal(err)
	}
	if i.auth.Owner != "alice" || i.pairCode != "" {
		t.Fatal("owner binding failed")
	}
	var n int
	_ = m.db.QueryRow(`SELECT COUNT(*) FROM harness_channel_messages`).Scan(&n)
	if n != 0 {
		t.Fatal("pairing invoked work")
	}
}
func TestNotificationUsesHostTransactionAndAccountFence(t *testing.T) {
	m, i := fixture(t, nil)
	ctx := context.Background()
	if err := m.accept(ctx, i, i.auth, i.cfg, channelproto.Batch{Messages: []channelproto.Message{message("prior", "alice")}}); err != nil {
		t.Fatal(err)
	}
	spec := automation.Spec{Name: "test", Workspace: i.cfg.Workspace, Notify: &automation.NotificationTarget{Channel: i.id, Account: "bot", Conversation: "alice", Sender: "alice"}}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.EnqueueNotificationTx(ctx, tx, spec, automation.Occurrence{ID: "o", Outcome: automation.Outcome{Status: "completed"}}); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	tx.Rollback()
	var n int
	_ = m.db.QueryRow(`SELECT COUNT(*) FROM harness_channel_messages WHERE id='schedule-o'`).Scan(&n)
	if n != 0 {
		t.Fatal("outbox escaped transaction")
	}
	spec.Notify.Account = "other"
	if m.CanNotify(spec.Notify) {
		t.Fatal("cross-account target allowed")
	}
	spec.Notify.Account = "bot"
	spec.Workspace = t.TempDir()
	tx, err = m.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.EnqueueNotificationTx(ctx, tx, spec, automation.Occurrence{ID: "wrong-workspace"}); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = m.db.QueryRow(`SELECT COUNT(*) FROM harness_channel_messages WHERE id='schedule-wrong-workspace'`).Scan(&n); err != nil || n != 0 {
		t.Fatal("workspace change leaked notification", n, err)
	}
}
func TestLoginRedactsCredentialsAndFencesOldAttempt(t *testing.T) {
	m, i := fixture(t, nil)
	ctx := context.Background()
	i.cfg.Enabled = false
	m.start = func(context.Context, Manifest) (transport, channelproto.Descriptor, error) {
		return &fakeTransport{call: func(_ context.Context, op string, in, out any) error {
			if op == "login.start" {
				return assign(out, channelproto.Login{ID: "new", State: "pending", QR: "qr", ExpiresIn: 180})
			}
			return assign(out, channelproto.Login{ID: "new", State: "confirmed", Auth: &channelproto.Auth{Account: "new-bot", Owner: "alice", Secret: json.RawMessage(`{"token":"DO-NOT-LEAK"}`)}})
		}}, i.manifest.Descriptor, nil
	}
	if _, err := m.Login(ctx, i.id, "login.start", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Login(ctx, i.id, "login.poll", "old", ""); err == nil {
		t.Fatal("old attempt accepted")
	}
	v, err := m.Login(ctx, i.id, "login.poll", "new", "")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(v)
	if string(raw) == "" || contains(string(raw), "DO-NOT-LEAK") {
		t.Fatal("credential leak")
	}
	if i.auth.Account != "new-bot" {
		t.Fatal("binding not saved")
	}
}
func contains(s, v string) bool {
	for j := 0; j+len(v) <= len(s); j++ {
		if s[j:j+len(v)] == v {
			return true
		}
	}
	return false
}

type fixtureHandler struct{}

func (fixtureHandler) Handle(_ context.Context, method string, raw json.RawMessage) (any, error) {
	if method == "initialize" {
		return channelproto.Descriptor{ID: "fixture", Name: "Fixture", Version: "1", Protocol: 1, Login: []string{"token"}, Messages: []string{"text"}, Fields: []channelproto.Field{{Key: "token", Label: "Token", Secret: true, Required: true, Login: true}}}, nil
	}
	if method == "login.start" {
		var in struct {
			Fields map[string]string `json:"fields"`
		}
		if err := channelproto.Decode(raw, &in); err != nil || in.Fields["token"] != "fixture-token" {
			return nil, errors.New("invalid fixture token")
		}
		return channelproto.Login{State: "confirmed", Auth: &channelproto.Auth{Account: "bot", Owner: "alice", Secret: json.RawMessage(`{"token":"fixture-token"}`)}}, nil
	}
	if method == "poll" {
		return channelproto.Batch{Cursor: "committed", Messages: []channelproto.Message{message("same-message", "alice"), message("untrusted", "stranger")}}, nil
	}
	if method == "send" {
		var in channelproto.Send
		if err := channelproto.Decode(raw, &in); err != nil || in.Message.ID != "same-message" || in.Key == "" || in.Text != "fixture reply" {
			return nil, errors.New("unexpected fixture delivery")
		}
		return map[string]bool{"sent": true}, nil
	}
	if method == "sleep" {
		time.Sleep(time.Second)
	}
	return map[string]bool{"ok": true}, nil
}

func TestRealPluginTokenLoginDeliveryAndRestart(t *testing.T) {
	ctx := context.Background()
	var runs atomic.Int32
	m, old := fixture(t, func(_ context.Context, _, _, prompt, approval string, bind func(string) error) (automation.Outcome, error) {
		runs.Add(1)
		if prompt != "hello" || approval != harness.ApprovalGlobal {
			return automation.Outcome{}, errors.New("invalid host dispatch")
		}
		return automation.Outcome{Status: "completed", Text: "fixture reply"}, bind("fixture-session")
	})
	delete(m.items, old.id)
	if _, err := m.db.Exec(`DELETE FROM harness_channels`); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	manifest := Manifest{ID: "fixture", Command: exe, Args: []string{"-test.run=^TestPluginChild$", "channel-fixture"}, SHA256: hex.EncodeToString(sum[:])}
	if _, err = m.Install(ctx, "other-platform", manifest); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Configure(ctx, "other-platform", 1, t.TempDir(), nil, true); err != nil {
		t.Fatal(err)
	}
	if _, err = m.LoginWithFields(ctx, "other-platform", "login.start", "", "", map[string]string{"token": "fixture-token"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var sent int
		if err = m.db.QueryRow(`SELECT COUNT(*) FROM harness_channel_messages WHERE status='sent'`).Scan(&sent); err != nil {
			t.Fatal(err)
		}
		if sent == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture delivery did not complete")
		}
		time.Sleep(20 * time.Millisecond)
	}
	m.Close()
	restarted, err := New(ctx, m.db, m.run)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	i := restarted.items["other-platform"]
	if i.auth.Owner != "alice" || i.cursor != "committed" {
		t.Fatal("binding/cursor not persisted")
	}
	for range 2 {
		if err = restarted.step(ctx, i); err != nil {
			t.Fatal(err)
		}
	}
	if runs.Load() != 1 {
		t.Fatal("duplicate or stranger executed", runs.Load())
	}
}

func TestNotificationContextRequirementComesFromDescriptor(t *testing.T) {
	m, i := fixture(t, nil)
	for index, required := range []bool{false, true} {
		i.manifest.Descriptor.RequiresReplyContext = required
		if err := m.persist(context.Background(), i); err != nil {
			t.Fatal(err)
		}
		tx, err := m.db.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		id := []string{"no-context-required", "context-required"}[index]
		spec := automation.Spec{Workspace: i.cfg.Workspace, Notify: &automation.NotificationTarget{Channel: i.id, Account: "bot", Conversation: "alice", Sender: "alice"}}
		if err = m.EnqueueNotificationTx(context.Background(), tx, spec, automation.Occurrence{ID: id}); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		var status string
		if err = m.db.QueryRow(`SELECT status FROM harness_channel_messages WHERE id=?`, "schedule-"+id).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if (required && status != "unknown") || (!required && status != "ready") {
			t.Fatal(required, status)
		}
	}
}
func TestPluginChild(t *testing.T) {
	if len(os.Args) < 2 || os.Args[len(os.Args)-1] != "channel-fixture" {
		return
	}
	_ = channelproto.Serve(context.Background(), os.Stdin, os.Stdout, fixtureHandler{})
	os.Exit(0)
}
func TestRealProcessHandshakeTimeoutAndChecksum(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	manifest := Manifest{ID: "fixture", Command: exe, Args: []string{"-test.run=^TestPluginChild$", "channel-fixture"}, SHA256: hex.EncodeToString(sum[:])}
	p, d, err := startProcess(context.Background(), manifest)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if d.ID != "fixture" {
		t.Fatal(d)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err = p.Call(ctx, "sleep", map[string]any{}, nil); err == nil {
		t.Fatal("timeout not enforced")
	}
	manifest.SHA256 = string(make([]byte, 64))
	if _, _, err = startProcess(context.Background(), manifest); err == nil {
		t.Fatal("checksum ignored")
	}
}
func TestStopCancelsOwnedRunWithoutReplay(t *testing.T) {
	var calls atomic.Int32
	began := make(chan struct{})
	m, i := fixture(t, func(ctx context.Context, _, _, _, _ string, _ func(string) error) (automation.Outcome, error) {
		calls.Add(1)
		close(began)
		<-ctx.Done()
		return automation.Outcome{}, ctx.Err()
	})
	i.proc = &fakeTransport{call: func(context.Context, string, any, any) error { return nil }}
	if err := m.accept(context.Background(), i, i.auth, i.cfg, channelproto.Batch{Messages: []channelproto.Message{message("a", "alice")}}); err != nil {
		t.Fatal(err)
	}
	m.Start()
	select {
	case <-began:
	case <-time.After(time.Second):
		t.Fatal("run not started")
	}
	if _, err := m.Configure(context.Background(), i.id, 1, i.cfg.Workspace, nil, false); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("unexpected replay")
	}
	var state string
	_ = m.db.QueryRow(`SELECT status FROM harness_channel_messages WHERE id='a'`).Scan(&state)
	if state != "unknown" {
		t.Fatal(state)
	}
}

func TestCancelFinishedLoginDoesNotInterruptOwnedRun(t *testing.T) {
	m, i := fixture(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	i.cancel = cancel
	i.done = make(chan struct{})
	close(i.done)
	if _, err := m.Login(context.Background(), i.id, "login.cancel", "", ""); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		t.Fatal("cancelling a finished login interrupted channel work")
	}
}

func TestLoginSecretFieldsNeverPersistInConfig(t *testing.T) {
	m, i := fixture(t, nil)
	i.cfg.Enabled = false
	i.auth = channelproto.Auth{}
	i.manifest.Descriptor.Fields = []channelproto.Field{{Key: "bot_token", Label: "Token", Secret: true, Login: true, Required: true}}
	if _, err := m.Configure(context.Background(), i.id, 1, i.cfg.Workspace, nil, false); err != nil {
		t.Fatal(err)
	}
	m.start = func(context.Context, Manifest) (transport, channelproto.Descriptor, error) {
		return &fakeTransport{call: func(_ context.Context, op string, in, out any) error {
			if in.(map[string]any)["fields"].(map[string]string)["bot_token"] != "private-token" {
				t.Fatal("secret login field absent")
			}
			return assign(out, channelproto.Login{State: "confirmed", Auth: &channelproto.Auth{Account: "bot", Owner: "owner", Secret: json.RawMessage(`{"token":"private-token"}`)}})
		}}, i.manifest.Descriptor, nil
	}
	if _, err := m.LoginWithFields(context.Background(), i.id, "login.start", "", "", map[string]string{"bot_token": "private-token"}); err != nil {
		t.Fatal(err)
	}
	var config string
	_ = m.db.QueryRow(`SELECT config FROM harness_channels WHERE id=?`, i.id).Scan(&config)
	if contains(config, "private-token") {
		t.Fatal("secret leaked to config")
	}
	v, err := m.Status(context.Background(), i.id)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(v)
	if contains(string(raw), "private-token") {
		t.Fatal("secret leaked to status")
	}
}
func TestPluginUpdateRollbackAndStaleRevision(t *testing.T) {
	m, i := fixture(t, nil)
	i.cfg.Enabled = false
	i.manifest.Descriptor.Version = "1"
	i.manifest.SHA256 = "old"
	m.start = func(_ context.Context, manifest Manifest) (transport, channelproto.Descriptor, error) {
		d := manifest.Descriptor
		return &fakeTransport{call: func(context.Context, string, any, any) error { return nil }}, d, nil
	}
	next := i.manifest
	next.SHA256 = "new"
	next.Descriptor.Version = "2"
	if _, err := m.Update(context.Background(), i.id, 1, next, false); err != nil {
		t.Fatal(err)
	}
	if i.manifest.SHA256 != "new" || i.manifest.Previous.SHA256 != "old" {
		t.Fatal("rollback manifest lost")
	}
	if _, err := m.Update(context.Background(), i.id, 1, next, false); err == nil {
		t.Fatal("stale update accepted")
	}
	if _, err := m.Update(context.Background(), i.id, 2, Manifest{}, true); err != nil {
		t.Fatal(err)
	}
	if i.manifest.SHA256 != "old" {
		t.Fatal("rollback failed")
	}
}
func TestLegacyMigrationPreservesHistoryAndDoesNotReplay(t *testing.T) {
	m, i := fixture(t, nil)
	delete(m.items, "test")
	i.id = "wechat"
	m.items["wechat"] = i
	if _, err := m.db.Exec(`DELETE FROM harness_channels`); err != nil {
		t.Fatal(err)
	}
	if err := m.persist(context.Background(), i); err != nil {
		t.Fatal(err)
	}
	_, err := m.db.Exec(`CREATE TABLE harness_wechat_auth(id INTEGER PRIMARY KEY,credentials BLOB,cursor TEXT);CREATE TABLE harness_wechat_threads(bot TEXT,sender TEXT,session_id TEXT);CREATE TABLE harness_wechat_messages(bot TEXT,id TEXT,sender TEXT,context_token TEXT,text TEXT,status TEXT,reply TEXT,session_id TEXT,client_id TEXT,created_ms INTEGER);INSERT INTO harness_wechat_threads VALUES('old-bot','alice','old-session');INSERT INTO harness_wechat_messages VALUES('old-bot','old-id','alice','context','hello','sending','reply','old-session','key',1);`)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := sealCredentials([]byte(`{"bot_token":"old-secret","ilink_bot_id":"old-bot","base_url":"https://ilinkai.weixin.qq.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.db.Exec(`INSERT INTO harness_wechat_auth VALUES(1,?,'cursor')`, sealed); err != nil {
		t.Fatal(err)
	}
	if err = m.MigrateWeChat(context.Background(), Config{Workspace: i.cfg.Workspace, LegacyAllowed: []string{"alice", "bob"}}); err != nil {
		t.Fatal(err)
	}
	var state string
	_ = m.db.QueryRow(`SELECT status FROM harness_channel_messages WHERE id='old-id'`).Scan(&state)
	if state != "unknown" {
		t.Fatal("old in-flight delivery replayable")
	}
	if !allowed(i.cfg, i.auth, "alice") || !allowed(i.cfg, i.auth, "bob") || allowed(i.cfg, i.auth, "other") {
		t.Fatal("legacy policy changed")
	}
	if err = m.MigrateWeChat(context.Background(), Config{}); err != nil {
		t.Fatal(err)
	}
	if len(i.cfg.LegacyAllowed) != 2 {
		t.Fatal("migration repeated")
	}
	if _, err = m.Login(context.Background(), "wechat", "logout", "", ""); err != nil {
		t.Fatal(err)
	}
	var credentials, history int
	if err = m.db.QueryRow(`SELECT COUNT(*) FROM harness_wechat_auth`).Scan(&credentials); err != nil {
		t.Fatal(err)
	}
	if err = m.db.QueryRow(`SELECT COUNT(*) FROM harness_wechat_messages`).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if credentials != 0 || history != 1 {
		t.Fatal("logout must clear migrated credentials but retain history", credentials, history)
	}
}
