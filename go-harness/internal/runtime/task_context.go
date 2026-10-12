package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

var taskNoteKey = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,47}$`)

func migrateTaskContext(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS harness_task_notes(session_id TEXT NOT NULL REFERENCES harness_sessions(id), name TEXT NOT NULL, content TEXT NOT NULL, revision INTEGER NOT NULL, PRIMARY KEY(session_id,name))`)
	return err
}

func taskContextAccess(ctx context.Context, tx *sql.Tx, req harness.RunRequest) error {
	var valid bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_runs WHERE id=? AND session_id=? AND input_id=? AND status='running')`, req.RunID, req.Session.ID, req.InputID).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return harness.ErrPermissionDenied
	}
	if attempt, ok := ctx.Value(executionAttemptKey{}).(*executionAttempt); ok {
		_, err = checkExecutionLease(ctx, tx, attempt.lease)
	}
	if attempt, ok := ctx.Value(backgroundInteractionAttemptKey{}).(*BackgroundInteractionAttempt); ok && err == nil {
		err = attempt.check(ctx, tx, true)
	}
	return err
}

func (s *Store) TaskNotes(ctx context.Context, req harness.RunRequest) ([]harness.TaskNote, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = taskContextAccess(ctx, tx, req); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT name,content,revision FROM harness_task_notes WHERE session_id=? ORDER BY name LIMIT 64`, req.Session.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []harness.TaskNote{}
	for rows.Next() {
		var note harness.TaskNote
		if err = rows.Scan(&note.Key, &note.Content, &note.Revision); err != nil {
			return nil, err
		}
		out = append(out, note)
	}
	return out, rows.Err()
}

func (s *Store) SetTaskNote(ctx context.Context, req harness.RunRequest, key, content string, expected int64) (note harness.TaskNote, err error) {
	if !taskNoteKey.MatchString(key) || !utf8.ValidString(content) || len(content) > 3000 || expected < 0 {
		return note, harness.MarkToolNotExecuted(fmt.Errorf("%w: note key must be 1..48 lowercase ASCII letters/digits/_/-; content <=3000 UTF-8 bytes; revision >=0", harness.ErrInvalidInput))
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return note, err
	}
	defer tx.Rollback()
	if err = taskContextAccess(ctx, tx, req); err != nil {
		return note, harness.MarkToolNotExecuted(err)
	}
	var old string
	var revision int64
	err = tx.QueryRowContext(ctx, `SELECT content,revision FROM harness_task_notes WHERE session_id=? AND name=?`, req.Session.ID, key).Scan(&old, &revision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return note, err
	}
	if expected != revision {
		return note, harness.MarkToolNotExecuted(errors.New("note_version_conflict: task_note action=list, then use current revision"))
	}
	var active, total int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN content<>'' THEN 1 ELSE 0 END),0) FROM harness_task_notes WHERE session_id=?`, req.Session.ID).Scan(&total, &active); err != nil {
		return note, err
	}
	if revision == 0 && total >= 64 || old == "" && content != "" && active >= 8 {
		return note, harness.MarkToolNotExecuted(errors.New("task note capacity: at most 8 active notes and 64 named slots; reuse an existing key"))
	}
	note = harness.TaskNote{Key: key, Content: content, Revision: revision + 1}
	_, err = tx.ExecContext(ctx, `INSERT INTO harness_task_notes(session_id,name,content,revision) VALUES(?,?,?,?) ON CONFLICT(session_id,name) DO UPDATE SET content=excluded.content,revision=excluded.revision`, req.Session.ID, key, content, note.Revision)
	if err != nil {
		return note, err
	}
	return note, tx.Commit()
}

func historyText(e harness.RunEvent) (string, string) {
	if e.AgentName != "" && e.AgentName != "deerflow" {
		return "", ""
	}
	var role string
	switch e.Kind {
	case "user_message":
		role = "user"
	case "text_delta":
		role = "assistant"
	case "tool_end":
		if e.ToolName == "task_note" || e.ToolName == "history_read" || e.ToolName == "history_search" {
			return "", ""
		}
		role = "tool"
	default:
		return "", ""
	}
	var b strings.Builder
	b.WriteString(e.Text)
	for _, c := range e.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return role, b.String()
}

func historyHorizon(ctx context.Context, tx *sql.Tx, req harness.RunRequest) (int64, error) {
	if err := taskContextAccess(ctx, tx, req); err != nil {
		return 0, err
	}
	var horizon int64
	// Include completed observations from long first turns too. Pagination's
	// descending cursor excludes later inserts; individual sources are immutable.
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0) FROM harness_events WHERE session_id=?`, req.Session.ID).Scan(&horizon)
	return horizon, err
}

func historyChunk(id int64, role, text string, offset, size int) (harness.TaskHistoryEntry, error) {
	runes := []rune(text)
	if offset < 0 || offset > len(runes) {
		return harness.TaskHistoryEntry{}, harness.ErrInvalidInput
	}
	end := min(len(runes), offset+size)
	out := harness.TaskHistoryEntry{ID: id, Role: role, Text: string(runes[offset:end]), Offset: offset, Truncated: end < len(runes)}
	if out.Truncated {
		out.NextOffset = end
	}
	return out, nil
}

func (s *Store) SearchTaskHistory(ctx context.Context, req harness.RunRequest, query, role string, before int64) (page harness.TaskHistoryPage, err error) {
	query = strings.TrimSpace(query)
	if query == "" || len(query) > 512 || !utf8.ValidString(query) || before < 0 || role != "" && role != "user" && role != "assistant" && role != "tool" {
		return page, harness.ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	page.Through, err = historyHorizon(ctx, tx, req)
	if err != nil {
		return page, err
	}
	if before == 0 || before > page.Through+1 {
		before = page.Through + 1
	}
	rows, err := tx.QueryContext(ctx, `SELECT sequence,event FROM harness_events WHERE session_id=? AND sequence<? AND sequence<=? ORDER BY sequence DESC LIMIT 257`, req.Session.ID, before, page.Through)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	page.Entries = []harness.TaskHistoryEntry{}
	count, bytesRead := 0, 0
	for rows.Next() {
		var id int64
		var raw []byte
		if err = rows.Scan(&id, &raw); err != nil {
			return page, err
		}
		if count >= 256 || bytesRead >= 2<<20 || len(page.Entries) >= 8 {
			page.NextBefore = id + 1
			break
		}
		count++
		bytesRead += len(raw)
		var event harness.RunEvent
		if err = json.Unmarshal(raw, &event); err != nil {
			return page, err
		}
		r, text := historyText(event)
		if r == "" || role != "" && r != role {
			continue
		}
		// Literal Unicode matching has stable source offsets without regex or
		// case-fold expansion. Keep the entire bounded query in the excerpt.
		index := strings.Index(text, query)
		if index < 0 {
			continue
		}
		padding := min(120, 600-utf8.RuneCountInString(query))
		offset := max(0, utf8.RuneCountInString(text[:index])-padding)
		entry, e := historyChunk(id, r, text, offset, 600)
		if e != nil {
			return page, e
		}
		page.Entries = append(page.Entries, entry)
	}
	return page, rows.Err()
}

func (s *Store) ReadTaskHistory(ctx context.Context, req harness.RunRequest, id int64, offset int) (out harness.TaskHistoryEntry, err error) {
	if id <= 0 || offset < 0 {
		return out, harness.ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	horizon, err := historyHorizon(ctx, tx, req)
	if err != nil {
		return out, err
	}
	var raw []byte
	err = tx.QueryRowContext(ctx, `SELECT event FROM harness_events WHERE session_id=? AND sequence=? AND sequence<=?`, req.Session.ID, id, horizon).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return out, harness.ErrNotFound
	}
	if err != nil {
		return out, err
	}
	var event harness.RunEvent
	if err = json.Unmarshal(raw, &event); err != nil {
		return out, err
	}
	role, text := historyText(event)
	if role == "" {
		return out, harness.ErrNotFound
	}
	return historyChunk(id, role, text, offset, 4000)
}
