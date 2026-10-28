package learning

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// A newly accepted continuation defers paid work until it ends. Once a later
// turn has ended, older unfinished roots are isolated instead of holding the
// entire conversation forever. Every selected root is still checked separately.
// The outer query supplies the episode and consent epoch through alias o.
const episodeTailActivePredicate = `EXISTS(SELECT 1 FROM harness_learning_inputs active JOIN harness_runs r ON r.id=active.run_id
 WHERE active.episode_id=o.episode_id AND active.epoch=o.epoch AND r.status NOT IN ('completed','failed','cancelled')
 AND active.accepted_sequence>COALESCE((SELECT MAX(ended.accepted_sequence) FROM harness_learning_inputs ended
 JOIN harness_learning_outbox terminal ON terminal.run_id=ended.run_id JOIN harness_runs ended_run ON ended_run.id=ended.run_id
 WHERE ended.episode_id=o.episode_id AND ended.epoch=o.epoch AND ended_run.status IN ('completed','failed','cancelled')),0))`

func episodeTailActiveTx(ctx context.Context, tx *sql.Tx, episode string, epoch int64) (bool, error) {
	var active bool
	err := tx.QueryRowContext(ctx, `SELECT `+episodeTailActivePredicate+` FROM (SELECT ? AS episode_id,? AS epoch) o`, episode, epoch).Scan(&active)
	return active, err
}

type queuedSource struct {
	source    sourceRef
	uncertain bool
}

const queueSourceColumns = `o.sequence,i.run_id,i.root_id,i.input_id,i.input_sha,i.accepted_sequence,o.frontier,o.epoch,o.status,o.ended_at,o.effects_uncertain`

func readQueueSources(rows *sql.Rows) ([]queuedSource, error) {
	var sources []queuedSource
	var err error
	for rows.Next() {
		var candidate queuedSource
		src := &candidate.source
		if err = rows.Scan(&src.Outbox, &src.Run, &src.Root, &src.Input, &src.InputSHA, &src.Accepted, &src.Frontier, &src.Epoch, &src.Status, &src.Ended, &candidate.uncertain); err != nil {
			break
		}
		sources = append(sources, candidate)
	}
	return sources, errors.Join(err, rows.Err(), rows.Close())
}

type queueSourceView struct {
	source   sourceRef
	refs     []harness.SkillLearningReference
	rootRefs []harness.SkillLearningReference
	roots    []rootCapture
	policy   string
}

// Validate each source before combining it with another. A cancelled or damaged
// source must never consume an unrelated, safely completed pending input.
func (s *Store) queueSourceTx(ctx context.Context, tx *sql.Tx, base extractionSnapshot, candidate queuedSource) (queueSourceView, string, error) {
	part := queueSourceView{source: candidate.source}
	if candidate.uncertain || candidate.source.Status == "cancelled" {
		return part, "unsafe_outcome", nil
	}
	settled, err := settledRoot(ctx, tx, candidate.source.Root)
	if err != nil {
		return part, "", err
	}
	if !settled {
		return part, "tree_unsettled", nil
	}
	part.refs, err = verifiedSourceViewTx(ctx, tx, candidate.source, base.Session, base.Episode, base.Epoch)
	if err == nil {
		base.Sources = []sourceRef{candidate.source}
		base.View.References = part.refs
		err = s.captureRootTx(ctx, tx, &base)
	}
	if err == nil {
		part.rootRefs = base.View.References[len(part.refs):]
		part.roots, part.policy = base.Roots, base.RootPolicy
		if !s.queueSnapshotFits(base) {
			return part, "source_bounds", nil
		}
	}
	switch {
	case errors.Is(err, ErrSourceBounds):
		return part, "source_bounds", nil
	case errors.Is(err, harness.ErrInvalidInput):
		return part, "source_invalid", nil
	case errors.Is(err, harness.ErrPermissionDenied), errors.Is(err, sql.ErrNoRows):
		return part, "unsafe_root_evidence", nil
	default:
		return part, "", err
	}
}

func snapshotWithQueueSources(base extractionSnapshot, parts []queueSourceView) extractionSnapshot {
	// Foreground references precede all descendant observations, matching the
	// immutable reconstruction in verifySnapshotEvidenceTx.
	base.Sources, base.Roots = nil, nil
	base.View.References = []harness.SkillLearningReference{}
	for _, part := range parts {
		base.Sources = append(base.Sources, part.source)
		base.View.References = append(base.View.References, part.refs...)
	}
	for _, part := range parts {
		base.RootPolicy = part.policy
		base.Roots = append(base.Roots, part.roots...)
		base.View.References = append(base.View.References, part.rootRefs...)
	}
	return base
}

func (s *Store) queueSnapshotFits(snap extractionSnapshot) bool {
	if len(snap.Sources) > 8 || len(snap.View.References) > 528 || rootObservationCount(snap) > 8 {
		return false
	}
	view, err := json.Marshal(snap.View)
	if err != nil || len(view) > s.inputLimit() {
		return false
	}
	// Include the fixed-size eventual ID in the payload bound.
	snap.ID = extractionID(snap, s.extractor.Identity())
	payload, err := json.Marshal(snap)
	return err == nil && len(payload) <= s.inputLimit()+(32<<10)
}

func markQueueSourceTx(ctx context.Context, tx *sql.Tx, sequence int64, state, reason, job string) error {
	_, err := tx.ExecContext(ctx, `UPDATE harness_learning_outbox SET state=?,reason=?,job_id=? WHERE sequence=? AND state='pending'`, state, reason, job, sequence)
	return err
}

// Retain recent safe, already processed turns so later corrections keep their
// context. History is optional; an unavailable old root cannot block new work.
func (s *Store) queueHistoryTx(ctx context.Context, tx *sql.Tx, base extractionSnapshot, parts []queueSourceView) (extractionSnapshot, error) {
	snap := snapshotWithQueueSources(base, parts)
	if len(parts) == 8 {
		return snap, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+queueSourceColumns+` FROM harness_learning_outbox o JOIN harness_learning_inputs i ON i.run_id=o.run_id
 WHERE o.episode_id=? AND o.epoch=? AND o.sequence<? AND o.state='processed' ORDER BY o.sequence DESC LIMIT 8`, base.Episode, base.Epoch, parts[len(parts)-1].source.Outbox)
	if err != nil {
		return snap, err
	}
	history, err := readQueueSources(rows)
	if err != nil {
		return snap, err
	}
	for _, candidate := range history {
		part, reason, err := s.queueSourceTx(ctx, tx, base, candidate)
		if err != nil {
			return snap, err
		}
		if reason != "" {
			continue
		}
		withHistory := append(append([]queueSourceView{}, parts...), part)
		sort.Slice(withHistory, func(i, j int) bool { return withHistory[i].source.Outbox < withHistory[j].source.Outbox })
		proposed := snapshotWithQueueSources(base, withHistory)
		if !s.queueSnapshotFits(proposed) {
			continue
		}
		parts, snap = withHistory, proposed
		if len(parts) == 8 {
			break
		}
	}
	return snap, nil
}
