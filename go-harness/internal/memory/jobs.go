package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

// Job contains only accepted human text and pinned attribute versions, never
// assistant claims, tool output, credentials, or a live model/session handle.
type Job struct {
	BudgetPolicy     string                      `json:"budget_policy"`
	MemberID         string                      `json:"member_id"`
	ClaimToken       string                      `json:"-"`
	WorkerAttemptID  string                      `json:"-"`
	Fence            int64                       `json:"-"`
	OperationID      string                      `json:"-"`
	Version          int                         `json:"version"`
	WritePolicy      string                      `json:"write_policy"`
	ExtractionSchema int                         `json:"extraction_schema"`
	Provenance       string                      `json:"provenance"`
	RootBudgetID     string                      `json:"root_budget_id"`
	SessionID        string                      `json:"session_id"`
	Channel          string                      `json:"-"`
	Result           string                      `json:"-"`
	ID               string                      `json:"id"`
	RunID            string                      `json:"run_id"`
	InputID          string                      `json:"input_id"`
	AttemptID        string                      `json:"attempt_id"`
	Workspace        string                      `json:"-"`
	ProfileID        string                      `json:"-"`
	Text             string                      `json:"-"`
	Sequence         int64                       `json:"-"`
	Versions         map[string]map[string]int64 `json:"-"`
}

func (s *Store) Enqueue(ctx context.Context, j Job) error {
	if j.AttemptID == "" {
		return harness.ErrInvalidInput
	}
	return optionalTransact(ctx, s.db, func(tx *sql.Tx) error {
		m, err := inputSnapshot(ctx, tx, j.RunID, j.InputID)
		if err != nil {
			return err
		}
		if !m.AutomaticEligible {
			return nil
		}
		if m.WritePolicy != WritePolicy {
			return ErrSourcePolicyMissing
		}
		if s.ledger == nil {
			return budget.ErrScope
		}
		var decision string
		if err := tx.QueryRowContext(ctx, "SELECT decision FROM memory_inputs WHERE input_id=?", m.InputID).Scan(&decision); err != nil {
			return err
		}
		if decision != "pending" && decision != "started" {
			return nil
		}
		var acceptedAttempt bool
		if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM budget_attempts WHERE id=? AND member_id=? AND root_id=? AND session_id=? AND state='active')", j.AttemptID, m.RunID, m.RootBudgetID, m.SessionID).Scan(&acceptedAttempt); err != nil {
			return &budget.PersistenceError{Operation: "memory source attempt", Err: err}
		}
		if !acceptedAttempt {
			return budget.ErrScope
		}
		explicit, err := explicitActionTx(ctx, tx, j.RunID, j.InputID)
		if err != nil {
			return err
		}
		if explicit {
			_, err = tx.ExecContext(ctx, `UPDATE memory_inputs SET decision='explicit_action' WHERE input_id=?`, m.InputID)
			return err
		}
		provenance, err := contextProvenance(ctx, tx, m)
		if err != nil {
			return err
		}
		j = Job{Version: 2, BudgetPolicy: JobBudgetPolicy, WritePolicy: WritePolicy, ExtractionSchema: ExtractionSchemaVersion, Provenance: provenance,
			ID: OperationID("extract/v3", m.RunID, m.InputID), RunID: m.RunID, InputID: m.InputID,
			AttemptID: j.AttemptID, RootBudgetID: m.RootBudgetID, SessionID: m.SessionID}
		j.MemberID = jobMember(j.ID)
		if err = s.ledger.BindMemberTx(ctx, tx, budget.MemberBinding{RootBudgetID: j.RootBudgetID, MemberID: j.MemberID, SessionID: j.SessionID, OriginRunID: j.RunID, ParentMemberID: j.RunID, Kind: "memory_job"}); err != nil {
			return err
		}
		raw, err := json.Marshal(j)
		if err != nil {
			return err
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err = tx.ExecContext(ctx, `INSERT INTO memory_jobs(id,run_id,input_id,attempt_id,payload,status,available_at,updated_at) VALUES(?,?,?,?,?,'queued',?,?) ON CONFLICT(id) DO NOTHING`, j.ID, j.RunID, j.InputID, j.AttemptID, raw, now, now); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO memory_job_leases(job_id) VALUES(?) ON CONFLICT(job_id) DO NOTHING", j.ID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE memory_inputs SET decision='queued',job_id=? WHERE input_id=?`, j.ID, j.InputID)
		return err
	})
}

func hydrateJob(ctx context.Context, tx *sql.Tx, j *Job) error {
	if j.Version != 2 || j.ID != OperationID("extract/v3", j.RunID, j.InputID) {
		return ErrSourceSnapshotMissing
	}
	if j.WritePolicy != WritePolicy || j.ExtractionSchema != ExtractionSchemaVersion || j.BudgetPolicy != JobBudgetPolicy || j.MemberID != jobMember(j.ID) {
		return ErrSourcePolicyMissing
	}
	m, err := inputSnapshot(ctx, tx, j.RunID, j.InputID)
	if err != nil {
		return err
	}
	if m.WritePolicy != WritePolicy {
		return ErrSourcePolicyMissing
	}
	if !m.AutomaticEligible || j.SessionID != m.SessionID || j.RootBudgetID != m.RootBudgetID {
		return harness.ErrPermissionDenied
	}
	j.ProfileID, j.Workspace, j.Text, j.Sequence = m.ProfileID, m.Workspace, m.Text, m.Sequence
	j.Versions = map[string]map[string]int64{}
	for kind, scope := range m.Scopes {
		j.Versions[string(kind)] = scope.Slots
	}
	scope, err := j.scope(WorkspaceScope)
	if err != nil {
		return err
	}
	err = tx.QueryRowContext(ctx, `SELECT channel FROM memory_channel_bindings WHERE workspace=?`, scope.workspace).Scan(&j.Channel)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return err
}

func (j Job) scope(kind ScopeKind) (Scope, error) {
	if kind == GlobalScope {
		return NewScope(GlobalScope, "", "", j.ProfileID, "")
	}
	if kind == WorkspaceScope {
		return NewScope(WorkspaceScope, j.Workspace, "", "", "")
	}
	return Scope{}, harness.ErrInvalidInput
}

// Claim only completed foreground turns. Recovery keeps the attempt counter;
// failures and process crashes therefore cannot cause an unbounded retry loop.
func discardLegacyJobsTx(ctx context.Context, tx *sql.Tx) error {
	// No accepted-time snapshot may be reconstructed from today's state, even
	// when a legacy worker saved a complete model result before it stopped.
	if _, err := tx.ExecContext(ctx, `INSERT INTO memory_job_dispositions(job_id,reason,usage_state)
 SELECT id,'legacy_unverifiable','legacy_usage_unavailable' FROM memory_jobs j
 WHERE status IN ('queued','running') AND
 ((CASE WHEN json_valid(CAST(payload AS TEXT)) THEN COALESCE(json_extract(CAST(payload AS TEXT),'$.version'),0) ELSE 0 END)<>2
 OR (CASE WHEN json_valid(CAST(payload AS TEXT)) THEN COALESCE(json_extract(CAST(payload AS TEXT),'$.write_policy'),'') ELSE '' END)<>'memory-write-v2'
 OR (CASE WHEN json_valid(CAST(payload AS TEXT)) THEN COALESCE(json_extract(CAST(payload AS TEXT),'$.extraction_schema'),0) ELSE 0 END)<>2
 OR (CASE WHEN json_valid(CAST(payload AS TEXT)) THEN COALESCE(json_extract(CAST(payload AS TEXT),'$.budget_policy'),'') ELSE '' END)<>'memory-job-budget-v1'
 OR NOT EXISTS(SELECT 1 FROM memory_inputs i WHERE i.input_id=j.input_id AND i.run_id=j.run_id AND
 CASE WHEN json_valid(CAST(i.manifest AS TEXT)) THEN json_extract(CAST(i.manifest AS TEXT),'$.write_policy')='memory-write-v2' ELSE 1 END))
 ON CONFLICT(job_id) DO NOTHING`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE memory_jobs SET status='discarded',error_code='legacy_unverifiable'
 WHERE status IN ('queued','running') AND id IN (SELECT job_id FROM memory_job_dispositions WHERE reason='legacy_unverifiable')`)
	return err
}

func (s *Store) ClaimJob(ctx context.Context) (Job, error) {
	var j Job
	if err := s.recoverBudgetJobs(ctx, false); err != nil {
		return j, err
	}
	err := transact(ctx, s.db, func(tx *sql.Tx) error {
		if err := discardLegacyJobsTx(ctx, tx); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE memory_jobs SET status='completed',error_code='explicit_action' WHERE status='queued' AND run_id IN (SELECT id FROM harness_runs WHERE status='completed') AND EXISTS(SELECT 1 FROM memory_operation_sources o WHERE o.run_id=memory_jobs.run_id AND o.input_id=memory_jobs.input_id)`)
		if err != nil {
			return err
		}
		// A persisted daily limit bounds automatic extraction independently of
		// foreground conversations. Retries consume the same daily allowance.
		var used int
		day := time.Now().UTC().Format("2006-01-02")
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT attempts FROM memory_daily_attempts WHERE day=?),0)`, day).Scan(&used); err != nil {
			return err
		}
		if used >= s.dailyAttempts {
			next := time.Now().UTC().Truncate(24 * time.Hour).Add(24 * time.Hour).Format(time.RFC3339Nano)
			if _, err = tx.ExecContext(ctx, "UPDATE memory_jobs SET error_code='daily_limit',available_at=? WHERE status='queued' AND result='' AND available_at<=?", next, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				return err
			}
		}
		var selectedID string
		err = tx.QueryRowContext(ctx, `SELECT j.id FROM memory_jobs j JOIN harness_runs r ON r.id=j.run_id WHERE j.status='queued' AND (j.result<>'' OR (j.attempts<3 AND ?)) AND (j.available_at<=? OR (j.error_code='daily_limit' AND ?)) AND r.status='completed' ORDER BY (j.result<>'') DESC,j.available_at,j.id LIMIT 1`, used < s.dailyAttempts, time.Now().UTC().Format(time.RFC3339Nano), used < s.dailyAttempts).Scan(&selectedID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		j, _, err = storedJob(ctx, tx, selectedID)
		if err == nil {
			err = hydrateJob(ctx, tx, &j)
		}
		if err != nil {
			if !sourceIntegrityError(err) {
				return err
			}
			j = Job{}
			return s.discardCorruptJobTx(ctx, tx, selectedID)
		}
		if err = s.claimBudgetJobTx(ctx, tx, &j); err != nil {
			return err
		}
		var state string
		if err = tx.QueryRowContext(ctx, "SELECT status FROM memory_jobs WHERE id=?", j.ID).Scan(&state); err != nil {
			return err
		}
		if state != "running" {
			j = Job{}
		}
		return nil
	})
	if err != nil {
		return Job{}, err
	}
	if j.ID == "" {
		return j, harness.ErrNotFound
	}
	return j, err
}

// SaveJobResult is retained for callers compiled against the previous internal
// protocol. Raw text without a leased, metered response cannot authorize a write.
func (s *Store) SaveJobResult(ctx context.Context, id, result string) error { return budget.ErrScope }

type ExtractedAttribute struct {
	validated     *ValidatedWrite
	Scope         ScopeKind `json:"scope"`
	Key           string    `json:"key"`
	Content       string    `json:"content"`
	Category      string    `json:"category"`
	Evidence      string    `json:"evidence"`
	Value         string    `json:"value"`
	EvidenceStart *int      `json:"evidence_start"`
	EvidenceEnd   *int      `json:"evidence_end"`
	Durability    string    `json:"durability"`
	Authority     string    `json:"authority"`
	Confidence    *float64  `json:"confidence"`
	Ordinal       int       `json:"-"`
}

var ExtractionInstruction = ExtractionPrompt([]ScopeKind{GlobalScope, WorkspaceScope}, true)

func ParseAttributes(raw string, text string) ([]ExtractedAttribute, error) {
	facts, _, err := ParseExtraction([]byte(raw), text, []ScopeKind{GlobalScope, WorkspaceScope}, true)
	return facts, err
}

func (s *Store) ApplyJob(ctx context.Context, j Job, raw string) error {
	return transact(ctx, s.db, func(tx *sql.Tx) error {
		stored, status, err := storedJob(ctx, tx, j.ID)
		if err != nil {
			return err
		}
		if status == "completed" && stored.Result != "" {
			if stored.Result != raw {
				return ErrVersionConflict
			}
			return nil
		}
		current, lease, err := s.leasedJobTx(ctx, tx, j)
		if err != nil {
			return err
		}
		if stored.Result == "" || stored.Result != raw || !lease.saved {
			return budget.ErrScope
		}
		var settled bool
		if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM budget_reservations WHERE id=? AND state='settled' AND attempt_id=?)", lease.operation, lease.attempt).Scan(&settled); err != nil {
			return err
		}
		if !settled {
			return budget.ErrUnknown
		}
		j = current
		if err := hydrateJob(ctx, tx, &j); err != nil {
			return err
		}
		if err = jobSourceReadyTx(ctx, tx, j); err != nil {
			if reason, ok := sourceRefusal(err); ok {
				return finishJobStateTx(ctx, tx, j, "discarded", reason, reason, time.Now())
			}
			return err
		}
		explicit, err := explicitActionTx(ctx, tx, j.RunID, j.InputID)
		if err != nil {
			return err
		}
		if explicit {
			return finishJobStateTx(ctx, tx, j, "completed", "explicit_action", "explicit_action", time.Now())
		}
		attributes, rejected, err := ParseExtraction([]byte(raw), j.Text, []ScopeKind{GlobalScope, WorkspaceScope}, true)
		if err != nil {
			return err
		}
		if err = saveExtractionResultTx(ctx, tx, j.ID, j.RunID, j.InputID, raw, j.Provenance); err != nil {
			return err
		}
		if err = recordItemsTx(ctx, tx, j.ID, rejected); err != nil {
			return err
		}
		// Checking explicit actions and publishing share this write transaction.
		// A competing explicit mutation cannot enter a check-then-write gap.
		applied := 0
		eligible := 0
		requiresExplicit := 0
		for _, f := range attributes {
			if j.Channel != "" && f.Category == "preference" && channelStyle(f.Key, f.Content) {
				if err = recordItemsTx(ctx, tx, j.ID, []Rejection{{f.Ordinal, "channel_style"}}); err != nil {
					return err
				}
				continue
			}
			eligible++
			scope, err := j.scope(f.Scope)
			if err != nil {
				return err
			}
			op := OperationID("memory-op-v2", j.ID, ExtractionPolicyV2, fmt.Sprint(ExtractionSchemaVersion), fmt.Sprint(f.Ordinal))
			receipt, err := s.mutateTx(ctx, tx, scope, Mutation{SchemaVersion: 2, RequireSnapshot: true,
				Validated:   f.validated,
				OperationID: op, Action: "remember", Key: f.Key,
				Content: f.Content, Category: f.Category, ExpectedRevision: j.Versions[string(f.Scope)][f.Key], Automatic: true,
				Source: Source{ID: op, Kind: "model", RunID: j.RunID, InputID: j.InputID, EventSequence: j.Sequence, PolicyVersion: ExtractionPolicyV2, Provenance: j.Provenance}})
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO memory_extraction_items(result_id,ordinal,status,reason,operation_id) VALUES(?,?,?,?,?) ON CONFLICT(result_id,ordinal) DO NOTHING", j.ID, f.Ordinal, receipt.Status, receipt.Reason, op); err != nil {
				return err
			}
			if receipt.Status == "saved" || receipt.Status == "updated" || receipt.Status == "unchanged" {
				applied++
			}
			if receipt.Status == "requires_explicit" {
				requiresExplicit++
			}
		}
		decision := "processed"
		if eligible == 0 {
			decision = "no_facts"
		} else if applied == 0 {
			decision = "superseded"
			if requiresExplicit > 0 {
				decision = "requires_explicit"
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE memory_inputs SET decision=? WHERE input_id=?`, decision, j.InputID)
		if err != nil {
			return err
		}
		return finishJobStateTx(ctx, tx, j, "completed", "", decision, time.Now())
	})
}

func channelStyle(key, content string) bool {
	value := strings.ToLower(key + " " + content)
	for _, marker := range []string{"language", "reply", "response", "answer", "tone", "voice", "speech", "brevity", "concise", "中文", "语音", "简短", "简洁", "回复", "回答"} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func (s *Store) FinishJob(ctx context.Context, id string, failed bool) error { return budget.ErrScope }

func (s *Store) JobStatus(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status,count(*) FROM memory_jobs GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	status := map[string]int{}
	for rows.Next() {
		var key string
		var n int
		if err = rows.Scan(&key, &n); err != nil {
			return nil, err
		}
		status[key] = n
	}
	return status, rows.Err()
}
