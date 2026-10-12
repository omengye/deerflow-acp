package learning

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"path/filepath"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func sessionWorkspaceTx(ctx context.Context, tx *sql.Tx, session string) (string, error) {
	var cwd string
	if err := tx.QueryRowContext(ctx, `SELECT cwd FROM harness_sessions WHERE id=?`, session).Scan(&cwd); err != nil {
		return "", err
	}
	if !filepath.IsAbs(cwd) {
		return "", harness.ErrPermissionDenied
	}
	return workspaceKey(cwd), nil
}

func (s *Store) sharedSessionTx(ctx context.Context, tx *sql.Tx, session, owner string, epoch int64) error {
	if !s.enabled || !s.options.CrossSession || s.options.Consolidation == nil {
		return harness.ErrPermissionDenied
	}
	var allowed bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_learning_controls c
 JOIN harness_learning_sharing sh ON sh.session_id=c.session_id JOIN harness_learning_host h ON h.id=1
 WHERE c.session_id=? AND c.contribute=1 AND c.epoch=? AND sh.workspace_shared=1 AND h.enabled=1 AND h.policy_version=?)`, session, epoch, PolicyVersion).Scan(&allowed); err != nil {
		return err
	}
	workspace, err := sessionWorkspaceTx(ctx, tx, session)
	if err == nil && (!allowed || workspace != owner) {
		err = harness.ErrPermissionDenied
	}
	return err
}

func (s *Store) seedSharedTx(ctx context.Context, tx *sql.Tx, dep experienceDependency, owner string) (bool, error) {
	if !s.options.CrossSession {
		return false, nil
	}
	var shared bool
	err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT workspace_shared FROM harness_learning_sharing WHERE session_id=?),0)`, dep.Session).Scan(&shared)
	if err == nil && shared {
		err = s.sharedSessionTx(ctx, tx, dep.Session, owner, dep.Epoch)
	}
	return shared, err
}

func (s *Store) sharedPatternTx(ctx context.Context, tx *sql.Tx, snap patternSnapshot, owner string) error {
	if !snap.Shared {
		return nil
	}
	for _, dep := range snap.Members {
		if err := s.sharedSessionTx(ctx, tx, dep.Session, owner, dep.Epoch); err != nil {
			return err
		}
	}
	return nil
}

// Initial identical accepted inputs conservatively share a vote. Explicit task
// boundaries get fresh keys; resume and host-linked copies keep their key.
func captureLineageTx(ctx context.Context, tx *sql.Tx, episode, owner, inputSHA string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO df_learning_episode_lineage VALUES(?,?,'initial_input')`, episode, encodedDigest([]string{"initial-task-v1", owner, inputSHA}))
	return err
}

func (s *Store) LinkEpisodeLineage(ctx context.Context, session string, req harness.SkillLearningLineageRequest) (string, error) {
	if session == req.SourceSessionID || req.ExpectedEpoch < 1 || req.ExpectedSourceEpoch < 1 || req.SourceEpisodeID == "" {
		return "", harness.ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	owner, err := sessionWorkspaceTx(ctx, tx, session)
	if err != nil {
		return "", err
	}
	if err = s.sharedSessionTx(ctx, tx, session, owner, req.ExpectedEpoch); err != nil {
		return "", err
	}
	if err = s.sharedSessionTx(ctx, tx, req.SourceSessionID, owner, req.ExpectedSourceEpoch); err != nil {
		return "", err
	}
	var started bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_runs WHERE session_id=?)
 OR EXISTS(SELECT 1 FROM df_learning_episodes WHERE session_id=?)`, session, session).Scan(&started); err != nil {
		return "", err
	}
	if started {
		return "", harness.ErrSkillConflict
	}
	var key string
	if err = tx.QueryRowContext(ctx, `SELECT l.independence_key FROM df_learning_episodes e
 JOIN df_learning_episode_lineage l ON l.episode_id=e.id WHERE e.id=? AND e.session_id=? AND e.workspace_key=?`, req.SourceEpisodeID, req.SourceSessionID, owner).Scan(&key); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = harness.ErrNotFound
		}
		return "", err
	}
	id, err := insertEpisodeTx(ctx, tx, session, owner, "linked_task", key)
	if err != nil {
		return "", err
	}
	return id, tx.Commit()
}

func insertEpisodeTx(ctx context.Context, tx *sql.Tx, session, owner, origin, lineage string) (string, error) {
	id := "lep_" + rand.Text()
	key := encodedDigest([]string{"episode-v1", session, id})
	if _, err := tx.ExecContext(ctx, `INSERT INTO df_learning_episodes VALUES(?,?,?,?,?)`, id, session, owner, key, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return "", err
	}
	if lineage == "" {
		lineage = key
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO df_learning_episode_lineage VALUES(?,?,?)`, id, lineage, origin)
	return id, err
}
