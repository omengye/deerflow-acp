package runtime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func taskContextFixture(t *testing.T) (*Store, *sqlite.Store, harness.RunRequest) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := NewStore(ctx, db.DB())
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.CreateSession(ctx, t.TempDir(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	req := harness.RunRequest{Session: session, RunID: NewID(), InputID: NewID(), Input: []harness.Content{{Type: "text", Text: "current"}}}
	return s, db, req
}

func TestTaskNotesCASCapacityAndScope(t *testing.T) {
	s, _, req := taskContextFixture(t)
	ctx := context.Background()
	if err := s.BeginRun(ctx, req); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := s.SetTaskNote(ctx, req, fmt.Sprintf("note%d", i), "fact", 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SetTaskNote(ctx, req, "ninth", "overflow", 0); err == nil {
		t.Fatal("capacity bypass")
	}
	if _, err := s.SetTaskNote(ctx, req, "note0", "stale", 0); err == nil {
		t.Fatal("CAS bypass")
	}
	note, err := s.SetTaskNote(ctx, req, "note0", "", 1)
	if err != nil || note.Revision != 2 {
		t.Fatal(note, err)
	}
	if _, err = s.SetTaskNote(ctx, req, "ninth", "new", 0); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetTaskNote(ctx, req, "note0", "revive", 0); err == nil {
		t.Fatal("tombstone reset")
	}
	forged := req
	forged.Session.ID = "other"
	if _, err = s.TaskNotes(ctx, forged); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, req.RunID, "end_turn", nil); err != nil {
		t.Fatal(err)
	}
	if _, err = s.TaskNotes(ctx, req); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("terminal run still authorized", err)
	}
	req.RunID, req.InputID = NewID(), NewID()
	if err = s.BeginRun(ctx, req); err != nil {
		t.Fatal(err)
	}
	notes, err := s.TaskNotes(ctx, req)
	if err != nil || len(notes) != 9 {
		t.Fatal(notes, err)
	}
}

func TestTaskHistoryScopeHorizonAndUnicode(t *testing.T) {
	s, _, req := taskContextFixture(t)
	ctx := context.Background()
	text := strings.Repeat("x", 800) + "\u4e2d\u6587" + strings.Repeat("y", 5000)
	event, err := s.Append(ctx, harness.RunEvent{SessionID: req.Session.ID, Kind: "user_message", Content: []harness.Content{{Type: "text", Text: text}}})
	if err != nil {
		t.Fatal(err)
	}
	private, err := s.Append(ctx, harness.RunEvent{SessionID: req.Session.ID, Kind: "reasoning_delta", Text: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BeginRun(ctx, req); err != nil {
		t.Fatal(err)
	}
	later, err := s.Append(ctx, harness.RunEvent{SessionID: req.Session.ID, Kind: "text_delta", Text: "future"})
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.SearchTaskHistory(ctx, req, "\u4e2d\u6587", "user", 0)
	if err != nil || len(page.Entries) != 1 {
		t.Fatal(page, err)
	}
	hit := page.Entries[0]
	if hit.ID != event.Sequence || hit.Offset != 680 {
		t.Fatal(hit)
	}
	read, err := s.ReadTaskHistory(ctx, req, hit.ID, hit.Offset)
	if err != nil || !strings.HasPrefix(read.Text, hit.Text) || !read.Truncated || read.NextOffset != 4680 {
		t.Fatal(read, err)
	}
	current, err := s.ReadTaskHistory(ctx, req, later.Sequence, 0)
	if err != nil || current.Text != "future" {
		t.Fatal("current run observation unavailable", current, err)
	}
	for _, id := range []int64{private.Sequence} {
		if _, err = s.ReadTaskHistory(ctx, req, id, 0); !errors.Is(err, harness.ErrNotFound) {
			t.Fatal("private/future access", id, err)
		}
	}
	other, err := s.CreateSession(ctx, t.TempDir(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := s.Append(ctx, harness.RunEvent{SessionID: other.ID, Kind: "text_delta", Text: "private"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReadTaskHistory(ctx, req, foreign.Sequence, 0); !errors.Is(err, harness.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestTaskHistoryBoundedScanAndNoteDeletion(t *testing.T) {
	s, db, req := taskContextFixture(t)
	ctx := context.Background()
	first, err := s.Append(ctx, harness.RunEvent{SessionID: req.Session.ID, Kind: "text_delta", Text: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 260; i++ {
		if _, err = s.Append(ctx, harness.RunEvent{SessionID: req.Session.ID, Kind: "text_delta", Text: "other"}); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.BeginRun(ctx, req); err != nil {
		t.Fatal(err)
	}
	page, err := s.SearchTaskHistory(ctx, req, "needle", "assistant", 0)
	if err != nil || len(page.Entries) != 0 || page.NextBefore == 0 {
		t.Fatal(page, err)
	}
	page, err = s.SearchTaskHistory(ctx, req, "needle", "assistant", page.NextBefore)
	if err != nil || len(page.Entries) != 1 || page.Entries[0].ID != first.Sequence {
		t.Fatal(page, err)
	}
	if _, err = s.SetTaskNote(ctx, req, "note", "private", 0); err != nil {
		t.Fatal(err)
	}
	if err = s.Finish(ctx, req.RunID, "end_turn", nil); err != nil {
		t.Fatal(err)
	}
	alreadyDeleted, _, err := s.purgeSession(ctx, req.Session.ID, false)
	if err != nil || alreadyDeleted {
		t.Fatal(alreadyDeleted, err)
	}
	var count int
	if err = db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM harness_task_notes`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}

func TestTaskNotesConcurrentCAS(t *testing.T) {
	s, _, req := taskContextFixture(t)
	ctx := context.Background()
	if err := s.BeginRun(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetTaskNote(ctx, req, "decision", "base", 0); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, text := range []string{"one", "two"} {
		wg.Add(1)
		go func(text string) {
			defer wg.Done()
			<-start
			_, err := s.SetTaskNote(ctx, req, "decision", text, 1)
			results <- err
		}(text)
	}
	close(start)
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent winners=%d", winners)
	}
	notes, err := s.TaskNotes(ctx, req)
	if err != nil || len(notes) != 1 || notes[0].Revision != 2 {
		t.Fatal(notes, err)
	}
}
