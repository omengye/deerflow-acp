package learning

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

func validateOptions(o harness.SkillLearningOptions) error {
	b := o.Budget
	if b.MaxDailyModelCalls < 1 || b.MaxDailyModelCalls > 100000 || b.MaxDailyTokens < 1 || b.MaxDailyTokens > 1000000000 || b.MaxJobSeconds < 1 || b.MaxJobSeconds > 3600 || b.Workers < 1 || b.Workers > 16 ||
		o.MaxDailyModelCalls < 1 || o.MaxDailyModelCalls > b.MaxDailyModelCalls || o.MaxDailyTokens < 1 || o.MaxDailyTokens > b.MaxDailyTokens {
		return invalid("requires explicit shared budget and bounded learning sublimits")
	}
	models := []harness.SkillGenerationModelConfig{o.Model}
	if o.CrossSession && o.Consolidation == nil {
		return invalid("cross-session learning requires consolidation")
	}
	if o.Consolidation != nil {
		models = append(models, *o.Consolidation)
	}
	if o.Matching != nil {
		if o.Consolidation == nil {
			return invalid("matching requires consolidation")
		}
		models = append(models, *o.Matching)
	}
	for _, m := range models {
		if m.Timeout != 0 && (m.Timeout < time.Second || m.Timeout > 5*time.Minute) || m.MaxInputBytes != 0 && (m.MaxInputBytes < 1024 || m.MaxInputBytes > 2<<20) || m.MaxOutputTokens != 0 && (m.MaxOutputTokens < 128 || m.MaxOutputTokens > 32768) {
			return invalid("invalid model bounds")
		}
	}
	return nil
}

// Configure is startup-only. A shared queue is dispatched by the evolution
// host; otherwise this store claims only its enabled learning producers.
func (s *Store) Configure(ctx context.Context, extractor harness.SkillExperienceExtractor, shared *skillmaintenance.Store) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.done != nil || !s.enabled || s.extractor != nil || extractor == nil || !validText(extractor.Identity(), 128, true) {
		return invalid("requires a versioned extractor")
	}
	s.extractor, s.maintenance, s.shared = extractor, shared, shared != nil
	if shared == nil {
		var err error
		s.maintenance, err = skillmaintenance.NewWithHooks(ctx, s.db, s.options.Budget, s.ExtractionHooks())
		if err != nil {
			s.extractor = nil
			return err
		}
	}
	return nil
}

func (s *Store) inputLimit() int {
	if s.options.Model.MaxInputBytes != 0 {
		return s.options.Model.MaxInputBytes
	}
	return 128 << 10
}

// QueueOne debounces settled real-user turns into a bounded, immutable,
// redacted snapshot. The source cursor and typed job commit atomically.
func (s *Store) QueueOne(ctx context.Context) (bool, error) {
	if !s.enabled || s.extractor == nil || s.maintenance == nil {
		return false, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var session, episode, owner string
	var epoch int64
	var sequence int64
	err = tx.QueryRowContext(ctx, `SELECT o.sequence,o.session_id,o.episode_id,e.workspace_key,o.epoch FROM harness_learning_outbox o
 JOIN df_learning_episodes e ON e.id=o.episode_id JOIN harness_learning_controls c ON c.session_id=o.session_id
 WHERE o.state='pending' AND c.contribute=1 AND c.epoch=o.epoch AND o.sequence>(SELECT cursor FROM df_learning_scan WHERE id=1)
 AND NOT EXISTS(SELECT 1 FROM harness_learning_inputs li JOIN harness_runs r ON r.id=li.run_id
 WHERE li.episode_id=o.episode_id AND r.status NOT IN ('completed','failed','cancelled'))
 AND NOT EXISTS(SELECT 1 FROM harness_learning_outbox newer WHERE newer.episode_id=o.episode_id AND julianday(newer.ended_at)>julianday(?))
 AND NOT EXISTS(SELECT 1 FROM df_learning_extractions x JOIN df_skill_maintenance_jobs j ON j.id=x.job_id
 WHERE x.episode_id=o.episode_id AND j.state IN ('queued','retry','running'))
 AND NOT EXISTS(SELECT 1 FROM harness_learning_inputs li JOIN budget_members m ON m.root_id=li.root_id
 LEFT JOIN harness_runs r ON r.id=m.id WHERE li.episode_id=o.episode_id AND (r.id IS NULL OR r.status NOT IN ('completed','failed','cancelled')))
 ORDER BY o.sequence LIMIT 1`, s.now().UTC().Add(-s.options.Idle).Format(time.RFC3339Nano)).Scan(&sequence, &session, &episode, &owner, &epoch)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err = tx.ExecContext(ctx, `UPDATE df_learning_scan SET cursor=0 WHERE id=1`); err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	if err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE df_learning_scan SET cursor=? WHERE id=1`, sequence); err != nil {
		return false, err
	}
	// Include prior committed turns from the same task, not just today's
	// unconsumed turn. Otherwise a later correction loses what it corrected.
	var upper int64
	if err = tx.QueryRowContext(ctx, `SELECT MAX(sequence) FROM (SELECT sequence FROM harness_learning_outbox
 WHERE episode_id=? AND epoch=? AND state='pending' ORDER BY sequence LIMIT 8)`, episode, epoch).Scan(&upper); err != nil {
		return false, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT o.sequence,i.run_id,i.root_id,i.input_id,i.input_sha,i.accepted_sequence,o.frontier,o.epoch,o.status,o.ended_at,o.effects_uncertain
 FROM harness_learning_outbox o JOIN harness_learning_inputs i ON i.run_id=o.run_id
 WHERE o.sequence IN (SELECT sequence FROM harness_learning_outbox WHERE episode_id=? AND epoch=? AND sequence<=? AND state NOT IN ('skipped','cancelled') ORDER BY sequence DESC LIMIT 8) ORDER BY o.sequence`, episode, epoch, upper)
	if err != nil {
		return false, err
	}
	snap := extractionSnapshot{Session: session, Episode: episode, Owner: owner, Epoch: epoch, MaxDailyModelCalls: s.options.MaxDailyModelCalls, MaxDailyTokens: s.options.MaxDailyTokens, View: harness.SkillLearningInput{EpisodeID: episode, References: []harness.SkillLearningReference{}}}
	var skip string
	for rows.Next() {
		var src sourceRef
		var uncertain bool
		if err = rows.Scan(&src.Outbox, &src.Run, &src.Root, &src.Input, &src.InputSHA, &src.Accepted, &src.Frontier, &src.Epoch, &src.Status, &src.Ended, &uncertain); err != nil {
			break
		}
		if uncertain || src.Status == "cancelled" {
			skip = "unsafe_outcome"
		}
		snap.Sources = append(snap.Sources, src)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return false, err
	}
	if skip == "" {
		for _, src := range snap.Sources {
			settled, checkErr := settledRoot(ctx, tx, src.Root)
			if checkErr != nil {
				return false, checkErr
			}
			if !settled && skip == "" {
				// Pending descendants are retried without model I/O; they do not
				// reset the idle window or consume the independent collector.
				_, err = tx.ExecContext(ctx, `UPDATE harness_learning_outbox SET reason='tree_unsettled' WHERE episode_id=? AND state='pending'`, episode)
				if err != nil {
					return false, err
				}
				return false, tx.Commit()
			}
			refs, viewErr := inputView(ctx, tx, src)
			if viewErr != nil {
				if errors.Is(viewErr, harness.ErrInvalidInput) {
					skip = "source_bounds"
					break
				}
				return false, viewErr
			}
			snap.View.References = append(snap.View.References, refs...)
		}
	}
	if skip == "" {
		if err = s.captureRootTx(ctx, tx, &snap); err != nil {
			if errors.Is(err, harness.ErrInvalidInput) {
				skip = "source_bounds"
			} else if errors.Is(err, harness.ErrPermissionDenied) {
				skip = "unsafe_root_evidence"
			} else {
				return false, err
			}
		}
	}
	view, err := json.Marshal(snap.View)
	if err != nil {
		return false, err
	}
	if len(view) > s.inputLimit() || len(snap.View.References) > 528 {
		skip = "source_bounds"
	}
	if skip != "" {
		_, err = tx.ExecContext(ctx, `UPDATE harness_learning_outbox SET state='skipped',reason=? WHERE episode_id=? AND sequence<=? AND state='pending'`, skip, episode, upper)
		if err != nil {
			return false, err
		}
		return true, tx.Commit()
	}
	if err = s.verifySnapshotTx(ctx, tx, snap); err != nil {
		return false, err
	}
	snap.ID = extractionID(snap, s.extractor.Identity())
	payload, err := json.Marshal(snap)
	if err != nil {
		return false, err
	}
	if len(payload) > s.inputLimit()+(32<<10) {
		if _, err = tx.ExecContext(ctx, `UPDATE harness_learning_outbox SET state='skipped',reason='source_bounds' WHERE episode_id=? AND sequence<=? AND state='pending'`, episode, upper); err != nil {
			return false, err
		}
		return true, tx.Commit()
	}
	job, err := s.maintenance.EnqueueTx(ctx, tx, skillmaintenance.Request{Kind: skillmaintenance.ExperienceExtract, OwnerKind: skillmaintenance.WorkspaceOwner, OwnerKey: owner, Key: snap.ID, Reference: snap.ID, InputDigest: digest(payload)})
	if errors.Is(err, skillmaintenance.ErrBudget) {
		_, err = tx.ExecContext(ctx, `UPDATE harness_learning_outbox SET reason='queue_backpressure' WHERE episode_id=? AND state='pending'`, episode)
		if err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	if err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO df_learning_extractions(id,session_id,episode_id,job_id,epoch,policy_version,model_identity,payload,payload_sha,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, snap.ID, session, episode, job.ID, epoch, PolicyVersion, s.extractor.Identity(), payload, digest(payload), s.now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_learning_outbox SET state='queued',job_id=?,reason='' WHERE episode_id=? AND sequence<=? AND state='pending'`, job.ID, episode, upper); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
