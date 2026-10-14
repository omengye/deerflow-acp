package evolution

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func (s *Service) DiscoveryStatus(ctx context.Context) (harness.SkillDiscoveryStatus, error) {
	result := harness.SkillDiscoveryStatus{Version: "skill-discovery-v1", Triggers: []string{"operator_corrections", "missing_skill", "repeated_workflow", "experience_pattern"}, Sources: []harness.SkillTarget{}, Today: time.Now().UTC().Format("2006-01-02")}
	if d := s.discovery; d != nil {
		result.Enabled = true
		options := d.options
		result.Options = &options
		result.Sources = s.GenerationStatus().Sources
		result.Today = d.now().UTC().Format("2006-01-02")
		d.lifecycle.mu.Lock()
		result.Running = d.lifecycle.done != nil && !d.lifecycle.closed
		d.lifecycle.mu.Unlock()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, `SELECT reservations FROM df_skill_discovery_days WHERE day=?`, result.Today).Scan(&result.DailyReservations)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	result.Pending, err = discoveryPending(ctx, tx)
	if err != nil {
		return result, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM df_skill_discovery_errors e JOIN df_skill_signals s ON s.id=e.signal_id WHERE attempts>=3 AND error_code='invalid_evidence' AND signal_version=s.state_version AND feedback_revision=COALESCE((SELECT MAX(revision) FROM df_skill_feedback WHERE signal_id=s.id),0)`).Scan(&result.FailedSignals); err != nil {
		return result, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT last_error_code FROM df_skill_discovery_health WHERE id=1`).Scan(&result.LastErrorCode); err != nil {
		return result, err
	}
	return result, tx.Commit()
}

// Discovery inventory uses live keyset pagination. Its immutable entries bind
// authorization/filter and last ID; mutable generation state is read afresh.
func (s *Service) QueryDiscoveries(ctx context.Context, q harness.SkillQuery) (harness.SkillDiscoveryPage, error) {
	page := harness.SkillDiscoveryPage{Discoveries: []harness.SkillDiscovery{}}
	if q.Status != "" {
		return page, invalid("discovery queries do not accept status")
	}
	allowed, where, args, cursor, err := s.querySources(q, "skill-discovery-v1")
	if err != nil {
		return page, err
	}
	if q.Cursor != "" && (encodeCursor(cursor) != q.Cursor || cursor.Revision != 0 || cursor.Name != "" || !strings.HasPrefix(cursor.Key, "sd_") || len(cursor.Key) > 128) {
		return page, invalid("discovery cursor is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	if q.Cursor != "" {
		d, _, _, err := readDiscovery(ctx, tx, cursor.Key)
		if err != nil {
			return page, err
		}
		if !checkTarget(allowed, d.Target) || q.Name != "" && d.Target.Name != q.Name {
			return page, harness.ErrPermissionDenied
		}
	}
	args = append(args, cursor.Key, q.Limit+1)
	rows, err := tx.QueryContext(ctx, `SELECT id FROM df_skill_discoveries WHERE `+where+` AND id>? ORDER BY id LIMIT ?`, args...)
	if err != nil {
		return page, err
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
		return page, err
	}
	for _, id := range ids {
		if len(page.Discoveries) == q.Limit {
			page.Next = encodeCursor(cursor)
			break
		}
		d, job, digest, err := readDiscovery(ctx, tx, id)
		if err != nil {
			return page, err
		}
		if !checkTarget(allowed, d.Target) {
			return page, harness.ErrPermissionDenied
		}
		r, err := readGeneration(ctx, tx, job)
		if err != nil {
			return page, err
		}
		if r.input.DiscoveryID != id || r.input.DiscoveryDigest != digest || r.input.Target != d.Target || r.input.Request.Base != d.Base {
			return page, invalid("discovery generation binding is inconsistent")
		}
		var previous string
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT previous_proposal_id FROM df_skill_discovery_successors WHERE discovery_id=?),'')`, id).Scan(&previous); err != nil {
			return page, err
		}
		page.Discoveries = append(page.Discoveries, harness.SkillDiscovery{ID: id, Target: d.Target, Base: d.Base, GenerationID: job, CreatedAt: d.CreatedAt, Attribution: "unassessed", Kind: d.kind(), Evidence: d.Evidence, State: r.job.State, ProposalID: r.proposal, HostAdjudicated: d.HostAdjudicated, ReviewDraft: d.ReviewDraft, CreationRetry: d.CreationRetry, PreviousProposalID: previous})
		cursor.Key = id
	}
	return page, tx.Commit()
}
