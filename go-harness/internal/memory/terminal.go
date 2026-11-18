package memory

import (
	"context"
	"database/sql"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// This runs in the host terminal transaction, after budget and staging settle.
// Never reclassify started/failed extraction, queued work, or resumable runs.
func (s *Store) SettleInputDecisionTx(ctx context.Context, tx *sql.Tx, runID string, status harness.ExecutionStatus, skipped bool) error {
	if status == harness.ExecutionFailed || status == harness.ExecutionCancelled {
		_, err := tx.ExecContext(ctx, `UPDATE memory_inputs SET decision='discarded' WHERE run_id=? AND job_id='' AND decision='pending'`, runID)
		return err
	}
	if status != harness.ExecutionCompleted || !skipped {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE memory_inputs SET decision='postrun_skipped'
 WHERE run_id=? AND decision='pending' AND job_id=''
 AND NOT EXISTS(SELECT 1 FROM memory_jobs WHERE run_id=memory_inputs.run_id)
 AND NOT EXISTS(SELECT 1 FROM memory_extraction_audit WHERE run_id=memory_inputs.run_id)
 AND NOT EXISTS(SELECT 1 FROM memory_extraction_results WHERE run_id=memory_inputs.run_id)
 AND NOT EXISTS(SELECT 1 FROM memory_staged_facts WHERE run_id=memory_inputs.run_id)
 AND NOT EXISTS(SELECT 1 FROM memory_operation_sources WHERE run_id=memory_inputs.run_id)`, runID)
	return err
}

// Historical repair uses durable host evidence, not the absence of a job alone.
// Schema-only tools and minimal stores have no execution journal and are skipped.
func (s *Store) reconcileSkippedInputsTx(ctx context.Context, tx *sql.Tx) error {
	var available bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='harness_executions')`).Scan(&available); err != nil {
		return err
	}
	if !available {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `WITH event_evidence AS (
 SELECT run_id, json_valid(CAST(event AS TEXT)) AS valid,
 json_extract(CASE WHEN json_valid(CAST(event AS TEXT)) THEN CAST(event AS TEXT) ELSE '{}' END,'$.kind') AS kind,
 json_extract(CASE WHEN json_valid(CAST(event AS TEXT)) THEN CAST(event AS TEXT) ELSE '{}' END,'$.text') AS text
 FROM harness_events)
 SELECT i.run_id,i.input_id FROM memory_inputs i
 JOIN harness_runs r ON r.id=i.run_id AND r.input_id=i.input_id
 JOIN harness_executions e ON e.run_id=r.id AND e.input_id=i.input_id
 JOIN budget_roots b ON b.id=e.root_budget_id
 WHERE i.decision='pending' AND i.job_id='' AND r.status='completed'
 AND e.status='completed' AND e.blocked_reason='' AND b.blocked_reason=''
 AND NOT EXISTS(SELECT 1 FROM budget_attempts a WHERE a.root_id=b.id AND a.state='active')
 AND NOT EXISTS(SELECT 1 FROM budget_reservations v WHERE v.root_id=b.id AND v.state IN ('reserved','dispatched','unknown'))
 AND NOT EXISTS(SELECT 1 FROM event_evidence h WHERE h.run_id=r.id AND NOT h.valid)
 AND EXISTS(SELECT 1 FROM event_evidence h WHERE h.run_id=r.id AND h.kind='user_message')
 AND (r.stop_reason IN ('max_tokens','max_turn_requests','refusal') OR
 (r.stop_reason='end_turn'
 AND EXISTS(SELECT 1 FROM event_evidence h WHERE h.run_id=r.id AND h.kind='model_timing')
 AND NOT EXISTS(SELECT 1 FROM event_evidence h WHERE h.run_id=r.id AND
 (h.kind='tool_start' OR (h.kind='text_delta' AND COALESCE(h.text,'')<>'')))))`)
	if err != nil {
		return err
	}
	var ids [][2]string
	for rows.Next() {
		var id [2]string
		if err = rows.Scan(&id[0], &id[1]); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		source, err := inputSnapshot(ctx, tx, id[0], id[1])
		if sourceIntegrityError(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !source.AutomaticEligible || source.WritePolicy != WritePolicy {
			continue
		}
		if err = s.SettleInputDecisionTx(ctx, tx, id[0], harness.ExecutionCompleted, true); err != nil {
			return err
		}
	}
	return nil
}
