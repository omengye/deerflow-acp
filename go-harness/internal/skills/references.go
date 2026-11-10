package skills

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type snapshotLease struct {
	mu      sync.Mutex
	db      *sql.DB
	owner   string
	handles atomic.Int64
}

func (r *Registry) migrateReferences(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS df_skill_version_references(
 owner TEXT NOT NULL,kind TEXT NOT NULL,source_key TEXT NOT NULL,name TEXT NOT NULL,version TEXT NOT NULL,
 PRIMARY KEY(owner,source_key,name,version),FOREIGN KEY(source_key,name,version) REFERENCES df_skill_versions(source_key,name,version));
CREATE INDEX IF NOT EXISTS df_skill_referenced_version ON df_skill_version_references(source_key,name,version);
CREATE TABLE IF NOT EXISTS df_skill_content_gc(source_key TEXT NOT NULL,name TEXT NOT NULL,version TEXT NOT NULL,state TEXT NOT NULL,marked_at TEXT NOT NULL,
 PRIMARY KEY(source_key,name,version));
CREATE TABLE IF NOT EXISTS df_skill_object_gc(hash TEXT PRIMARY KEY,state TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS df_skill_scoped_versions(source_key TEXT NOT NULL,name TEXT NOT NULL,version TEXT NOT NULL,workspace TEXT NOT NULL,PRIMARY KEY(source_key,name,version));
CREATE TABLE IF NOT EXISTS df_skill_routes(source_key TEXT NOT NULL,name TEXT NOT NULL,workspace TEXT NOT NULL,state TEXT NOT NULL,stable_version TEXT NOT NULL,public_version TEXT NOT NULL,candidate_version TEXT NOT NULL,PRIMARY KEY(source_key,name));
CREATE TABLE IF NOT EXISTS df_skill_route_tasks(root_id TEXT NOT NULL,source_key TEXT NOT NULL,name TEXT NOT NULL,version TEXT NOT NULL,PRIMARY KEY(root_id,source_key,name));`)
	if err != nil {
		return err
	}
	var runs bool
	if err = r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='harness_runs')`).Scan(&runs); err != nil {
		return err
	}
	if runs {
		_, err = r.db.ExecContext(ctx, `CREATE TRIGGER IF NOT EXISTS df_skill_checkpoint_ref_delete AFTER DELETE ON harness_runs BEGIN DELETE FROM df_skill_version_references WHERE owner='checkpoint/'||OLD.id AND kind='checkpoint'; END;`)
	}
	return err
}

func pinVersionsTx(ctx context.Context, tx *sql.Tx, owner, kind string, refs []harness.SkillRef) error {
	for _, ref := range refs {
		var state string
		err := tx.QueryRowContext(ctx, `SELECT state FROM df_skill_content_gc WHERE source_key=? AND name=? AND version=?`, ref.SourceIdentity, ref.Name, ref.Version).Scan(&state)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if state == "deleting" || state == "expired" {
			return fmtContentExpired()
		}
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO df_skill_version_references VALUES(?,?,?,?,?)`, owner, kind, ref.SourceIdentity, ref.Name, ref.Version); err != nil {
			return err
		}
		if state == "marked" {
			if _, err = tx.ExecContext(ctx, `DELETE FROM df_skill_content_gc WHERE source_key=? AND name=? AND version=?`, ref.SourceIdentity, ref.Name, ref.Version); err != nil {
				return err
			}
		}
	}
	return nil
}

var ErrContentExpired = errors.New("skill version content has expired")

func fmtContentExpired() error { return ErrContentExpired }

func (r *Registry) ReleaseVersions(ctx context.Context, owner, kind string) error {
	if owner == "" || kind == "" {
		return harness.ErrInvalidInput
	}
	_, err := r.db.ExecContext(ctx, `DELETE FROM df_skill_version_references WHERE owner=? AND kind=?`, owner, kind)
	return err
}

// PinVersions persists checkpoint or publication dependencies before making
// the referencing record visible. Content deletion and pin acquisition use the
// same SQLite write transaction, so a grace period is never the only guard.
func (r *Registry) PinVersions(ctx context.Context, owner, kind string, refs []harness.SkillRef) error {
	if owner == "" || len(owner) > 512 || kind == "" {
		return harness.ErrInvalidInput
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = pinVersionsTx(ctx, tx, owner, kind, refs); err != nil {
		return err
	}
	return tx.Commit()
}

func pinSnapshotTx(ctx context.Context, tx *sql.Tx, s *Snapshot) error {
	lease := &snapshotLease{db: s.db, owner: "snapshot/" + rand.Text()}
	lease.handles.Store(1)
	if err := pinVersionsTx(ctx, tx, lease.owner, "snapshot", s.Refs()); err != nil {
		return err
	}
	s.lease, s.release = lease, &sync.Once{}
	return nil
}

func (s *Snapshot) inheritLease(parent *Snapshot) bool {
	if parent.lease == nil {
		return parent.closed == nil || !parent.closed.Load()
	}
	parent.lease.mu.Lock()
	defer parent.lease.mu.Unlock()
	if parent.closed.Load() {
		return false
	}
	parent.lease.handles.Add(1)
	s.lease = parent.lease
	s.release = &sync.Once{}
	s.closed = &atomic.Bool{}
	return true
}

// Close releases this handle. Subset and observed copies retain independent
// handles; closing a parent cannot invalidate a child or a saved checkpoint.
func (s *Snapshot) Close() error {
	if s == nil || s.lease == nil {
		return nil
	}
	var err error
	s.release.Do(func() {
		s.lease.mu.Lock()
		defer s.lease.mu.Unlock()
		s.closed.Store(true)
		if s.lease.handles.Add(-1) == 0 {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err = s.db.ExecContext(ctx, `DELETE FROM df_skill_version_references WHERE owner=? AND kind='snapshot'`, s.lease.owner)
		}
	})
	return err
}
