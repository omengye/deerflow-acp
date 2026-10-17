package wechat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/internal/automation"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func channelFixture(t *testing.T, runner Runner) (*Service, Credentials) {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := New(context.Background(), db.DB(), Config{Enabled: true, Workspace: t.TempDir(), AllowedUsers: []string{"alice", "bob"}}, runner)
	if err != nil {
		t.Fatal(err)
	}
	cred := Credentials{Token: "private", BotID: "bot", Origin: DefaultOrigin}
	raw, _ := json.Marshal(cred)
	raw, err = sealCredentials(raw)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.DB().Exec(`INSERT INTO harness_wechat_auth(id,credentials) VALUES(1,?)`, raw)
	if err != nil {
		t.Fatal(err)
	}
	return s, cred
}
func incoming(t *testing.T, id, sender string) Message {
	t.Helper()
	var m Message
	raw := `{"message_type":1,"message_id":"` + id + `","from_user_id":"` + sender + `","context_token":"reply-context","item_list":[{"type":1,"text_item":{"text":"hello"}}]}`
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	return m
}
func TestWeChatDedupeAllowlistConversationIsolation(t *testing.T) {
	ctx := context.Background()
	calls, sends := 0, 0
	var seen []string
	s, cred := channelFixture(t, func(_ context.Context, session, text string, bind func(string) error) (automation.Outcome, error) {
		calls++
		seen = append(seen, session)
		if session == "" {
			session = "session-" + string(rune('0'+calls))
		}
		if err := bind(session); err != nil {
			return automation.Outcome{}, err
		}
		return automation.Outcome{Status: "completed", SessionID: session, Text: "reply"}, nil
	})
	s.client = fakeClient(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/ilink/bot/sendmessage" {
			t.Fatal(r.URL.Path)
		}
		sends++
		return response(`{"ret":0}`), nil
	})
	batch := Updates{Cursor: "next", Messages: []Message{incoming(t, "a", "alice"), incoming(t, "b", "bob"), incoming(t, "x", "intruder")}}
	for range 2 {
		if err := s.accept(ctx, cred, batch); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		if _, err := s.process(ctx, cred); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 || sends != 2 || seen[0] != "" || seen[1] != "" {
		t.Fatal(calls, sends, seen)
	}
	if err := s.accept(ctx, cred, Updates{Cursor: "later", Messages: []Message{incoming(t, "c", "alice")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.process(ctx, cred); err != nil {
		t.Fatal(err)
	}
	if calls != 3 || seen[2] != "session-1" {
		t.Fatal(calls, seen)
	}
	_, cursor, err := s.credentials(ctx)
	if err != nil || cursor != "later" {
		t.Fatal(cursor, err)
	}
}
func TestWeChatUnknownDeliveryIsNotRetriedAndCrashIsFenced(t *testing.T) {
	ctx := context.Background()
	calls, sends := 0, 0
	s, cred := channelFixture(t, func(_ context.Context, _ string, _ string, bind func(string) error) (automation.Outcome, error) {
		calls++
		if err := bind("session"); err != nil {
			return automation.Outcome{}, err
		}
		return automation.Outcome{Status: "completed", Text: "reply"}, nil
	})
	s.client = fakeClient(func(*http.Request) (*http.Response, error) {
		sends++
		return nil, errors.New("timeout after remote acceptance")
	})
	if err := s.accept(ctx, cred, Updates{Messages: []Message{incoming(t, "a", "alice")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.process(ctx, cred); err == nil {
		t.Fatal("expected uncertain send")
	}
	if err := s.accept(ctx, cred, Updates{Messages: []Message{incoming(t, "a", "alice")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.process(ctx, cred); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || sends != 1 {
		t.Fatal("replayed uncertain work", calls, sends)
	}
	if _, err := s.db.Exec(`UPDATE harness_wechat_messages SET status='running' WHERE id='a'`); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(ctx, s.db, s.cfg, s.run)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.process(ctx, cred); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("crashed input replayed")
	}
	var status string
	if err = s.db.QueryRow(`SELECT status FROM harness_wechat_messages WHERE id='a'`).Scan(&status); err != nil || status != "unknown" {
		t.Fatal(status, err)
	}
}
func TestWeChatMissingAllowlistFailsClosed(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = New(context.Background(), db.DB(), Config{Enabled: true, Workspace: t.TempDir()}, nil); err == nil {
		t.Fatal("enabled without allowlist")
	}
}

func TestWeChatScheduledNotificationSharesOccurrenceTransaction(t *testing.T) {
	ctx := context.Background()
	s, cred := channelFixture(t, func(context.Context, string, string, func(string) error) (automation.Outcome, error) {
		t.Fatal("notification invoked model")
		return automation.Outcome{}, nil
	})
	if err := s.accept(ctx, cred, Updates{Messages: []Message{incoming(t, "prior", "alice")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE harness_wechat_messages SET status='sent' WHERE id='prior'`); err != nil {
		t.Fatal(err)
	}
	sends := 0
	s.client = fakeClient(func(r *http.Request) (*http.Response, error) {
		sends++
		var body struct {
			Msg struct {
				To      string `json:"to_user_id"`
				Context string `json:"context_token"`
			} `json:"msg"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Msg.To != "alice" || body.Msg.Context != "reply-context" {
			t.Fatal(body)
		}
		return response(`{"ret":0}`), nil
	})
	spec := automation.Spec{Name: "Daily report", NotifyWeChat: "alice"}
	occ := automation.Occurrence{ID: "occ", Outcome: automation.Outcome{Status: "completed", SessionID: "result-session", Text: "done"}}
	for _, commit := range []bool{false, true} {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.EnqueueNotificationTx(ctx, tx, spec, occ); err != nil {
			t.Fatal(err)
		}
		if commit {
			err = tx.Commit()
		} else {
			err = tx.Rollback()
		}
		if err != nil {
			t.Fatal(err)
		}
		handled, err := s.process(ctx, cred)
		if err != nil || handled != commit {
			t.Fatal(handled, err)
		}
	}
	if sends != 1 {
		t.Fatal("notification not exactly once admitted", sends)
	}
}

func TestWeChatLogoutCancelsActiveRunAndDoesNotSend(t *testing.T) {
	ctx := context.Background()
	started := make(chan struct{})
	s, cred := channelFixture(t, func(ctx context.Context, _ string, _ string, _ func(string) error) (automation.Outcome, error) {
		close(started)
		<-ctx.Done()
		return automation.Outcome{}, ctx.Err()
	})
	s.client = fakeClient(func(*http.Request) (*http.Response, error) {
		t.Error("send after logout")
		return response(`{"ret":0}`), nil
	})
	if err := s.accept(ctx, cred, Updates{Messages: []Message{incoming(t, "active", "alice")}}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _, _ = s.process(ctx, cred) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not start")
	}
	if err := s.Logout(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("logout did not cancel")
	}
	var status string
	if err := s.db.QueryRow(`SELECT status FROM harness_wechat_messages WHERE id='active'`).Scan(&status); err != nil || status != "unknown" {
		t.Fatal(status, err)
	}
}

func TestWeChatUnknownBlocksSenderUntilReviewWithoutReplay(t *testing.T) {
	s, cred := channelFixture(t, func(context.Context, string, string, func(string) error) (automation.Outcome, error) {
		return automation.Outcome{Status: "completed", Text: "ok"}, nil
	})
	ctx := context.Background()
	if err := s.accept(ctx, cred, Updates{Messages: []Message{incoming(t, "unknown", "alice"), incoming(t, "later", "alice")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE harness_wechat_messages SET status='unknown' WHERE id='unknown'`); err != nil {
		t.Fatal(err)
	}
	s.client = fakeClient(func(*http.Request) (*http.Response, error) { return response(`{"ret":0}`), nil })
	if handled, err := s.process(ctx, cred); err != nil || handled {
		t.Fatal("unknown sender not fenced", handled, err)
	}
	if err := s.Review(ctx, "unknown"); err != nil {
		t.Fatal(err)
	}
	if handled, err := s.process(ctx, cred); err != nil || !handled {
		t.Fatal(handled, err)
	}
	var status string
	if err := s.db.QueryRow(`SELECT status FROM harness_wechat_messages WHERE id='unknown'`).Scan(&status); err != nil || status != "reviewed" {
		t.Fatal(status, err)
	}
}

func TestWeChatBatchKeepsArrivalOrderInsteadOfSortingOpaqueIDs(t *testing.T) {
	ctx := context.Background()
	var prompts []string
	s, cred := channelFixture(t, func(_ context.Context, _ string, text string, _ func(string) error) (automation.Outcome, error) {
		prompts = append(prompts, text)
		return automation.Outcome{Status: "completed", Text: "ok"}, nil
	})
	s.client = fakeClient(func(*http.Request) (*http.Response, error) { return response(`{"ret":0}`), nil })
	first, second := incoming(t, "z-first", "alice"), incoming(t, "a-second", "alice")
	first.Items[0].Text.Text, second.Items[0].Text.Text = "first", "second"
	first.Context, second.Context = "old-context", "latest-context"
	if err := s.accept(ctx, cred, Updates{Messages: []Message{first, second}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE harness_wechat_messages SET created_ms=1`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if handled, err := s.process(ctx, cred); err != nil || !handled {
			t.Fatal(handled, err)
		}
	}
	if len(prompts) != 2 || prompts[0] != "first" || prompts[1] != "second" {
		t.Fatal("messages reordered", prompts)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = s.EnqueueNotificationTx(ctx, tx, automation.Spec{NotifyWeChat: "alice"}, automation.Occurrence{ID: "latest-context"}); err != nil {
		t.Fatal(err)
	}
	var token string
	if err = tx.QueryRowContext(ctx, `SELECT context_token FROM harness_wechat_messages WHERE id='schedule-latest-context'`).Scan(&token); err != nil || token != "latest-context" {
		t.Fatal("notification used stale context", token, err)
	}
}

func TestWeChatIncompletePersistenceFencesOnlyAffectedSender(t *testing.T) {
	for _, state := range []string{"unknown", "running", "sending"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			calls := 0
			s, cred := channelFixture(t, func(context.Context, string, string, func(string) error) (automation.Outcome, error) {
				calls++
				return automation.Outcome{Status: "completed", Text: "ok"}, nil
			})
			s.client = fakeClient(func(*http.Request) (*http.Response, error) { return response(`{"ret":0}`), nil })
			if err := s.accept(ctx, cred, Updates{Messages: []Message{incoming(t, "blocked", "alice"), incoming(t, "later", "alice"), incoming(t, "other", "bob")}}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`UPDATE harness_wechat_messages SET status=? WHERE id='blocked'`, state); err != nil {
				t.Fatal(err)
			}
			if handled, err := s.process(ctx, cred); err != nil || !handled {
				t.Fatal("unaffected sender blocked", handled, err)
			}
			if handled, err := s.process(ctx, cred); err != nil || handled || calls != 1 {
				t.Fatal("incomplete sender not fenced", handled, calls, err)
			}
		})
	}
}

func TestWeChatLoginRetainsVerificationAcrossWaitsAndRedirects(t *testing.T) {
	s, _ := channelFixture(t, nil)
	ctx := context.Background()
	step := 0
	s.client = fakeClient(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/ilink/bot/get_bot_qrcode" {
			return response(`{"qrcode":"qr","qrcode_img_content":"qr-content"}`), nil
		}
		step++
		if r.URL.Query().Get("verify_code") != "123456" {
			t.Error("verification code lost across polling")
		}
		if step == 1 {
			return response(`{"status":"wait"}`), nil
		}
		if step == 2 {
			return response(`{"status":"scaned_but_redirect","redirect_host":"other.weixin.qq.com"}`), nil
		}
		if r.URL.Host != "other.weixin.qq.com" {
			t.Error("validated provider redirect lost")
		}
		return response(`{"status":"need_verifycode"}`), nil
	})
	if _, err := s.LoginStart(ctx); err != nil {
		t.Fatal(err)
	}
	id := s.loginID
	for i, expected := range []string{"pending", "scanned", "verification_required"} {
		verify := ""
		if i == 0 {
			verify = "123456"
		}
		value, err := s.LoginPoll(ctx, id, verify)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(value)
		var status struct {
			Status string `json:"status"`
		}
		if err = json.Unmarshal(raw, &status); err != nil || status.Status != expected {
			t.Fatal("unexpected normalized state", status, err)
		}
	}
	if s.loginVerify != "" {
		t.Fatal("rejected verification code retained")
	}
	s.client = fakeClient(func(*http.Request) (*http.Response, error) { return response(`{"status":"verify_code_blocked"}`), nil })
	if _, err := s.LoginPoll(ctx, id, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoginPoll(ctx, id, ""); err == nil {
		t.Fatal("terminal login remained active")
	}
}
