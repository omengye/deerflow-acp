package evolution

import (
	"context"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func (s *Service) AutoPatchStatus(ctx context.Context) (harness.SkillAutoPatchStatus, error) {
	now := time.Now().UTC()
	r := harness.SkillAutoPatchStatus{Today: now.Format("2006-01-02")}
	if a := s.autoPatch; a != nil {
		r.Enabled = true
		options := a.options
		r.Options = &options
		a.lifecycle.mu.Lock()
		r.Running = a.lifecycle.done != nil && !a.lifecycle.closed
		a.lifecycle.mu.Unlock()
	} else {
		s.observationMonitor.mu.Lock()
		r.Running = s.observationMonitor.done != nil && !s.observationMonitor.closed
		s.observationMonitor.mu.Unlock()
	}
	_, where, args, _, err := s.querySources(harness.SkillQuery{Limit: 1}, "auto-patch-status")
	if err != nil {
		return r, err
	}
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	parameters := append(append([]any{}, args...), start.UnixNano(), start.Add(24*time.Hour).UnixNano())
	err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM df_skill_auto_patch_observations WHERE `+where+` AND started_ns>=? AND started_ns<?`, parameters...).Scan(&r.PublishedToday)
	if err != nil {
		return r, err
	}
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM df_skill_auto_patch_observations WHERE `+where+` AND status='observing'`, args...).Scan(&r.Observing); err != nil {
		return r, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT last_error_code FROM df_skill_auto_patch_scan WHERE id=1`).Scan(&r.LastErrorCode)
	if err == nil {
		r.Readiness, err = s.autoPatchReadiness(ctx)
	}
	return r, err
}

func (s *Service) QueryAutoPatch(ctx context.Context, q harness.SkillQuery) (harness.SkillAutoPatchPage, error) {
	page := harness.SkillAutoPatchPage{Observations: []harness.SkillAutoPatchObservation{}}
	if q.Status != "" {
		return page, invalid("auto_patch observations do not accept a status filter")
	}
	allowed, where, args, cursor, err := s.querySources(q, "auto-patch-observations-v1")
	if err != nil {
		return page, err
	}
	args = append(args, cursor.Key, q.Limit+1)
	rows, err := s.db.QueryContext(ctx, `SELECT proposal_id FROM df_skill_auto_patch_observations WHERE `+where+` AND proposal_id>? ORDER BY proposal_id LIMIT ?`, args...)
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
		if len(page.Observations) == q.Limit {
			page.Next = encodeCursor(cursor)
			break
		}
		o, err := readAutoPatchObservation(ctx, s.db, id)
		if err != nil {
			return page, err
		}
		if !checkTarget(allowed, o.Target) {
			return page, harness.ErrPermissionDenied
		}
		page.Observations = append(page.Observations, o)
		cursor.Key = id
	}
	return page, nil
}
