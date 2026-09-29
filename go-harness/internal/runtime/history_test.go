package runtime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestHistoryPagesPinSnapshotAndSession(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := NewStore(ctx, db.DB())
	if err != nil {
		t.Fatal(err)
	}
	x, err := s.CreateSession(ctx, t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateSession(ctx, t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 305 {
		if _, err := s.Append(ctx, harness.RunEvent{SessionID: x.ID, Kind: "text_delta", Text: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
		if i%3 == 0 {
			if _, err := s.Append(ctx, harness.RunEvent{SessionID: other.ID, Kind: "text_delta", Text: "other"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	first, err := s.HistoryPage(ctx, x.ID, "", 128)
	if err != nil || len(first.Events) != 128 || first.NextCursor == "" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	if _, err := s.HistoryPage(ctx, other.ID, first.NextCursor, 128); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatalf("cross-session cursor: %v", err)
	}
	if _, err := s.Append(ctx, harness.RunEvent{SessionID: x.ID, Kind: "text_delta", Text: "later"}); err != nil {
		t.Fatal(err)
	}
	events := append([]harness.RunEvent(nil), first.Events...)
	cursor := first.NextCursor
	for cursor != "" {
		page, err := s.HistoryPage(ctx, x.ID, cursor, 73)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, page.Events...)
		cursor = page.NextCursor
	}
	if len(events) != 305 {
		t.Fatalf("snapshot count=%d", len(events))
	}
	for i, event := range events {
		if event.SessionID != x.ID || event.Text != fmt.Sprint(i) {
			t.Fatalf("event %d=%+v", i, event)
		}
		if i > 0 && events[i-1].Sequence >= event.Sequence {
			t.Fatal("non-increasing sequence")
		}
	}
	all, err := s.HistoryPage(ctx, x.ID, "", 500)
	if err != nil || len(all.Events) != 306 || all.Events[305].Text != "later" {
		t.Fatalf("new snapshot count=%d err=%v", len(all.Events), err)
	}
	service := NewService(s, nil, "test")
	if _, err := service.Coordinator.Attach(x.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.HistoryPage(ctx, "stranger", x.ID, "", 128); !errors.Is(err, harness.ErrNotAttached) {
		t.Fatalf("owner check: %v", err)
	}
	_, release, err := service.Coordinator.Begin(ctx, x.ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.HistoryPage(ctx, "owner", x.ID, "", 128); !errors.Is(err, harness.ErrBusy) {
		t.Fatalf("busy check: %v", err)
	}
	release()
	var replay []harness.RunEvent
	_, err = service.Load(ctx, "owner", x.ID, x.CWD, true, func(_ context.Context, e harness.RunEvent) error { replay = append(replay, e); return nil })
	if err != nil || len(replay) != 306 {
		t.Fatalf("paged load count=%d err=%v", len(replay), err)
	}
}

func TestHistoryPagesBoundBytesWithoutSkippingLargeEvent(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, err := NewStore(ctx, db.DB())
	if err != nil {
		t.Fatal(err)
	}
	x, err := s.CreateSession(ctx, t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{strings.Repeat("a", maxHistoryPageBytes+1), "last"} {
		if _, err := s.Append(ctx, harness.RunEvent{SessionID: x.ID, Text: value}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.HistoryPage(ctx, x.ID, "", 128)
	if err != nil || len(page.Events) != 1 || page.NextCursor == "" {
		t.Fatalf("large page count=%d err=%v", len(page.Events), err)
	}
	page, err = s.HistoryPage(ctx, x.ID, page.NextCursor, 128)
	if err != nil || len(page.Events) != 1 || page.Events[0].Text != "last" || page.NextCursor != "" {
		t.Fatalf("last page=%+v err=%v", page, err)
	}
	for _, cursor := range []string{"%%", "bnVsbA", "e30"} {
		if _, err := s.HistoryPage(ctx, x.ID, cursor, 128); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatalf("cursor %q: %v", cursor, err)
		}
	}
}
