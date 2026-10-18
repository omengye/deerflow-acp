package automation

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestScheduleIdempotentCreateAndDeletedTombstone(t *testing.T) {
	s := fixture(t, nil)
	ctx := context.Background()
	spec := Spec{Name: "idempotent", Prompt: "inspect", Workspace: t.TempDir(), Kind: "once", At: time.Now().Add(time.Hour)}
	one, err := s.CreateIdempotent(ctx, spec, "message-operation")
	if err != nil {
		t.Fatal(err)
	}
	two, err := s.CreateIdempotent(ctx, spec, "message-operation")
	if err != nil || one.Spec.ID != two.Spec.ID {
		t.Fatal(two, err)
	}
	spec.Prompt = "changed"
	if _, err = s.CreateIdempotent(ctx, spec, "message-operation"); !errors.Is(err, ErrConflict) {
		t.Fatal("conflicting replay accepted", err)
	}
	spec.Prompt = "inspect"
	if err = s.Delete(ctx, one.Spec.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateIdempotent(ctx, spec, "message-operation"); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted task recreated", err)
	}
}

func TestScheduleIdempotencySurvivesNewSourceSession(t *testing.T) {
	s := fixture(t, nil)
	ctx := context.Background()
	spec := Spec{Name: "same intent", Prompt: "inspect", Workspace: t.TempDir(), Kind: "interval", EverySeconds: 3600, Source: &Source{Kind: "buzz", Account: "account", Sender: "owner", SessionID: "old-session"}}
	one, err := s.CreateIdempotent(ctx, spec, "message-key")
	if err != nil {
		t.Fatal(err)
	}
	spec.Source.SessionID = "new-session"
	two, err := s.CreateIdempotent(ctx, spec, "message-key")
	if err != nil || one.Spec.ID != two.Spec.ID {
		t.Fatal(two, err)
	}
}
