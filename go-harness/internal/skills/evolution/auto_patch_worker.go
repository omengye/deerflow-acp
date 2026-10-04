package evolution

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func (s *Service) autoPatchBatch(ctx context.Context, monitor bool) error {
	column := "publish_cursor"
	if monitor {
		column = "monitor_cursor"
	}
	var cursor string
	if err := s.db.QueryRowContext(ctx, `SELECT `+column+` FROM df_skill_auto_patch_scan WHERE id=1`).Scan(&cursor); err != nil {
		return err
	}
	query := `SELECT p.id FROM df_skill_proposals p JOIN df_skill_generation_inputs g ON g.proposal_id=p.id JOIN df_skill_maintenance_jobs j ON j.id=g.job_id WHERE p.id>? AND p.status='pending_review' AND j.state='completed' ORDER BY p.id LIMIT 16`
	if monitor {
		query = `SELECT proposal_id FROM df_skill_auto_patch_observations WHERE proposal_id>? AND status='observing' ORDER BY proposal_id LIMIT 16`
	}
	rows, err := s.db.QueryContext(ctx, query, cursor)
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
	var combined error
	for _, id := range ids {
		if monitor {
			err = s.monitorAutoPatch(ctx, id)
		} else {
			var p Proposal
			p, err = s.Get(ctx, id)
			if err == nil {
				_, err = s.Approve(context.WithValue(ctx, autoPatchContextKey{}, true), id, Review{ExpectedStateVersion: p.StateVersion, Actor: "auto_patch", Note: "Automatically published a bounded patch after content and behavioral evaluation."})
			}
		}
		if err != nil && !errors.Is(err, errAutoPatchReview) && !errors.Is(err, harness.ErrSkillConflict) && !errors.Is(err, harness.ErrPermissionDenied) && !errors.Is(err, harness.ErrNotFound) {
			combined = errors.Join(combined, err)
		}
		if ctx.Err() != nil {
			return errors.Join(combined, ctx.Err())
		}
		if _, e := s.db.ExecContext(ctx, `UPDATE df_skill_auto_patch_scan SET `+column+`=? WHERE id=1`, id); e != nil {
			return errors.Join(combined, e)
		}
	}
	err = nil
	if len(ids) < 16 {
		_, err = s.db.ExecContext(ctx, `UPDATE df_skill_auto_patch_scan SET `+column+`='' WHERE id=1`)
	}
	return errors.Join(combined, err)
}

func (s *Service) RunAutoPatch(ctx context.Context) error {
	if s.autoPatch == nil {
		return nil
	}
	err := s.autoPatchBatch(ctx, true)
	if ctx.Err() == nil {
		err = errors.Join(err, s.autoPatchBatch(ctx, false))
	}
	code := ""
	if err != nil {
		code = "maintenance_error"
	}
	_, saveErr := s.db.ExecContext(ctx, `UPDATE df_skill_auto_patch_scan SET last_error_code=? WHERE id=1`, code)
	return errors.Join(err, saveErr)
}

func (s *Service) StartAutoPatch() error {
	a := s.autoPatch
	if a == nil {
		return nil
	}
	a.lifecycle.mu.Lock()
	defer a.lifecycle.mu.Unlock()
	if a.lifecycle.closed {
		return invalid("auto_patch worker is closed")
	}
	if a.lifecycle.done != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.lifecycle.cancel = cancel
	a.lifecycle.done = make(chan struct{})
	go func() {
		defer close(a.lifecycle.done)
		warned := false
		for {
			step, stop := context.WithTimeout(ctx, 5*time.Second)
			err := s.RunAutoPatch(step)
			stop()
			if ctx.Err() != nil {
				return
			}
			if err != nil && !warned {
				slog.Warn("skill automatic publication maintenance failed")
			}
			warned = err != nil
			timer := time.NewTimer(2 * time.Second)
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

func (s *Service) closeAutoPatch() {
	a := s.autoPatch
	if a == nil {
		return
	}
	a.lifecycle.mu.Lock()
	a.lifecycle.closed = true
	if a.lifecycle.cancel != nil {
		a.lifecycle.cancel()
	}
	done := a.lifecycle.done
	a.lifecycle.mu.Unlock()
	if done != nil {
		<-done
	}
}
