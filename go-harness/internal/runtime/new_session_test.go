package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func newSessionTestService(t *testing.T) *Service {
	t.Helper()
	native, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = native.Close() })
	store, err := NewStore(context.Background(), native.DB())
	if err != nil {
		t.Fatal(err)
	}
	return NewService(store, nil, "test")
}

func TestNewSessionSetupFailureDiscardsEmptySession(t *testing.T) {
	for _, failure := range []string{"config", "attachment"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			s := newSessionTestService(t)
			if failure == "config" {
				_, err := s.Store.db.ExecContext(ctx, `CREATE TRIGGER fail_new_config BEFORE INSERT ON harness_session_configs BEGIN SELECT RAISE(ABORT, 'fixture config failure'); END`)
				if err != nil {
					t.Fatal(err)
				}
			} else if err := s.Disconnect(ctx, "owner"); err != nil {
				t.Fatal(err)
			}
			x, err := s.NewSession(ctx, "owner", t.TempDir())
			if err == nil || x.ID == "" {
				t.Fatalf("session=%+v error=%v", x, err)
			}
			if failure == "config" && !strings.Contains(err.Error(), "fixture config failure") {
				t.Fatalf("original config error lost: %v", err)
			}
			if failure == "attachment" && !errors.Is(err, harness.ErrNotAttached) {
				t.Fatalf("original attachment error lost: %v", err)
			}
			sessions, err := s.Store.List(ctx, "", "", 100)
			if err != nil || len(sessions) != 0 {
				t.Fatalf("phantom sessions=%+v error=%v", sessions, err)
			}
			var configs int
			if err := s.Store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM harness_session_configs`).Scan(&configs); err != nil {
				t.Fatal(err)
			}
			if configs != 0 {
				t.Fatalf("orphan configs=%d", configs)
			}
		})
	}
}

func TestDiscardNewSessionPreservesExistingWork(t *testing.T) {
	for _, work := range []string{"run", "event"} {
		t.Run(work, func(t *testing.T) {
			ctx := context.Background()
			s := newSessionTestService(t)
			x, err := s.NewSession(ctx, "owner", t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if work == "run" {
				_, err = s.Store.db.ExecContext(ctx, `INSERT INTO harness_runs(id,session_id,input_id,status,created_at,updated_at) VALUES(?,?,?,'running',?,?)`, "run", x.ID, "input", timestamp(), timestamp())
			} else {
				_, err = s.Store.Append(ctx, harness.RunEvent{SessionID: x.ID, RunID: "run", Kind: "text_delta", Text: "saved"})
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := s.discardNewSession(x.ID); !errors.Is(err, harness.ErrBusy) {
				t.Fatalf("discard error=%v", err)
			}
			if _, err := s.Store.Session(ctx, x.ID); err != nil {
				t.Fatalf("session was deleted: %v", err)
			}
			var configs int
			if err := s.Store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM harness_session_configs WHERE session_id=?`, x.ID).Scan(&configs); err != nil {
				t.Fatal(err)
			}
			if configs != 1 {
				t.Fatalf("existing config lost: %d", configs)
			}
		})
	}
}
