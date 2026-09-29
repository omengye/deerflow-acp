// Package sqlite provides durable Eino stores in the harness database.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"
	_ "modernc.org/sqlite"
)

// Config controls persistence serialization and task lease limits. A deployment
// must keep its serializer compatible with previously persisted session events.
type Config struct {
	EventSerializer      schema.Serializer
	ActiveAttemptTimeout time.Duration
	MaxTaskValueBytes    int64
}

// Store owns one SQLite connection pool. Eino tables are prefixed with eino_;
// applications may use DB for independent business tables in the same database.
type Store struct {
	db         *sql.DB
	serializer schema.Serializer
	tasks      *TaskStore
}

func Open(path string) (*Store, error) { return OpenWithConfig(path, Config{}) }

func OpenWithConfig(path string, cfg Config) (*Store, error) {
	if path == "" {
		return nil, errors.New("sqlite: database path is empty")
	}
	var dsn string
	if path == ":memory:" {
		dsn = ":memory:"
	} else {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("sqlite: resolve path: %w", err)
		}
		if err = os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
			return nil, err
		}
		p := filepath.ToSlash(absolute)
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		dsn = (&url.URL{Scheme: "file", Path: p}).String()
	}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(FULL)")
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", dsn+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	// Single local writer, short transactions; immediate acquisition also avoids
	// deferred read-to-write upgrade races between distinct Store instances.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db, serializer: cfg.EventSerializer}
	if s.serializer == nil {
		s.serializer = &canonicalEventSerializer{}
	}
	if _, err = db.Exec("PRAGMA journal_mode=WAL"); err == nil {
		err = s.migrate(context.Background())
	}
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite: initialize: %w", err)
	}
	s.tasks, err = newTaskStore(s, cfg)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) DB() *sql.DB  { return s.db }
func (s *Store) Close() error { return s.db.Close() }

// Tasks exposes the backgroundtask provider separately because its Get method
// has a different signature from CheckPointStore.Get.
func (s *Store) Tasks() *TaskStore { return s.tasks }

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS eino_schema_migrations (version INTEGER PRIMARY KEY)"); err != nil {
		return err
	}
	var version int
	if err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(version),0) FROM eino_schema_migrations").Scan(&version); err != nil {
		return err
	}
	if version > 1 {
		return fmt.Errorf("sqlite: database schema %d is newer than supported schema 1", version)
	}
	_, err = tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS eino_schema_migrations (version INTEGER PRIMARY KEY);
CREATE TABLE IF NOT EXISTS eino_store_identity (id INTEGER PRIMARY KEY CHECK(id=1), value TEXT NOT NULL);
INSERT OR IGNORE INTO eino_store_identity(id,value) VALUES(1,lower(hex(randomblob(16))));
CREATE TABLE IF NOT EXISTS eino_checkpoints (
  id TEXT PRIMARY KEY, payload BLOB NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS eino_session_events (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL, event_id TEXT NOT NULL, kind TEXT NOT NULL,
  payload BLOB NOT NULL, UNIQUE(session_id,event_id)
);
CREATE INDEX IF NOT EXISTS eino_session_events_session ON eino_session_events(session_id,seq);
CREATE TABLE IF NOT EXISTS eino_background_tasks (
  id TEXT PRIMARY KEY, executor_key TEXT NOT NULL, status TEXT NOT NULL,
  version INTEGER NOT NULL, lease_expires_at INTEGER NOT NULL DEFAULT 0,
  payload BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS eino_background_tasks_status ON eino_background_tasks(status,executor_key,id);
CREATE TABLE IF NOT EXISTS eino_task_events (
  seq INTEGER PRIMARY KEY AUTOINCREMENT, task_id TEXT NOT NULL,
  event_id TEXT NOT NULL, data BLOB NOT NULL, created_at TEXT NOT NULL,
  UNIQUE(task_id,event_id), FOREIGN KEY(task_id) REFERENCES eino_background_tasks(id)
);
CREATE INDEX IF NOT EXISTS eino_task_events_task ON eino_task_events(task_id,seq);
CREATE TABLE IF NOT EXISTS eino_task_notifications (
  seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT UNIQUE NOT NULL,
  task_id TEXT NOT NULL, payload BLOB NOT NULL,
  receipt BLOB, lease_expires_at INTEGER NOT NULL DEFAULT 0,
  FOREIGN KEY(task_id) REFERENCES eino_background_tasks(id)
);
CREATE TABLE IF NOT EXISTS eino_notification_replays (
  task_id TEXT NOT NULL, event_id TEXT NOT NULL, kind TEXT NOT NULL, data BLOB,
  PRIMARY KEY(task_id,event_id), FOREIGN KEY(task_id) REFERENCES eino_background_tasks(id)
);
INSERT OR IGNORE INTO eino_schema_migrations(version) VALUES(1);`)
	if err != nil {
		return err
	}
	return tx.Commit()
}
