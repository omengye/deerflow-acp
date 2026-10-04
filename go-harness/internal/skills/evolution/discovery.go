package evolution

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

type discoveryScanItem struct {
	id, sourceID, source, name, version string
	previous                            string
	signalVersion, feedbackRevision     int64
}

// The durable rotating scan visits at most 32 signals per step. A scan cursor
// advances after each attempted item. An interrupted page retains its tail;
// competing scanners use cursor CAS and cannot overwrite each other's progress.
func (s *Service) discoveryScan(ctx context.Context) ([]discoveryScanItem, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE df_skill_discovery_scan SET cursor=cursor WHERE id=1`); err != nil {
		return nil, err
	}
	var cursor string
	if err = tx.QueryRowContext(ctx, `SELECT cursor FROM df_skill_discovery_scan WHERE id=1`).Scan(&cursor); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT s.id,s.source_id,s.source_key,s.name,s.version,s.state_version,COALESCE((SELECT MAX(revision) FROM df_skill_feedback WHERE signal_id=s.id),0)
 FROM df_skill_signals s WHERE s.id>? AND NOT EXISTS(SELECT 1 FROM df_skill_discovery_errors e WHERE e.signal_id=s.id AND e.signal_version=s.state_version AND e.feedback_revision=COALESCE((SELECT MAX(revision) FROM df_skill_feedback WHERE signal_id=s.id),0) AND ((e.attempts>=3 AND e.error_code='invalid_evidence') OR e.retry_ns>?)) ORDER BY s.id LIMIT 32`, cursor, s.discovery.now().UnixNano())
	if err != nil {
		return nil, err
	}
	var items []discoveryScanItem
	for rows.Next() {
		var item discoveryScanItem
		if err = rows.Scan(&item.id, &item.sourceID, &item.source, &item.name, &item.version, &item.signalVersion, &item.feedbackRevision); err != nil {
			break
		}
		item.previous = cursor
		cursor = item.id
		items = append(items, item)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		if _, err = tx.ExecContext(ctx, `UPDATE df_skill_discovery_scan SET cursor='' WHERE id=1`); err != nil {
			return nil, err
		}
	}
	return items, tx.Commit()
}

func discoverySkippable(err error) bool {
	return errors.Is(err, errDiscoveryEvidence) || errors.Is(err, harness.ErrNotFound) || errors.Is(err, harness.ErrPermissionDenied) || errors.Is(err, harness.ErrInvalidInput) || errors.Is(err, errCollectionEvidence)
}

// A cap counts queued/running maintenance and unfinished proposals once each,
// across all sources and producers. A completed job with a pending proposal
// still occupies capacity; a failed/cancelled job never refunds a daily slot.
func discoveryPending(ctx context.Context, tx *sql.Tx) (int, error) {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM df_skill_maintenance_jobs WHERE state IN ('queued','running','retry'))+
 (SELECT count(*) FROM df_skill_proposals p WHERE p.status IN ('validating','pending_review','publishing') AND NOT EXISTS(
 SELECT 1 FROM df_skill_generation_inputs i JOIN df_skill_maintenance_jobs j ON j.id=i.job_id WHERE i.proposal_id=p.id AND j.state IN ('queued','running','retry')) AND NOT EXISTS(
 SELECT 1 FROM df_skill_behavior_evaluations e JOIN df_skill_maintenance_jobs j ON j.id=e.job_id WHERE e.proposal_id=p.id AND e.manual=1 AND j.state IN ('queued','running','retry')))`).Scan(&count)
	return count, err
}

func (s *Service) discoveryConsider(ctx context.Context, item discoveryScanItem) (bool, error) {
	if s.discovery.options.DisableOperatorCorrections {
		return false, nil
	}
	target, ok := s.generation.allowed[item.sourceID]
	if !ok || target.SourceIdentity != item.source || !harness.ValidSkillName(item.name) {
		return false, nil
	}
	target.Name = item.name
	reserved := false
	err := s.registry.WithPublication(ctx, item.sourceID, item.name, func(tx *skills.PublicationTx) error {
		if tx.Target() != target {
			return harness.ErrPermissionDenied
		}
		now := s.discovery.now().UTC()
		result, err := tx.SQL().ExecContext(ctx, `UPDATE df_skill_discovery_scan SET last_ns=? WHERE id=1 AND last_ns<=?`, now.UnixNano(), now.UnixNano())
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return skillmaintenance.ErrClock
		}
		head, err := tx.Current()
		if err != nil {
			return err
		}
		remove := func(id string) error {
			_, e := tx.SQL().ExecContext(ctx, `DELETE FROM df_skill_discovery_candidates WHERE signal_id=?`, id)
			return e
		}
		if !head.Exists || !head.Enabled || head.Version != item.version {
			return remove(item.id)
		}
		sig, fb, err := discoveryFeedback(ctx, tx.SQL(), item.id, target, head.Version)
		if errors.Is(err, harness.ErrInvalidInput) || errors.Is(err, errCollectionEvidence) {
			return err
		}
		if discoverySkippable(err) {
			return remove(item.id)
		}
		if err != nil {
			return err
		}
		created, _ := time.Parse(time.RFC3339Nano, fb.CreatedAt)
		window := now.Add(-time.Duration(s.discovery.options.RepeatWindowDays) * 24 * time.Hour)
		if created.Before(window) || created.After(now) {
			return remove(item.id)
		}
		var consumed bool
		if err = tx.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_discovery_consumed WHERE root_digest=?)`, discoveryRootKey(sig.RootID, target, head.Version)).Scan(&consumed); err != nil {
			return err
		}
		if consumed {
			return remove(item.id)
		}
		if _, err = tx.SQL().ExecContext(ctx, `INSERT INTO df_skill_discovery_candidates(signal_id,source_key,name,version) VALUES(?,?,?,?) ON CONFLICT(signal_id) DO UPDATE SET source_key=excluded.source_key,name=excluded.name,version=excluded.version`, item.id, target.SourceIdentity, target.Name, head.Version); err != nil {
			return err
		}
		var until int64
		err = tx.SQL().QueryRowContext(ctx, `SELECT until_ns FROM df_skill_discovery_cooldowns WHERE source_key=? AND name=?`, target.SourceIdentity, target.Name).Scan(&until)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if until > now.UnixNano() {
			return nil
		}
		var daily int
		err = tx.SQL().QueryRowContext(ctx, `SELECT reservations FROM df_skill_discovery_days WHERE day=?`, now.Format("2006-01-02")).Scan(&daily)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if daily >= s.discovery.options.MaxDailyProposals {
			return nil
		}
		pending, err := discoveryPending(ctx, tx.SQL())
		if err != nil {
			return err
		}
		if pending >= s.discovery.options.MaxPendingProposals {
			return nil
		}
		var busy bool
		if err = tx.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_maintenance_jobs WHERE source_key=? AND name=? AND state IN ('queued','running','retry')) OR EXISTS(SELECT 1 FROM df_skill_proposals WHERE source_key=? AND name=? AND status IN ('validating','pending_review','publishing'))`, target.SourceIdentity, target.Name, target.SourceIdentity, target.Name).Scan(&busy); err != nil {
			return err
		}
		if busy {
			return nil
		}
		rows, err := tx.SQL().QueryContext(ctx, `SELECT signal_id FROM df_skill_discovery_candidates WHERE source_key=? AND name=? AND version=? ORDER BY signal_id LIMIT 32`, target.SourceIdentity, target.Name, head.Version)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				break
			}
			ids = append(ids, id)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		refs := make([]harness.SkillDiscoveryEvidence, 0, s.discovery.options.RepeatThreshold)
		roots := map[string]bool{}
		for _, id := range ids {
			signal, feedback, err := discoveryFeedback(ctx, tx.SQL(), id, target, head.Version)
			if discoverySkippable(err) {
				if err = remove(id); err != nil {
					return err
				}
				continue
			}
			if err != nil {
				return err
			}
			created, _ := time.Parse(time.RFC3339Nano, feedback.CreatedAt)
			if created.Before(window) || created.After(now) {
				if err = remove(id); err != nil {
					return err
				}
				continue
			}
			if err = tx.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_discovery_consumed WHERE root_digest=?)`, discoveryRootKey(signal.RootID, target, head.Version)).Scan(&consumed); err != nil {
				return err
			}
			if consumed {
				if err = remove(id); err != nil {
					return err
				}
				continue
			}
			if roots[signal.RootID] {
				continue
			}
			roots[signal.RootID] = true
			refs = append(refs, harness.SkillDiscoveryEvidence{SignalID: id, RootID: signal.RootID, SignalStateVersion: signal.StateVersion, FeedbackRevision: feedback.Revision, ReadIDs: feedback.ReadIDs})
			if len(refs) == s.discovery.options.RepeatThreshold {
				break
			}
		}
		if len(refs) < s.discovery.options.RepeatThreshold {
			return nil
		}
		overlay, err := tx.OverlayBase()
		if err != nil {
			return err
		}
		d := discoverySnapshot{ID: "sd_" + rand.Text(), Target: target, Base: head, OverlayBase: overlay, Options: s.discovery.options, CreatedAt: now, Evidence: refs}
		payload, err := json.Marshal(d)
		if err != nil {
			return err
		}
		if len(payload) > 32<<10 {
			return invalid("discovery snapshot exceeds bounds")
		}
		request := harness.SkillGenerationRequest{SourceID: target.SourceID, Name: target.Name, RequestKey: "discovery:" + d.ID, Reason: "Review repeated explicit operator corrections. Their skill-quality causality remains unassessed.", Author: "discovery", Base: head}
		r := generationRecord{id: "sgi_" + rand.Text(), created: now, input: generationSnapshot{Request: request, Target: target, OverlayBase: overlay, GeneratorIdentity: s.generation.identity, ValidatorIdentity: s.generation.reviewIdentity, DiscoveryID: d.ID, DiscoveryDigest: hash(payload), BehaviorIdentity: s.behavior.bindingIdentity(target)}}
		input, err := json.Marshal(r.input)
		if err != nil {
			return err
		}
		r.job, err = s.generation.store.EnqueueTx(ctx, tx.SQL(), skillmaintenance.Request{SourceIdentity: target.SourceIdentity, Name: target.Name, Key: request.RequestKey, Reference: r.id, InputDigest: hash(input)})
		if err != nil {
			return err
		}
		if _, err = tx.SQL().ExecContext(ctx, `INSERT INTO df_skill_generation_inputs(id,job_id,source_key,name,request_key,request_sha,payload,progress_sha,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, r.id, r.job.ID, target.SourceIdentity, target.Name, request.RequestKey, generationRequestHash(request, target), input, generationProgress(r), now.Format(time.RFC3339Nano)); err != nil {
			return err
		}
		if _, err = tx.SQL().ExecContext(ctx, `INSERT INTO df_skill_discoveries(id,source_key,name,job_id,payload,payload_sha,created_ns) VALUES(?,?,?,?,?,?,?)`, d.ID, target.SourceIdentity, target.Name, r.job.ID, payload, hash(payload), now.UnixNano()); err != nil {
			return err
		}
		for _, ref := range refs {
			if _, err = tx.SQL().ExecContext(ctx, `INSERT INTO df_skill_discovery_consumed(root_digest,discovery_id) VALUES(?,?)`, discoveryRootKey(ref.RootID, target, head.Version), d.ID); err != nil {
				return err
			}
			if err = remove(ref.SignalID); err != nil {
				return err
			}
		}
		if _, err = tx.SQL().ExecContext(ctx, `INSERT INTO df_skill_discovery_days(day,reservations) VALUES(?,1) ON CONFLICT(day) DO UPDATE SET reservations=reservations+1`, now.Format("2006-01-02")); err != nil {
			return err
		}
		if _, err = tx.SQL().ExecContext(ctx, `INSERT INTO df_skill_discovery_cooldowns(source_key,name,until_ns) VALUES(?,?,?) ON CONFLICT(source_key,name) DO UPDATE SET until_ns=excluded.until_ns`, target.SourceIdentity, target.Name, now.Add(time.Duration(d.Options.CooldownHours)*time.Hour).UnixNano()); err != nil {
			return err
		}
		reserved = true
		return nil
	})
	if discoverySkippable(err) && !errors.Is(err, harness.ErrInvalidInput) && !errors.Is(err, errCollectionEvidence) {
		return false, nil
	}
	return reserved, err
}

// RunDiscovery performs one bounded metadata-only pass. Provider calls run in
// the existing separately budgeted generation worker after atomic reservation.
func (s *Service) RunDiscovery(ctx context.Context) (bool, error) {
	if s.discovery == nil {
		return false, invalid("discovery is not configured")
	}
	derivedWork, derivedErr := s.runDerivedDiscovery(ctx)
	if ctx.Err() != nil {
		return derivedWork, errors.Join(derivedErr, ctx.Err())
	}
	items, err := s.discoveryScan(ctx)
	if err != nil {
		return false, err
	}
	combined := derivedErr
	for _, item := range items {
		if ctx.Err() != nil {
			return len(items) > 0, errors.Join(combined, ctx.Err())
		}
		_, err := s.discoveryConsider(ctx, item)
		if errors.Is(err, skillmaintenance.ErrBudget) {
			err = nil
		} // Backpressure is not invalid evidence.
		if err != nil && ctx.Err() == nil {
			err = errors.Join(err, s.discoveryRecordError(ctx, item, err))
		}
		combined = errors.Join(combined, err)
		advanced, advanceErr := s.advanceDiscoveryScan(ctx, item)
		if advanceErr != nil {
			return true, errors.Join(combined, advanceErr)
		}
		if !advanced {
			break
		}
	}
	return len(items) > 0 || derivedWork, combined
}

func (s *Service) advanceDiscoveryScan(ctx context.Context, item discoveryScanItem) (bool, error) {
	// An attempted slow item must not keep the scan pinned merely because its
	// step deadline expired. Only this tiny cursor commit survives cancellation.
	cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer stop()
	result, err := s.db.ExecContext(cleanup, `UPDATE df_skill_discovery_scan SET cursor=? WHERE id=1 AND cursor=?`, item.id, item.previous)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (s *Service) discoveryRecordError(ctx context.Context, item discoveryScanItem, cause error) error {
	code := "storage_error"
	if errors.Is(cause, harness.ErrInvalidInput) || errors.Is(cause, errCollectionEvidence) {
		code = "invalid_evidence"
	}
	if errors.Is(cause, skillmaintenance.ErrClock) {
		code = "clock_reversed"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Changes to either immutable evidence revision start a new bounded retry
	// sequence. Unchanged poison records stop after three metadata-only checks.
	_, err = tx.ExecContext(ctx, `INSERT INTO df_skill_discovery_errors(signal_id,signal_version,feedback_revision,attempts,retry_ns,error_code)
 SELECT id,?,?,1,?,? FROM df_skill_signals WHERE id=? AND state_version=?
 ON CONFLICT(signal_id) DO UPDATE SET signal_version=excluded.signal_version,feedback_revision=excluded.feedback_revision,
 attempts=CASE WHEN signal_version=excluded.signal_version AND feedback_revision=excluded.feedback_revision AND error_code=excluded.error_code THEN MIN(attempts+1,3) ELSE 1 END,
 retry_ns=excluded.retry_ns,error_code=excluded.error_code`, item.signalVersion, item.feedbackRevision, s.discovery.now().Add(5*time.Second).UnixNano(), code, item.id, item.signalVersion)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE df_skill_discovery_health SET last_error_code=? WHERE id=1`, code); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) StartDiscovery() error {
	d := s.discovery
	if d == nil {
		return nil
	}
	d.lifecycle.mu.Lock()
	defer d.lifecycle.mu.Unlock()
	if d.lifecycle.closed {
		return invalid("discovery worker is closed")
	}
	if d.lifecycle.done != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.lifecycle.cancel = cancel
	d.lifecycle.done = make(chan struct{})
	go func() {
		defer close(d.lifecycle.done)
		warned := false
		for {
			step, stop := context.WithTimeout(ctx, 5*time.Second)
			_, err := s.RunDiscovery(step)
			stop()
			if ctx.Err() != nil {
				return
			}
			if err != nil && !warned {
				slog.Warn("skill discovery maintenance failed; user task state is unchanged")
			}
			warned = err != nil
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
	return nil
}

func (s *Service) CloseDiscovery() error {
	d := s.discovery
	if d == nil {
		return nil
	}
	d.lifecycle.mu.Lock()
	d.lifecycle.closed = true
	if d.lifecycle.cancel != nil {
		d.lifecycle.cancel()
	}
	done := d.lifecycle.done
	d.lifecycle.mu.Unlock()
	if done != nil {
		<-done
	}
	return nil
}
