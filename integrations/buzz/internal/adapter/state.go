package adapter

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gofrs/flock"
	_ "modernc.org/sqlite"
)

type State struct {
	db    *sql.DB
	lease *flock.Flock
}

// The original Python inbox/outbox schema is retained. A single adapter owns
// it at a time, and an interrupted prompt is quarantined instead of replayed.
func OpenState(path string) (*State, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	lease := flock.New(path + ".lock")
	locked, err := lease.TryLock()
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, errors.New("another Buzz adapter owns this state database")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		_ = lease.Unlock()
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &State{db: db, lease: lease}
	ok := false
	defer func() {
		if !ok {
			s.Close()
		}
	}()
	for _, query := range []string{
		`PRAGMA busy_timeout=5000`, `PRAGMA journal_mode=WAL`,
		`CREATE TABLE IF NOT EXISTS sessions (conversation_key TEXT PRIMARY KEY, session_id TEXT NOT NULL, workspace TEXT NOT NULL, updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE IF NOT EXISTS channel_cursors (channel_id TEXT PRIMARY KEY, created_at INTEGER NOT NULL, updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE IF NOT EXISTS inbox_messages (message_key TEXT PRIMARY KEY, conversation_key TEXT NOT NULL, channel_id TEXT NOT NULL, event_id TEXT NOT NULL, created_at INTEGER NOT NULL, author_pubkey TEXT NOT NULL, kind INTEGER NOT NULL, content TEXT NOT NULL, is_dm INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL DEFAULT 'pending', attempts INTEGER NOT NULL DEFAULT 0, last_error TEXT, response_content TEXT, received_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP, completed_at TEXT)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS inbox_event_id ON inbox_messages(event_id)`,
		`CREATE TABLE IF NOT EXISTS seeded_events (event_id TEXT PRIMARY KEY)`,
	} {
		if _, err = db.Exec(query); err != nil {
			return nil, err
		}
	}
	for _, column := range []struct{ table, name, definition string }{{"inbox_messages", "tags_json", "TEXT NOT NULL DEFAULT '[]'"}, {"inbox_messages", "acknowledgement_attempted", "INTEGER NOT NULL DEFAULT 0"}, {"sessions", "backend_id", "TEXT NOT NULL DEFAULT ''"}} {
		rows, e := db.Query("PRAGMA table_info(" + column.table + ")")
		if e != nil {
			return nil, e
		}
		exists := false
		for rows.Next() {
			var cid, notnull, pk int
			var name, typ string
			var defaultValue any
			if e = rows.Scan(&cid, &name, &typ, &notnull, &defaultValue, &pk); e != nil {
				rows.Close()
				return nil, e
			}
			if name == column.name {
				exists = true
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return nil, e
		}
		if !exists {
			if _, e = db.Exec("ALTER TABLE " + column.table + " ADD COLUMN " + column.name + " " + column.definition); e != nil {
				return nil, e
			}
		}
	}
	ok = true
	return s, nil
}
func (s *State) Close() error {
	if s == nil {
		return nil
	}
	var errs []error
	if s.db != nil {
		errs = append(errs, s.db.Close())
		s.db = nil
	}
	if s.lease != nil {
		errs = append(errs, s.lease.Unlock())
		s.lease = nil
	}
	return errors.Join(errs...)
}
func (s *State) RecoverInterrupted() error {
	_, err := s.db.Exec(`UPDATE inbox_messages SET status=CASE WHEN status='processing' THEN 'uncertain' ELSE 'delivery_unknown' END, last_error='Adapter interrupted during execution or delivery; inspect the ACP session and relay before retrying' WHERE status IN ('processing','delivering')`)
	return err
}
func (s *State) Cursor(channel string) (int64, bool, error) {
	var value int64
	err := s.db.QueryRow(`SELECT created_at FROM channel_cursors WHERE channel_id=?`, channel).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return value, err == nil, err
}
func (s *State) SeedCursor(channel string, created int64) error {
	_, err := s.db.Exec(`INSERT INTO channel_cursors(channel_id,created_at) VALUES(?,?) ON CONFLICT(channel_id) DO UPDATE SET created_at=MAX(channel_cursors.created_at,excluded.created_at),updated_at=CURRENT_TIMESTAMP`, channel, created)
	return err
}

// Inbox insertion and cursor advancement are one transaction. A crash cannot
// move a cursor past messages which were never durably admitted.
func (s *State) EnqueueAndAdvance(ctx context.Context, channel string, created int64, messages []Message, scope string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, m := range messages {
		tags, e := json.Marshal(m.Tags)
		if e != nil {
			return e
		}
		if m.Tags == nil {
			tags = []byte("[]")
		}
		dm := 0
		if m.DM {
			dm = 1
		}
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO inbox_messages(message_key,conversation_key,channel_id,event_id,created_at,author_pubkey,kind,content,is_dm,tags_json) SELECT ?,?,?,?,?,?,?,?,?,? WHERE NOT EXISTS (SELECT 1 FROM seeded_events WHERE event_id=?)`, m.ID, m.conversation(scope), m.Channel, m.ID, m.Created, m.Author, m.Kind, m.Content, dm, string(tags), m.ID); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO channel_cursors(channel_id,created_at) VALUES(?,?) ON CONFLICT(channel_id) DO UPDATE SET created_at=MAX(channel_cursors.created_at,excluded.created_at),updated_at=CURRENT_TIMESTAMP`, channel, created); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *State) Seed(ctx context.Context, channel string, created int64, messages []Message) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, m := range messages {
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO seeded_events(event_id) VALUES(?)`, m.ID); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO channel_cursors(channel_id,created_at) VALUES(?,?) ON CONFLICT(channel_id) DO UPDATE SET created_at=MAX(channel_cursors.created_at,excluded.created_at)`, channel, created); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *State) Pending() ([]Pending, error) {
	rows, err := s.db.Query(`SELECT message_key,conversation_key,channel_id,event_id,created_at,author_pubkey,kind,content,is_dm,attempts,response_content,tags_json FROM inbox_messages WHERE status='pending' ORDER BY created_at,message_key LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Pending
	for rows.Next() {
		var p Pending
		var dm int
		var response sql.NullString
		var tags string
		if err = rows.Scan(&p.Key, &p.Conversation, &p.Channel, &p.ID, &p.Created, &p.Author, &p.Kind, &p.Content, &dm, &p.Attempts, &response, &tags); err != nil {
			return nil, err
		}
		p.DM = dm != 0
		if response.Valid {
			p.Response = &response.String
		}
		if json.Unmarshal([]byte(tags), &p.Tags) != nil {
			return nil, errors.New("invalid attachment tags in inbox")
		}
		result = append(result, p)
	}
	return result, rows.Err()
}
func (s *State) Session(conversation, workspace, backend string) (string, error) {
	var id, oldWorkspace, oldBackend string
	err := s.db.QueryRow(`SELECT session_id,workspace,backend_id FROM sessions WHERE conversation_key=?`, conversation).Scan(&id, &oldWorkspace, &oldBackend)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if oldWorkspace != workspace || oldBackend != backend {
		return "", nil
	}
	return id, nil
}
func (s *State) PutSession(conversation, id, workspace, backend string) error {
	_, err := s.db.Exec(`INSERT INTO sessions(conversation_key,session_id,workspace,backend_id) VALUES(?,?,?,?) ON CONFLICT(conversation_key) DO UPDATE SET session_id=excluded.session_id,workspace=excluded.workspace,backend_id=excluded.backend_id,updated_at=CURRENT_TIMESTAMP`, conversation, id, workspace, backend)
	return err
}

// Persist the acknowledgement attempt before publishing. A lost receipt or a
// restart must not repeat the reaction, and must not quarantine the ACP task.
func (s *State) ClaimAcknowledgement(ctx context.Context, key string) (bool, error) {
	r, err := s.db.ExecContext(ctx, `UPDATE inbox_messages SET acknowledgement_attempted=1 WHERE message_key=? AND status='pending' AND response_content IS NULL AND acknowledgement_attempted=0`, key)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}

func (s *State) BeginPrompt(key string) error {
	r, err := s.db.Exec(`UPDATE inbox_messages SET status='processing' WHERE message_key=? AND status='pending' AND response_content IS NULL`, key)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n != 1 {
		return errors.New("inbox message is no longer eligible for execution")
	}
	return err
}
func (s *State) SaveResponse(key, response string) error {
	r, err := s.db.Exec(`UPDATE inbox_messages SET response_content=?,status='pending' WHERE message_key=? AND status IN ('pending','processing')`, response, key)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n != 1 {
		return errors.New("inbox response could not be persisted")
	}
	return err
}
func (s *State) MarkDone(key string) error {
	_, err := s.db.Exec(`UPDATE inbox_messages SET status='done',completed_at=CURRENT_TIMESTAMP,last_error=NULL,response_content=NULL WHERE message_key=?`, key)
	return err
}
func (s *State) MarkUnknown(key, status, detail string) error {
	if status != "uncertain" && status != "delivery_unknown" {
		return errors.New("invalid quarantine state")
	}
	_, err := s.db.Exec(`UPDATE inbox_messages SET status=?,last_error=? WHERE message_key=?`, status, detail, key)
	return err
}

// Persist the send boundary so a crash after relay acceptance cannot resend.
func (s *State) BeginDelivery(key string) error {
	r, err := s.db.Exec(`UPDATE inbox_messages SET status='delivering' WHERE message_key=? AND status='pending' AND response_content IS NOT NULL`, key)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n != 1 {
		return errors.New("reply is no longer eligible for delivery")
	}
	return err
}
func (s *State) RetryDelivery(key string) error {
	_, err := s.db.Exec(`UPDATE inbox_messages SET status='pending' WHERE message_key=? AND status='delivering'`, key)
	return err
}
func (s *State) MarkFailed(key, detail string, max int) error {
	_, err := s.db.Exec(`UPDATE inbox_messages SET attempts=attempts+1,last_error=?,status=CASE WHEN attempts+1>=? THEN 'failed' ELSE status END WHERE message_key=? AND status='pending'`, detail, max, key)
	return err
}
func (s *State) Counts() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT status,COUNT(*) FROM inbox_messages GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	r := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err = rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		r[status] = n
	}
	return r, rows.Err()
}
func backendID(c Config) string {
	return digestString(fmt.Sprintf("go-acp-v2|%s|%s|%s|%s|%s", c.Deerflow.Command, c.Deerflow.ConfigPath, c.Deerflow.RuntimeDir, c.Deerflow.DataDir, strings.Join(c.Deerflow.Args, "\x00")))
}
