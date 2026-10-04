package agents

import (
	"context"
	"database/sql"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"time"
	"unicode/utf8"
)

var ErrStopped = harness.ErrTaskTreeStopped

func migrateStates(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS harness_agent_states (
 invocation_id TEXT PRIMARY KEY REFERENCES harness_agent_invocations(id) ON DELETE CASCADE,
 root_id TEXT NOT NULL,state TEXT NOT NULL,version INTEGER NOT NULL,attempt_id TEXT NOT NULL,
 result_text TEXT NOT NULL DEFAULT '',result_available INTEGER NOT NULL DEFAULT 0,result_truncated INTEGER NOT NULL DEFAULT 0,
 created_at TEXT NOT NULL,updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS harness_task_tree_stops (
 root_id TEXT NOT NULL,node_id TEXT NOT NULL,created_at TEXT NOT NULL,PRIMARY KEY(root_id,node_id)
);
CREATE TABLE IF NOT EXISTS harness_execution_intent_roles (
 intent_id TEXT PRIMARY KEY REFERENCES harness_execution_intents(id) ON DELETE CASCADE,
 root_id TEXT NOT NULL,invocation_id TEXT NOT NULL,operation_id TEXT NOT NULL DEFAULT ''
);`)
	return err
}

// RecordStopped ends an already joined, suspended invocation after the host
// has retired its pending permissions. It cannot certify a live attempt.
func (c *Controller) RecordStopped(ctx context.Context, root, id string) (err error) {
	if c == nil || c.db == nil {
		return nil
	}
	defer func() { err = wrapTimePersistence("record stopped role", err) }()
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = CheckStopped(ctx, tx, root, id); err != ErrStopped {
		if err == nil {
			return ErrInvocationConflict
		}
		return err
	}
	var active bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_agent_attempts WHERE invocation_id=? AND state='active')`, id).Scan(&active); err != nil {
		return err
	}
	if active {
		return ErrAttemptBusy
	}
	_, err = tx.ExecContext(ctx, `UPDATE harness_agent_states SET state='cancelled',version=version+1,result_text='',result_available=0,result_truncated=0,updated_at=? WHERE invocation_id=? AND root_id=? AND state IN ('waiting_input','cancelling')`, time.Now().UTC().Format(time.RFC3339Nano), id, root)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// CheckStopped follows immutable logical ancestry. Checking a proposed child's
// parent before inserting its registration fences late descendants as well.
// Empty id means the root itself. Call inside the transaction admitting work.
func CheckStopped(ctx context.Context, q ancestryQuery, root, id string) error {
	var stopped bool
	err := q.QueryRowContext(ctx, `WITH RECURSIVE ancestors(id) AS (
 SELECT ? UNION SELECT i.parent_id FROM harness_agent_invocations i JOIN ancestors a ON i.id=a.id WHERE i.root_id=? AND i.parent_id<>''
) SELECT EXISTS(SELECT 1 FROM harness_task_tree_stops WHERE root_id=? AND (node_id='' OR node_id IN (SELECT id FROM ancestors)))`, id, root, root).Scan(&stopped)
	if err != nil {
		return err
	}
	if stopped {
		return ErrStopped
	}
	return nil
}

func CheckStoppedIfConfigured(ctx context.Context, q ancestryQuery, root, id string) error {
	var exists bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='harness_task_tree_stops')`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	return CheckStopped(ctx, q, root, id)
}

func (a *Attempt) CheckStop(ctx context.Context) (err error) {
	defer func() { err = wrapTimePersistence("check role stop", err) }()
	if a == nil || a.controller.db == nil {
		return nil
	}
	var root string
	if err := a.controller.db.QueryRowContext(ctx, `SELECT root_id FROM harness_agent_invocations WHERE id=?`, a.invocation).Scan(&root); err != nil {
		return err
	}
	return CheckStopped(ctx, a.controller.db, root, a.invocation)
}

// RecordState never transfers attempt authority. A stale attempt cannot
// overwrite a successor's running or terminal state, even after its cleanup.
func (a *Attempt) RecordState(ctx context.Context, state string, result *string) (err error) {
	if a == nil || a.controller.db == nil {
		return nil
	}
	switch state {
	case "running", "waiting_input", "completed", "failed", "cancelled":
	default:
		return ErrInvocationConflict
	}
	defer func() { err = wrapTimePersistence("record role state", err) }()
	tx, err := a.controller.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var root, attemptState string
	var newer bool
	err = tx.QueryRowContext(ctx, `SELECT i.root_id,a.state,EXISTS(SELECT 1 FROM harness_agent_attempts n WHERE n.invocation_id=a.invocation_id AND n.rowid>a.rowid) FROM harness_agent_attempts a JOIN harness_agent_invocations i ON i.id=a.invocation_id WHERE a.id=? AND a.invocation_id=?`, a.id, a.invocation).Scan(&root, &attemptState, &newer)
	if err != nil {
		return err
	}
	if newer || state == "running" && attemptState != "active" || state != "running" && attemptState != "ended" {
		return ErrAttemptConflict
	}
	text, truncated := "", false
	if result != nil {
		text = *result
		if len(text) > 8192 {
			text = text[:8192]
			for !utf8.ValidString(text) {
				text = text[:len(text)-1]
			}
			truncated = true
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = tx.ExecContext(ctx, `INSERT INTO harness_agent_states(invocation_id,root_id,state,version,attempt_id,result_text,result_available,result_truncated,created_at,updated_at) VALUES(?,?,?,1,?,?,?,?,?,?) ON CONFLICT(invocation_id) DO UPDATE SET state=excluded.state,version=harness_agent_states.version+1,attempt_id=excluded.attempt_id,result_text=excluded.result_text,result_available=excluded.result_available,result_truncated=excluded.result_truncated,updated_at=excluded.updated_at`, a.invocation, root, state, a.id, text, result != nil, truncated, now, now)
	if err != nil {
		return err
	}
	return tx.Commit()
}
