package statebackup

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gofrs/flock"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/runtime"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestOfflineBackupRestoreAndIntegrity(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	data := filepath.Join(base, "data")
	native, err := sqlite.Open(filepath.Join(data, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := runtime.NewStore(ctx, native.DB())
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.CreateSession(ctx, t.TempDir(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Append(ctx, harness.RunEvent{SessionID: session.ID, RunID: "run-fixture", Kind: "text_delta", Text: "durable fixture"}); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(data, "assets", "session"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(data, "assets", "session", "image.bin"), []byte("immutable fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	lock := flock.New(filepath.Join(data, "runtime.lock"))
	if err = lock.Lock(); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(base, "backup")
	if _, err = Backup(ctx, data, backup); err == nil {
		t.Fatal("live harness was copied")
	}
	if err = lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err = native.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = Backup(ctx, data, filepath.Join(data, "nested")); err == nil {
		t.Fatal("nested backup allowed")
	}
	m, err := Backup(ctx, data, backup)
	if err != nil || len(m.Files) != 2 {
		t.Fatalf("backup=%+v error=%v", m, err)
	}
	if _, err = Verify(ctx, backup); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(base, "restored")
	if _, err = Restore(ctx, backup, restored); err != nil {
		t.Fatal(err)
	}
	if _, err = Restore(ctx, backup, restored); err == nil {
		t.Fatal("existing directory overwritten")
	}
	content, err := os.ReadFile(filepath.Join(restored, "assets", "session", "image.bin"))
	if err != nil || string(content) != "immutable fixture" {
		t.Fatalf("asset=%q error=%v", content, err)
	}
	opened, err := sqlite.Open(filepath.Join(restored, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	restoredStore, err := runtime.NewStore(ctx, opened.DB())
	if err != nil {
		t.Fatal(err)
	}
	actual, err := restoredStore.Session(ctx, session.ID)
	if err != nil || actual.Model != "fixture" {
		t.Fatalf("session=%+v error=%v", actual, err)
	}
	events, err := restoredStore.History(ctx, session.ID)
	if err != nil || len(events) != 1 || events[0].Text != "durable fixture" {
		t.Fatalf("events=%+v error=%v", events, err)
	}
	if err = os.WriteFile(filepath.Join(backup, "assets", "session", "image.bin"), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Verify(ctx, backup); err == nil {
		t.Fatal("tamper was accepted")
	}
	rejected := filepath.Join(base, "rejected")
	if _, err = Restore(ctx, backup, rejected); err == nil {
		t.Fatal("tampered restore was accepted")
	}
	if _, err = os.Stat(rejected); !os.IsNotExist(err) {
		t.Fatal("invalid backup created restore directory")
	}
}
