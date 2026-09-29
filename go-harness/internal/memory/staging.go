package memory

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
)

type StagedCandidate struct {
	Scope                 Scope
	ExpectedScopeRevision int64
	Fact                  Candidate
}

func (s *Store) AuditExtraction(ctx context.Context, runID, inputID, attemptID, status, responseSHA string, notes []string) error {
	if runID == "" || inputID == "" || attemptID == "" || len(status) > 64 || len(responseSHA) > 64 || len(notes) > 16 {
		return harness.ErrInvalidInput
	}
	encoded, err := json.Marshal(notes)
	if err != nil || len(encoded) > 4096 {
		return harness.ErrInvalidInput
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO memory_extraction_audit(run_id,attempt_id,input_id,status,response_sha,notes,updated_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(run_id,attempt_id) DO UPDATE SET status=excluded.status,response_sha=excluded.response_sha,notes=excluded.notes,updated_at=excluded.updated_at WHERE input_id=excluded.input_id`, runID, attemptID, inputID, status, responseSHA, encoded, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// Stage records model proposals without changing visible facts. The source
// frontier is read from the accepted user event, never native message count.
// A failed or interrupted run leaves only auditable staging rows.
func (s *Store) Stage(ctx context.Context, runID, inputID, attemptID string, proposed []StagedCandidate) error {
	if runID == "" || inputID == "" || attemptID == "" || len(proposed) > 8 {
		return harness.ErrInvalidInput
	}
	for _, item := range proposed {
		if item.Scope.key == "" || item.ExpectedScopeRevision < 0 || !validCandidate(item.Fact) || item.Fact.Source.Kind != "model" || item.Fact.Source.RunID != runID || item.Fact.Source.InputID != inputID {
			return harness.ErrInvalidInput
		}
	}
	return transact(ctx, s.db, func(tx *sql.Tx) error {
		var actualInput, workspace, sessionID, state string
		if err := tx.QueryRowContext(ctx, `SELECT r.input_id,s.cwd,r.session_id FROM harness_runs r JOIN harness_sessions s ON s.id=r.session_id WHERE r.id=?`, runID).Scan(&actualInput, &workspace, &sessionID); err != nil {
			return err
		}
		if actualInput != inputID {
			return harness.ErrInvalidInput
		}
		var child bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_background_specs WHERE child_session_id=?)`, sessionID).Scan(&child); err != nil {
			return err
		}
		if child {
			return harness.ErrInvalidInput
		}
		if err := tx.QueryRowContext(ctx, `SELECT state FROM budget_attempts WHERE id=? AND member_id=?`, attemptID, runID).Scan(&state); err != nil {
			return err
		}
		if state != "active" {
			return harness.ErrExecutionConflict
		}
		var sequence int64
		if err := tx.QueryRowContext(ctx, `SELECT sequence FROM harness_events WHERE run_id=? AND json_extract(event,'$.kind')='user_message' ORDER BY sequence LIMIT 1`, runID).Scan(&sequence); err != nil {
			return err
		}
		for i, item := range proposed {
			if !session.SameWorkspace(item.Scope.workspace, workspace) || item.Scope.kind == SessionScope && item.Scope.subject != sessionID || item.Scope.kind != SessionScope && item.Scope.kind != WorkspaceScope && item.Scope.kind != UserScope {
				return harness.ErrInvalidInput
			}
			if item.Fact.Source.EventSequence != sequence {
				return harness.ErrInvalidInput
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO memory_staged_facts(run_id,input_id,attempt_id,ordinal,scope_key,scope_kind,workspace,subject,agent,scope_revision,content,category,confidence,source_id,source_event_sequence,policy_version) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				runID, inputID, attemptID, i, item.Scope.key, item.Scope.kind, item.Scope.workspace, item.Scope.subject, item.Scope.agent, item.ExpectedScopeRevision, item.Fact.Content, item.Fact.Category, item.Fact.Confidence, item.Fact.Source.ID, sequence, item.Fact.Source.PolicyVersion)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// AcceptedInputSequence returns the immutable domain event frontier needed to
// attribute an extracted fact. It rejects child/notification sources.
func (s *Store) AcceptedInputSequence(ctx context.Context, runID, inputID string) (int64, error) {
	var actual, sessionID string
	if err := s.db.QueryRowContext(ctx, `SELECT input_id,session_id FROM harness_runs WHERE id=?`, runID).Scan(&actual, &sessionID); err != nil {
		return 0, err
	}
	if actual != inputID {
		return 0, harness.ErrInvalidInput
	}
	var child bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_background_specs WHERE child_session_id=?)`, sessionID).Scan(&child); err != nil {
		return 0, err
	}
	if child {
		return 0, harness.ErrInvalidInput
	}
	var sequence int64
	if err := s.db.QueryRowContext(ctx, `SELECT sequence FROM harness_events WHERE run_id=? AND json_extract(event,'$.kind')='user_message' ORDER BY sequence LIMIT 1`, runID).Scan(&sequence); err != nil {
		return 0, err
	}
	return sequence, nil
}

// SettleStagingTx is called inside the run's terminal transaction. Promotion
// shares the execution and budget commit; all other outcomes discard proposals.
func (s *Store) SettleStagingTx(ctx context.Context, tx *sql.Tx, runID, attemptID string, promote bool) (int, error) {
	if runID == "" || attemptID == "" {
		return 0, harness.ErrInvalidInput
	}
	rows, err := tx.QueryContext(ctx, `SELECT ordinal,input_id,scope_key,scope_kind,workspace,subject,agent,scope_revision,content,category,confidence,source_id,source_event_sequence,policy_version FROM memory_staged_facts WHERE run_id=? AND attempt_id=? AND status='staged' ORDER BY ordinal`, runID, attemptID)
	if err != nil {
		return 0, err
	}
	type staged struct {
		ordinal               int
		inputID               string
		scope                 Scope
		expectedScopeRevision int64
		fact                  Candidate
	}
	var items []staged
	for rows.Next() {
		var item staged
		if err = rows.Scan(&item.ordinal, &item.inputID, &item.scope.key, &item.scope.kind, &item.scope.workspace, &item.scope.subject, &item.scope.agent, &item.expectedScopeRevision, &item.fact.Content, &item.fact.Category, &item.fact.Confidence, &item.fact.Source.ID, &item.fact.Source.EventSequence, &item.fact.Source.PolicyVersion); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	if len(items) > 8 {
		return 0, harness.ErrInvalidInput
	}
	promoted := 0
	scopeChanged := false
	for _, item := range items {
		status := "discarded"
		if promote {
			item.fact.Source.Kind, item.fact.Source.RunID, item.fact.Source.InputID = "model", runID, item.inputID
			if !validCandidate(item.fact) {
				return 0, harness.ErrInvalidInput
			}
			version, err := checkScope(ctx, tx, item.scope, true)
			if err != nil {
				return 0, err
			}
			if version != item.expectedScopeRevision {
				status = "scope_changed"
				scopeChanged = true
			} else {
				var duplicate bool
				if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM memory_facts f JOIN memory_fact_revisions r ON r.fact_id=f.id AND r.revision=f.head_revision WHERE f.scope_key=? AND f.deleted=0 AND r.category=? AND lower(trim(r.content))=lower(trim(?)))`, item.scope.key, item.fact.Category, item.fact.Content).Scan(&duplicate); err != nil {
					return 0, err
				}
				if duplicate {
					status = "duplicate"
				} else {
					version, err = bumpScope(ctx, tx, item.scope, version)
					if err != nil {
						return 0, err
					}
					now := time.Now().UTC()
					fact := Fact{ID: rand.Text(), ScopeKey: item.scope.key, Revision: 1, Content: item.fact.Content, Category: item.fact.Category, Confidence: item.fact.Confidence, Source: item.fact.Source, CreatedAt: now, UpdatedAt: now}
					if _, err = tx.ExecContext(ctx, `INSERT INTO memory_facts(id,scope_key,head_revision,created_at) VALUES(?,?,1,?)`, fact.ID, fact.ScopeKey, now.Format(time.RFC3339Nano)); err != nil {
						return 0, err
					}
					if err = insertRevision(ctx, tx, fact, version, false); err != nil {
						return 0, err
					}
					status = "promoted"
					promoted++
				}
			}
		}
		res, err := tx.ExecContext(ctx, `UPDATE memory_staged_facts SET status=? WHERE run_id=? AND attempt_id=? AND ordinal=? AND status='staged'`, status, runID, attemptID, item.ordinal)
		if err != nil {
			return 0, err
		}
		changed, err := res.RowsAffected()
		if err != nil || changed != 1 {
			return 0, errors.Join(ErrVersionConflict, fmt.Errorf("staging row changed"))
		}
	}
	auditStatus := "discarded"
	if promote {
		auditStatus = "deduplicated"
		if scopeChanged {
			auditStatus = "scope_changed"
		}
		if promoted > 0 {
			auditStatus = "promoted"
			if scopeChanged {
				auditStatus = "partial"
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_extraction_audit SET status=?,updated_at=? WHERE run_id=? AND attempt_id=? AND status='staged'`, auditStatus, time.Now().UTC().Format(time.RFC3339Nano), runID, attemptID); err != nil {
		return 0, err
	}
	return promoted, nil
}
