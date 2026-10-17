package skills

import (
	"context"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// HostEdits exposes bounded, redacted journal evidence for one fully identified
// installation. It is an operator API, never granted to model tools.
func (r *Registry) HostEdits(ctx context.Context, target harness.SkillTarget) ([]harness.SkillEditResult, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, ErrClosed
	}
	s, err := r.catalogTarget(target)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM df_skill_edits WHERE source_key=? AND name=? ORDER BY rowid DESC LIMIT 256`, target.SourceIdentity, target.Name)
	if err != nil {
		return nil, err
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
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	out := make([]harness.SkillEditResult, 0, len(ids))
	for _, id := range ids {
		rec, e := readEdit(ctx, r.db, id)
		if e != nil {
			return nil, e
		}
		result, e := r.editResult(ctx, s, rec)
		if e != nil {
			return nil, e
		}
		out = append(out, result)
	}
	return out, nil
}

// ReconcileEdit retries only the originally approved bytes or closes an edit
// after the operator has restored the exact original package. It never adopts
// external bytes or overwrites them to force a rollback.
func (r *Registry) ReconcileEdit(ctx context.Context, target harness.SkillTarget, id string, expected harness.SkillHead, action, actor string) (out harness.SkillEditResult, err error) {
	if !validManagementText(actor, 256) || action != "resume" && action != "abort" {
		return out, invalid("reconciliation requires an actor and resume or abort")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return out, ErrClosed
	}
	s, err := r.catalogTarget(target)
	if err != nil {
		return out, err
	}
	rec, err := readEdit(ctx, r.db, id)
	if err != nil {
		return out, err
	}
	if rec.Review.Target != target {
		return out, harness.ErrPermissionDenied
	}
	err = r.withEditPublication(ctx, s, target.Name, id, func(p *PublicationTx) error {
		rec, err = readEdit(ctx, p.tx, id)
		if err != nil {
			return err
		}
		head, e := p.Current()
		if e != nil {
			return e
		}
		if head != expected {
			return harness.ErrSkillConflict
		}
		if action == "abort" && rec.Status == "aborted" || action == "resume" && rec.Status == "applied" {
			return nil
		}
		if rec.Status != "needs_reconciliation" || head != rec.Review.Base {
			return harness.ErrSkillConflict
		}
		if pending, e := editPending(ctx, p.tx, s.identity, target.Name, id); e != nil {
			return e
		} else if pending {
			return harness.ErrReconciliationRequired
		}
		if action == "abort" {
			if rec.Review.Storage == "source" {
				pkg, e := r.readPackage(ctx, s, rec.Dir)
				if e != nil {
					return e
				}
				if (&Candidate{pkg: pkg}).Version() != head.Version {
					return harness.ErrSkillConflict
				}
			}
			rec.Status = "aborted"
		} else {
			rec.Status = "applying"
		}
		rec.Error = ""
		rec.ReconciledBy = actor
		return saveEdit(ctx, p.tx, rec)
	})
	if err != nil {
		return out, err
	}
	if rec.Status == "applying" {
		if err = r.finishEdit(ctx, s, &rec); err != nil {
			return out, err
		}
	}
	return r.editResult(ctx, s, rec)
}
