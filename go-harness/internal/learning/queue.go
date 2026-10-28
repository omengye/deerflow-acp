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
	WHERE o.state='pending' AND c.contribute=1 AND c.epoch=o.epoch
 AND NOT (`+episodeTailActivePredicate+`)
 AND NOT EXISTS(SELECT 1 FROM harness_learning_outbox newer WHERE newer.episode_id=o.episode_id AND julianday(newer.ended_at)>julianday(?))
 AND NOT EXISTS(SELECT 1 FROM df_learning_extractions x JOIN df_skill_maintenance_jobs j ON j.id=x.job_id
 WHERE x.episode_id=o.episode_id AND j.state IN ('queued','retry','running'))
 ORDER BY CASE WHEN o.sequence>(SELECT cursor FROM df_learning_scan WHERE id=1) THEN 0 ELSE 1 END,o.sequence LIMIT 1`, s.now().UTC().Add(-s.options.Idle).Format(time.RFC3339Nano)).Scan(&sequence, &session, &episode, &owner, &epoch)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err = tx.ExecContext(ctx, `UPDATE df_learning_scan SET cursor=0 WHERE id=1`); err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	if err != nil {
		return false, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+queueSourceColumns+` FROM harness_learning_outbox o JOIN harness_learning_inputs i ON i.run_id=o.run_id
 WHERE o.episode_id=? AND o.epoch=? AND o.sequence>=? AND o.state='pending' ORDER BY o.sequence LIMIT 8`, episode, epoch, sequence)
	if err != nil {
		return false, err
	}
	candidates, err := readQueueSources(rows)
	if err != nil {
		return false, err
	}
	base := extractionSnapshot{Session: session, Episode: episode, Owner: owner, Epoch: epoch, MaxDailyModelCalls: s.options.MaxDailyModelCalls, MaxDailyTokens: s.options.MaxDailyTokens, View: harness.SkillLearningInput{EpisodeID: episode, References: []harness.SkillLearningReference{}}}
	var parts []queueSourceView
	var changed bool
	for _, candidate := range candidates {
		part, reason, sourceErr := s.queueSourceTx(ctx, tx, base, candidate)
		if sourceErr != nil {
			return false, sourceErr
		}
		if reason != "" {
			state := "skipped"
			if reason == "tree_unsettled" {
				state = "pending"
			} else {
				changed = true
			}
			if err = markQueueSourceTx(ctx, tx, candidate.source.Outbox, state, reason, ""); err != nil {
				return false, err
			}
		} else {
			proposed := append(append([]queueSourceView{}, parts...), part)
			if !s.queueSnapshotFits(snapshotWithQueueSources(base, proposed)) {
				// The source fits alone. Keep it pending for the next chunk,
				// instead of discarding all safe sources in an oversized batch.
				break
			}
			parts = proposed
		}
		sequence = candidate.source.Outbox
	}
	if _, err = tx.ExecContext(ctx, `UPDATE df_learning_scan SET cursor=? WHERE id=1`, sequence); err != nil {
		return false, err
	}
	if len(parts) == 0 {
		return changed, tx.Commit()
	}
	// Keep the original episode/independence key and recent safe corrections.
	snap, err := s.queueHistoryTx(ctx, tx, base, parts)
	if err != nil {
		return false, err
	}
	if err = s.verifySnapshotTx(ctx, tx, snap); err != nil {
		return false, err
	}
	snap.ID = extractionID(snap, s.extractor.Identity())
	payload, err := json.Marshal(snap)
	if err != nil {
		return false, err
	}
	job, err := s.maintenance.EnqueueTx(ctx, tx, skillmaintenance.Request{Kind: skillmaintenance.ExperienceExtract, OwnerKind: skillmaintenance.WorkspaceOwner, OwnerKey: owner, Key: snap.ID, Reference: snap.ID, InputDigest: digest(payload)})
	if errors.Is(err, skillmaintenance.ErrBudget) {
		for _, part := range parts {
			if err = markQueueSourceTx(ctx, tx, part.source.Outbox, "pending", "queue_backpressure", ""); err != nil {
				return false, err
			}
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
	for _, part := range parts {
		if err = markQueueSourceTx(ctx, tx, part.source.Outbox, "queued", "", job.ID); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}
