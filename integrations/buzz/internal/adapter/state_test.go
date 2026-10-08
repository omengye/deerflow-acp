package adapter

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func testMessage(t *testing.T, id string, created int64) Message {
	c := testConfig(t)
	return Message{ID: strings.Repeat(id, 64), Author: c.Buzz.AllowedPubkeys[0], Created: created, Kind: 9, Content: "hello", Channel: "channel", Tags: [][]string{{"p", c.Buzz.AgentPubkey}}}
}
func testState(t *testing.T) *State {
	t.Helper()
	s, e := OpenState(filepath.Join(t.TempDir(), "state.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func TestStateMigrationAndBackendBoundary(t *testing.T) {
	p := filepath.Join(t.TempDir(), "legacy.db")
	db, e := sql.Open("sqlite", p)
	if e != nil {
		t.Fatal(e)
	}
	_, e = db.Exec(`CREATE TABLE sessions(conversation_key TEXT PRIMARY KEY,session_id TEXT NOT NULL,workspace TEXT NOT NULL,updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP); INSERT INTO sessions(conversation_key,session_id,workspace) VALUES('channel:c','python-session','workspace')`)
	if e != nil {
		t.Fatal(e)
	}
	// A pre-acknowledgement inbox already contains durable pending work.
	_, e = db.Exec(`CREATE TABLE inbox_messages (message_key TEXT PRIMARY KEY, conversation_key TEXT NOT NULL, channel_id TEXT NOT NULL, event_id TEXT NOT NULL, created_at INTEGER NOT NULL, author_pubkey TEXT NOT NULL, kind INTEGER NOT NULL, content TEXT NOT NULL, is_dm INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL DEFAULT 'pending', attempts INTEGER NOT NULL DEFAULT 0, last_error TEXT, response_content TEXT, received_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP, completed_at TEXT)`)
	if e != nil {
		t.Fatal(e)
	}
	m := testMessage(t, "a", 10)
	_, e = db.Exec(`INSERT INTO inbox_messages(message_key,conversation_key,channel_id,event_id,created_at,author_pubkey,kind,content) VALUES(?,?,?,?,?,?,?,?)`, m.ID, m.conversation("thread"), m.Channel, m.ID, m.Created, m.Author, m.Kind, m.Content)
	if e != nil {
		t.Fatal(e)
	}
	db.Close()
	s, e := OpenState(p)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if id, e := s.Session("channel:c", "workspace", "go"); e != nil || id != "" {
		t.Fatalf("legacy session crossed backend: %q %v", id, e)
	}
	if e = s.PutSession("channel:c", "go-session", "workspace", "go"); e != nil {
		t.Fatal(e)
	}
	if id, e := s.Session("channel:c", "workspace", "go"); e != nil || id != "go-session" {
		t.Fatal(id, e)
	}
	if _, e = OpenState(p); e == nil {
		t.Fatal("second writer got lock")
	}
	if claimed, e := s.ClaimAcknowledgement(context.Background(), m.ID); e != nil || !claimed {
		t.Fatal("legacy pending message could not be acknowledged", claimed, e)
	}
	if claimed, e := s.ClaimAcknowledgement(context.Background(), m.ID); e != nil || claimed {
		t.Fatal("reaction claimed twice", claimed, e)
	}
	pending, e := s.Pending()
	if e != nil || len(pending) != 1 || pending[0].ID != m.ID || pending[0].Content != m.Content {
		t.Fatal("migration or acknowledgement lost pending work", pending, e)
	}
}
func TestStateDedupAndInterruptedBoundaries(t *testing.T) {
	s := testState(t)
	m1, m2 := testMessage(t, "a", 10), testMessage(t, "b", 11)
	if e := s.EnqueueAndAdvance(context.Background(), "channel", 11, []Message{m1, m2, m1}, "thread"); e != nil {
		t.Fatal(e)
	}
	pending, e := s.Pending()
	if e != nil || len(pending) != 2 {
		t.Fatal(len(pending), e)
	}
	if e = s.BeginPrompt(m1.ID); e != nil {
		t.Fatal(e)
	}
	if e = s.SaveResponse(m2.ID, "saved"); e != nil {
		t.Fatal(e)
	}
	if e = s.BeginDelivery(m2.ID); e != nil {
		t.Fatal(e)
	}
	if e = s.RecoverInterrupted(); e != nil {
		t.Fatal(e)
	}
	counts, e := s.Counts()
	if e != nil || counts["uncertain"] != 1 || counts["delivery_unknown"] != 1 {
		t.Fatal(counts, e)
	}
	pending, e = s.Pending()
	if e != nil || len(pending) != 0 {
		t.Fatal("interrupted task replayed")
	}
}
func TestStateCursorAtomicRollbackAndSeed(t *testing.T) {
	s := testState(t)
	m := testMessage(t, "a", 50)
	if e := s.Seed(context.Background(), "channel", 50, []Message{m}); e != nil {
		t.Fatal(e)
	}
	if e := s.EnqueueAndAdvance(context.Background(), "channel", 51, []Message{m}, "thread"); e != nil {
		t.Fatal(e)
	}
	p, _ := s.Pending()
	if len(p) != 0 {
		t.Fatal("bootstrap message replayed")
	}
	_, e := s.db.Exec(`CREATE TRIGGER fail_cursor BEFORE UPDATE ON channel_cursors BEGIN SELECT RAISE(ABORT,'fixture'); END`)
	if e != nil {
		t.Fatal(e)
	}
	m.ID = strings.Repeat("b", 64)
	if e = s.EnqueueAndAdvance(context.Background(), "channel", 99, []Message{m}, "thread"); e == nil {
		t.Fatal("expected rollback")
	}
	p, _ = s.Pending()
	cursor, _, _ := s.Cursor("channel")
	if len(p) != 0 || cursor != 51 {
		t.Fatal("transaction partially committed", cursor, len(p))
	}
}
func TestSavedReplyPreservesRetries(t *testing.T) {
	s := testState(t)
	m := testMessage(t, "a", 1)
	_ = s.EnqueueAndAdvance(context.Background(), m.Channel, 1, []Message{m}, "thread")
	_ = s.BeginPrompt(m.ID)
	_ = s.SaveResponse(m.ID, "answer")
	if e := s.BeginPrompt(m.ID); e == nil {
		t.Fatal("saved task executed twice")
	}
	_ = s.BeginDelivery(m.ID)
	_ = s.RetryDelivery(m.ID)
	p, e := s.Pending()
	if e != nil || len(p) != 1 || p[0].Response == nil || *p[0].Response != "answer" || p[0].Attempts != 0 {
		t.Fatal(p, e)
	}
	_ = s.MarkFailed(m.ID, "bad", 1)
	p, _ = s.Pending()
	if len(p) != 0 {
		t.Fatal("attempt budget ignored")
	}
	if !errors.Is(s.Close(), nil) {
		t.Fatal("close failed")
	}
}
