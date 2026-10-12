package learning

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

const PolicyVersion = "procedural-experience-v1"

type Store struct {
	db           *sql.DB
	enabled      bool
	options      harness.SkillLearningOptions
	mu           sync.Mutex
	cancel       context.CancelFunc
	done         chan struct{}
	closed       bool
	extractor    harness.SkillExperienceExtractor
	consolidator harness.SkillExperienceConsolidator
	matcher      harness.SkillExperienceMatcher
	matchAccess  *skills.MatchAccess
	rootEvidence RootEvidenceReader
	maintenance  *skillmaintenance.Store
	shared       bool
	now          func() time.Time
}

func Migrate(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS harness_learning_host(id INTEGER PRIMARY KEY CHECK(id=1),enabled INTEGER NOT NULL CHECK(enabled IN (0,1)),policy_version TEXT NOT NULL);
INSERT OR IGNORE INTO harness_learning_host VALUES(1,0,'procedural-experience-v1');
CREATE TABLE IF NOT EXISTS harness_learning_controls(
 session_id TEXT PRIMARY KEY REFERENCES harness_sessions(id) ON DELETE CASCADE,
 contribute INTEGER NOT NULL CHECK(contribute IN (0,1)),epoch INTEGER NOT NULL CHECK(epoch>=1));
CREATE TABLE IF NOT EXISTS df_learning_episodes(
 id TEXT PRIMARY KEY,session_id TEXT NOT NULL REFERENCES harness_sessions(id) ON DELETE CASCADE,
 workspace_key TEXT NOT NULL,independence_key TEXT NOT NULL UNIQUE,created_at TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS df_learning_episode_session ON df_learning_episodes(session_id,created_at,id);
CREATE TABLE IF NOT EXISTS harness_learning_inputs(
 run_id TEXT PRIMARY KEY REFERENCES harness_runs(id) ON DELETE CASCADE,
 session_id TEXT NOT NULL REFERENCES harness_sessions(id) ON DELETE CASCADE,
 input_id TEXT NOT NULL,root_id TEXT NOT NULL,episode_id TEXT NOT NULL REFERENCES df_learning_episodes(id) ON DELETE CASCADE,
 accepted_sequence INTEGER NOT NULL,input_sha TEXT NOT NULL,epoch INTEGER NOT NULL,policy_version TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS harness_learning_input_session ON harness_learning_inputs(session_id,episode_id);
CREATE TABLE IF NOT EXISTS harness_learning_outbox(
 sequence INTEGER PRIMARY KEY AUTOINCREMENT,run_id TEXT NOT NULL UNIQUE REFERENCES harness_learning_inputs(run_id) ON DELETE CASCADE,
 session_id TEXT NOT NULL REFERENCES harness_sessions(id) ON DELETE CASCADE,
 episode_id TEXT NOT NULL REFERENCES df_learning_episodes(id) ON DELETE CASCADE,
 frontier INTEGER NOT NULL,epoch INTEGER NOT NULL,status TEXT NOT NULL,effects_uncertain INTEGER NOT NULL,
 ended_at TEXT NOT NULL,state TEXT NOT NULL DEFAULT 'pending',job_id TEXT NOT NULL DEFAULT '',reason TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS harness_learning_outbox_pending ON harness_learning_outbox(state,sequence);
CREATE INDEX IF NOT EXISTS harness_learning_outbox_episode ON harness_learning_outbox(episode_id,sequence);
CREATE TABLE IF NOT EXISTS df_learning_scan(id INTEGER PRIMARY KEY CHECK(id=1),cursor INTEGER NOT NULL);
INSERT OR IGNORE INTO df_learning_scan VALUES(1,0);
CREATE TRIGGER IF NOT EXISTS harness_learning_terminal_outbox
AFTER UPDATE OF status ON harness_runs
WHEN NEW.status IN ('completed','failed','cancelled') AND OLD.status NOT IN ('completed','failed','cancelled')
BEGIN
 INSERT INTO harness_learning_outbox(run_id,session_id,episode_id,frontier,epoch,status,effects_uncertain,ended_at)
 SELECT i.run_id,i.session_id,i.episode_id,
 (SELECT COALESCE(MAX(sequence),0) FROM harness_events WHERE run_id=NEW.id),i.epoch,NEW.status,
 EXISTS(SELECT 1 FROM harness_tool_receipts WHERE run_id=NEW.id AND state IN ('pending','started','uncertain')),NEW.updated_at
 FROM harness_learning_inputs i JOIN harness_learning_controls c ON c.session_id=i.session_id
 JOIN harness_learning_host h ON h.id=1 WHERE i.run_id=NEW.id AND h.enabled=1 AND c.contribute=1
 AND c.epoch=i.epoch AND i.policy_version=h.policy_version;
END;
CREATE TABLE IF NOT EXISTS df_learning_extractions(
 id TEXT PRIMARY KEY,session_id TEXT NOT NULL REFERENCES harness_sessions(id) ON DELETE CASCADE,
 episode_id TEXT NOT NULL REFERENCES df_learning_episodes(id) ON DELETE CASCADE,
 job_id TEXT NOT NULL UNIQUE,epoch INTEGER NOT NULL,policy_version TEXT NOT NULL,model_identity TEXT NOT NULL,
 payload BLOB NOT NULL,payload_sha TEXT NOT NULL,result_sha TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS df_learning_extraction_episode ON df_learning_extractions(episode_id,job_id);
CREATE TABLE IF NOT EXISTS df_learning_experiences(
 id TEXT PRIMARY KEY,extraction_id TEXT NOT NULL REFERENCES df_learning_extractions(id) ON DELETE CASCADE,
 session_id TEXT NOT NULL REFERENCES harness_sessions(id) ON DELETE CASCADE,
 episode_id TEXT NOT NULL REFERENCES df_learning_episodes(id) ON DELETE CASCADE,
 revision INTEGER NOT NULL,payload BLOB NOT NULL,payload_sha TEXT NOT NULL,created_at TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS df_learning_experiences_session ON df_learning_experiences(session_id,id);
`)
	if err != nil {
		return err
	}
	if err = migrateConsolidation(ctx, tx); err != nil {
		return err
	}
	if err = migrateMatching(ctx, tx); err != nil {
		return err
	}
	if err = migrateWorkspaceLearning(ctx, tx); err != nil {
		return err
	}
	if err = migrateReview(ctx, tx); err != nil {
		return err
	}
	return migratePromotion(ctx, tx)
}

func New(ctx context.Context, db *sql.DB, options *harness.SkillLearningOptions) (*Store, error) {
	if db == nil {
		return nil, harness.ErrInvalidInput
	}
	s := &Store{db: db, enabled: options != nil, now: time.Now}
	if options != nil {
		s.options = *options
		if options.Consolidation != nil {
			copy := *options.Consolidation
			s.options.Consolidation = &copy
		}
		if options.Matching != nil {
			copy := *options.Matching
			s.options.Matching = &copy
		}
		if s.options.Idle == 0 {
			s.options.Idle = 5 * time.Minute
		}
		if s.options.Idle < time.Second || s.options.Idle > 24*time.Hour {
			return nil, invalid("invalid idle window")
		}
		if err := validateOptions(s.options); err != nil {
			return nil, err
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = Migrate(ctx, tx); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_learning_host SET enabled=?,policy_version=? WHERE id=1`, s.enabled, PolicyVersion); err != nil {
		return nil, err
	}
	if !s.enabled {
		// Switching the host off does not silently discard paid receipts or
		// installed skills, but old extraction claims must stop progressing.
		var jobs bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='df_skill_maintenance_jobs')`).Scan(&jobs); err != nil {
			return nil, err
		}
		if jobs {
			if err = skillmaintenance.Migrate(ctx, tx); err != nil {
				return nil, err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE df_skill_maintenance_jobs SET state='cancelled',state_version=state_version+1,token='',lease_ns=0,retry_ns=0,error_code='cancelled'
 WHERE job_kind IN ('experience_extract','experience_consolidate','skill_match') AND state IN ('queued','retry','running')`); err != nil {
				return nil, err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE harness_learning_outbox SET state='cancelled',reason='host_disabled'
 WHERE state='queued' AND job_id IN (SELECT id FROM df_skill_maintenance_jobs WHERE job_kind='experience_extract' AND state='cancelled')`); err != nil {
				return nil, err
			}
		}
	}
	if !s.enabled || s.options.Consolidation == nil {
		if err = disableConsolidationTx(ctx, tx); err != nil {
			return nil, err
		}
	}
	if !s.enabled || s.options.Matching == nil {
		if err = disableMatchingTx(ctx, tx); err != nil {
			return nil, err
		}
	}
	if !s.enabled || !s.options.CrossSession {
		if err = disableWorkspaceLearningTx(ctx, tx); err != nil {
			return nil, err
		}
	}
	return s, tx.Commit()
}

func invalid(message string) error {
	return fmt.Errorf("%w: skill learning %s", harness.ErrInvalidInput, message)
}

func digest(data []byte) string      { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func encodedDigest(value any) string { data, _ := json.Marshal(value); return digest(data) }

func workspaceKey(workspace string) string {
	workspace = filepath.Clean(workspace)
	if runtime.GOOS == "windows" {
		workspace = strings.ToLower(workspace)
	}
	return encodedDigest([]string{"workspace", workspace})
}

// CaptureInputTx is called only at the host's real-user BeginRun boundary, not
// from the similarly named child/notification transcript events.
func (s *Store) CaptureInputTx(ctx context.Context, tx *sql.Tx, req harness.RunRequest, sequence int64, input []byte) error {
	if !s.enabled {
		return nil
	}
	if sequence < 1 || req.RunID == "" || req.InputID == "" || !filepath.IsAbs(req.Session.CWD) {
		return invalid("invalid accepted input")
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO harness_learning_controls VALUES(?,1,1)`, req.Session.ID); err != nil {
		return err
	}
	var contribute bool
	var epoch int64
	if err := tx.QueryRowContext(ctx, `SELECT contribute,epoch FROM harness_learning_controls WHERE session_id=?`, req.Session.ID).Scan(&contribute, &epoch); err != nil {
		return err
	}
	if !contribute {
		return nil
	}
	// Preserve privacy/backpressure across restarts without blocking the main
	// task once its small, metadata-only pending inventory is full.
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM harness_learning_inputs i JOIN harness_runs r ON r.id=i.run_id
 WHERE i.session_id=? AND NOT EXISTS(SELECT 1 FROM harness_learning_outbox o WHERE o.run_id=i.run_id AND o.state NOT IN ('pending','queued'))`, req.Session.ID).Scan(&pending); err != nil {
		return err
	}
	if pending >= 128 {
		return nil
	}
	var episode, workspace string
	err := tx.QueryRowContext(ctx, `SELECT id,workspace_key FROM df_learning_episodes WHERE session_id=? ORDER BY rowid DESC LIMIT 1`, req.Session.ID).Scan(&episode, &workspace)
	if errors.Is(err, sql.ErrNoRows) {
		episode = "lep_" + req.InputID
		workspace = workspaceKey(req.Session.CWD)
		_, err = tx.ExecContext(ctx, `INSERT INTO df_learning_episodes VALUES(?,?,?,?,?)`, episode, req.Session.ID, workspace, encodedDigest([]string{"episode-v1", req.Session.ID, req.InputID}), time.Now().UTC().Format(time.RFC3339Nano))
		if err == nil {
			err = captureLineageTx(ctx, tx, episode, workspace, digest(input))
		}
	}
	if err != nil {
		return err
	}
	var lineageExists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_learning_episode_lineage WHERE episode_id=?)`, episode).Scan(&lineageExists); err != nil {
		return err
	}
	if !lineageExists {
		return harness.ErrPermissionDenied
	}
	if workspace != workspaceKey(req.Session.CWD) {
		return harness.ErrPermissionDenied
	}
	root := req.RootBudgetID
	if root == "" {
		root = req.RunID
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO harness_learning_inputs VALUES(?,?,?,?,?,?,?,?,?)`, req.RunID, req.Session.ID, req.InputID, root, episode, sequence, digest(input), epoch, PolicyVersion)
	return err
}

func (s *Store) Control(ctx context.Context, sessionID string) (harness.SkillLearningControl, error) {
	var result harness.SkillLearningControl
	result.SessionID = sessionID
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_sessions WHERE id=?)`, sessionID).Scan(&exists); err != nil {
		return result, err
	}
	if !exists {
		return result, harness.ErrNotFound
	}
	err := s.db.QueryRowContext(ctx, `SELECT c.contribute,c.epoch,COALESCE(sh.workspace_shared,0)
 FROM harness_learning_controls c LEFT JOIN harness_learning_sharing sh ON sh.session_id=c.session_id WHERE c.session_id=?`, sessionID).Scan(&result.Contribute, &result.Epoch, &result.WorkspaceShared)
	if errors.Is(err, sql.ErrNoRows) {
		result.Contribute, result.Epoch = s.enabled, 0
		err = nil
	}
	return result, err
}
