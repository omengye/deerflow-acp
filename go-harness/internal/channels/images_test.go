package channels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/channelproto"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/automation"
)

func imageRef(n int) harness.AssetRef {
	return harness.AssetRef{ID: fmt.Sprintf("image-%d", n), SessionID: "session", SHA256: fmt.Sprintf("%064d", n), Size: 80, MimeType: "image/png", Kind: harness.AssetImage}
}

func imageFixture(t *testing.T, text string) (*Manager, *instance, *int) {
	t.Helper()
	runs := 0
	m, i := fixture(t, func(_ context.Context, _, _, _, _ string, bind func(string) error) (automation.Outcome, error) {
		runs++
		return automation.Outcome{SessionID: "session", Status: "completed", Text: text, Images: []harness.AssetRef{imageRef(1), imageRef(2)}}, bind("session")
	})
	i.manifest.Descriptor.OutboundImages = true
	if err := m.persist(t.Context(), i); err != nil {
		t.Fatal(err)
	}
	m.ResolveImage = func(_ context.Context, session, workspace string, ref harness.AssetRef) (channelproto.Image, error) {
		if session != "session" || workspace != i.cfg.Workspace || ref.SessionID != session {
			t.Fatal("image crossed identity boundary")
		}
		return channelproto.Image{Path: ref.ID, SHA256: ref.SHA256, Size: ref.Size, MimeType: ref.MimeType}, nil
	}
	return m, i, &runs
}

func imageState(t *testing.T, m *Manager, position int) string {
	t.Helper()
	var state string
	if err := m.db.QueryRow(`SELECT status FROM harness_channel_images WHERE id='a' AND position=?`, position).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func messageState(t *testing.T, m *Manager) string {
	t.Helper()
	var state string
	if err := m.db.QueryRow(`SELECT status FROM harness_channel_messages WHERE id='a'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestImagesPersistWithReplyAndDeliverOnceAfterRestart(t *testing.T) {
	for _, text := range []string{"answer", ""} {
		t.Run("text="+text, func(t *testing.T) {
			m, i, runs := imageFixture(t, text)
			msg := message("a", "alice")
			ctx := t.Context()
			if err := m.accept(ctx, i, i.auth, i.cfg, channelproto.Batch{Messages: []channelproto.Message{msg}}); err != nil {
				t.Fatal(err)
			}
			if _, err := m.db.Exec(`UPDATE harness_channel_messages SET status='running' WHERE id='a'`); err != nil {
				t.Fatal(err)
			}
			if err := m.execute(ctx, ctx, i, i.auth, i.cfg, msg, ""); err != nil {
				t.Fatal(err)
			}
			if messageState(t, m) != "ready" || imageState(t, m, 0) != "pending" {
				t.Fatal("reply not durably ready")
			}
			// Recreate the manager while no worker is running, as after a restart.
			reopened, err := New(ctx, m.db, m.run)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			reopened.ResolveImage = m.ResolveImage
			loaded := reopened.items[i.id]
			var sent []channelproto.Send
			p := &fakeTransport{call: func(_ context.Context, method string, in, out any) error {
				if method != "send" {
					t.Fatal(method)
				}
				sent = append(sent, in.(channelproto.Send))
				return assign(out, map[string]bool{"sent": true})
			}}
			for range 2 {
				if _, err = reopened.process(ctx, loaded, p, loaded.auth, loaded.cfg); err != nil {
					t.Fatal(err)
				}
			}
			imageStart := 0
			if text != "" {
				imageStart = 1
			}
			if len(sent) != 2+imageStart || *runs != 1 {
				t.Fatal("replayed run or reply", len(sent), *runs)
			}
			if text != "" && (sent[0].Text != text || sent[0].Image != nil) {
				t.Fatal("text changed")
			}
			for n, send := range sent[imageStart:] {
				if send.Image == nil || send.Image.SHA256 != imageRef(n+1).SHA256 || send.Text != "" || !strings.HasSuffix(send.Key, fmt.Sprintf("-image-%d", n)) || send.Message.Context != "reply-token" || send.Message.Sender != "alice" {
					t.Fatal("wrong attachment", send)
				}
				if imageState(t, m, n) != "sent" {
					t.Fatal("missing image receipt")
				}
			}
			if messageState(t, m) != "sent" {
				t.Fatal("reply incomplete")
			}
		})
	}
}

func TestImageFailuresPreserveTextAndNeverReplayAmbiguousSends(t *testing.T) {
	for _, mode := range []string{"unsupported", "unavailable", "wrong_session", "upload_failed", "ambiguous", "unacknowledged", "expired"} {
		t.Run(mode, func(t *testing.T) {
			m, i, runs := imageFixture(t, "answer")
			if mode == "unsupported" {
				i.manifest.Descriptor.OutboundImages = false
			}
			if mode == "unavailable" {
				m.ResolveImage = func(context.Context, string, string, harness.AssetRef) (channelproto.Image, error) {
					return channelproto.Image{}, errors.New("PRIVATE-PATH")
				}
			}
			if mode == "wrong_session" {
				m.run = func(context.Context, string, string, string, string, func(string) error) (automation.Outcome, error) {
					*runs++
					return automation.Outcome{Text: "answer", Status: "completed", Images: []harness.AssetRef{imageRef(1)}}, nil
				}
			}
			var sent []channelproto.Send
			imageCalls := 0
			p := &fakeTransport{call: func(_ context.Context, method string, in, out any) error {
				send := in.(channelproto.Send)
				sent = append(sent, send)
				if send.Image != nil {
					imageCalls++
					if imageCalls == 2 || mode == "upload_failed" {
						switch mode {
						case "upload_failed":
							return channelproto.ErrImageNotSent
						case "ambiguous":
							return errors.New("connection lost after send")
						case "unacknowledged":
							return assign(out, map[string]bool{"sent": false})
						case "expired":
							return channelproto.ErrAuthExpired
						}
					}
				}
				return assign(out, map[string]bool{"sent": true})
			}}
			ctx := t.Context()
			if err := m.accept(ctx, i, i.auth, i.cfg, channelproto.Batch{Messages: []channelproto.Message{message("a", "alice")}}); err != nil {
				t.Fatal(err)
			}
			_, err := m.process(ctx, i, p, i.auth, i.cfg)
			ambiguous := mode == "ambiguous" || mode == "unacknowledged" || mode == "expired"
			if (err != nil) != ambiguous || sent[0].Text != "answer" {
				t.Fatal(err, sent)
			}
			if ambiguous {
				if messageState(t, m) != "unknown" || imageState(t, m, 0) != "sent" || imageState(t, m, 1) != "unknown" {
					t.Fatal("partial send lost its receipts")
				}
			} else {
				if messageState(t, m) != "sent" || imageState(t, m, 0) != "failed" || !strings.Contains(sent[len(sent)-1].Text, "未发送成功") || strings.Contains(sent[len(sent)-1].Text, "PRIVATE") {
					t.Fatal("missing safe failure notice")
				}
			}
			reopened, err := New(ctx, m.db, m.run)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			reopened.ResolveImage = m.ResolveImage
			loaded := reopened.items[i.id]
			before := len(sent)
			if _, err = reopened.process(ctx, loaded, p, loaded.auth, loaded.cfg); err != nil || len(sent) != before || *runs != 1 {
				t.Fatal("restart replayed delivery", err, len(sent), *runs)
			}
		})
	}
}

func TestImageQueueCommitsAtomicallyAndScheduledImagesUseSameOutbox(t *testing.T) {
	m, i, _ := imageFixture(t, "answer")
	ctx := t.Context()
	if err := m.accept(ctx, i, i.auth, i.cfg, channelproto.Batch{Messages: []channelproto.Message{message("prior", "alice")}}); err != nil {
		t.Fatal(err)
	}
	spec := automation.Spec{Name: "draw", Workspace: i.cfg.Workspace, Notify: &automation.NotificationTarget{Channel: i.id, Account: "bot", Conversation: "alice", Sender: "alice"}}
	occ := automation.Occurrence{ID: "drawing", Outcome: automation.Outcome{Status: "completed", SessionID: "session", Images: []harness.AssetRef{imageRef(1)}}}
	for _, commit := range []bool{false, true, true} {
		tx, err := m.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = m.EnqueueNotificationTx(ctx, tx, spec, occ); err != nil {
			tx.Rollback()
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
		var count int
		if err = m.db.QueryRow(`SELECT COUNT(*) FROM harness_channel_images WHERE id='schedule-drawing'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		want := 0
		if commit {
			want = 1
		}
		if count != want {
			t.Fatal("outbox lost atomicity or dedupe", count)
		}
	}
	var raw []byte
	if err := m.db.QueryRow(`SELECT asset FROM harness_channel_images WHERE id='schedule-drawing'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var saved harness.AssetRef
	if json.Unmarshal(raw, &saved) != nil || saved != imageRef(1) {
		t.Fatal("artifact reference was not persisted")
	}
}

func TestImageInFlightRestartRequiresReview(t *testing.T) {
	m, i, _ := imageFixture(t, "answer")
	ctx := t.Context()
	msg := message("a", "alice")
	if err := m.accept(ctx, i, i.auth, i.cfg, channelproto.Batch{Messages: []channelproto.Message{msg}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.db.Exec(`UPDATE harness_channel_messages SET status='running' WHERE id='a'`); err != nil {
		t.Fatal(err)
	}
	if err := m.execute(ctx, ctx, i, i.auth, i.cfg, msg, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.db.Exec(`UPDATE harness_channel_messages SET status='sending' WHERE id='a'; UPDATE harness_channel_images SET status='sending' WHERE position=0`); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(ctx, m.db, m.run)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if messageState(t, m) != "unknown" || imageState(t, m, 0) != "unknown" || imageState(t, m, 1) != "pending" {
		t.Fatal("restart discarded delivery fence")
	}
}
