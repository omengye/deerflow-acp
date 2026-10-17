package skills

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// FilePublicationStatus distinguishes durable approval history from actual
// filesystem completion. Old histories have no new-protocol completion row.
func FilePublicationStatus(ctx context.Context, q fileQuery, source, name string, state int64) (string, error) {
	var status string
	err := q.QueryRowContext(ctx, `SELECT status FROM df_skill_file_publication_results WHERE source_key=? AND name=? AND state_version=?`, source, name, state).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "legacy", nil
	}
	return status, err
}

func (r *Registry) HostFilePublications(ctx context.Context, target harness.SkillTarget) ([]harness.SkillFilePublication, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, ErrClosed
	}
	s, err := r.catalogTarget(target)
	if err != nil {
		return nil, err
	}
	out := []harness.SkillFilePublication{}
	err = r.withPublication(ctx, s, target.Name, func(p *PublicationTx) error {
		rec, e := readFilePublication(ctx, p.tx, s.identity, target.Name)
		if errors.Is(e, sql.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		head, e := p.Current()
		if e != nil {
			return e
		}
		out = append(out, harness.SkillFilePublication{ID: rec.ID, Target: target, SourceDirectory: filepath.Join(s.config.Root, filepath.FromSlash(rec.Directory)), BackupRelative: ".deerflow-publication-backups/" + rec.ID, Base: rec.Base, Next: rec.Next, Current: head, ProposalID: rec.ProposalID, Status: "needs_reconciliation"})
		return nil
	})
	return out, err
}

// Abort never restores history automatically. The operator must first restore
// the exact base directory (or absence for a create). Resume uses approved bytes.
func (r *Registry) ReconcilePublication(ctx context.Context, target harness.SkillTarget, id string, expected harness.SkillHead, action, actor string) error {
	if !validEditID(id) || !validManagementText(actor, 256) || action != "resume" && action != "abort" {
		return invalid("publication reconciliation requires id, actor and resume or abort")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	s, err := r.catalogTarget(target)
	if err != nil {
		return err
	}
	return r.withPublication(ctx, s, target.Name, func(p *PublicationTx) error {
		rec, err := readFilePublication(ctx, p.tx, s.identity, target.Name)
		if err != nil {
			return err
		}
		head, err := p.Current()
		if err != nil {
			return err
		}
		if rec.ID != id || head != expected || head != rec.Base {
			return harness.ErrSkillConflict
		}
		if action == "resume" {
			return p.ResumeFilePublication()
		}
		if err = validateSourceRoot(s); err != nil {
			return err
		}
		if rec.Base.Exists {
			pkg, e := r.readPackage(ctx, s, rec.Directory)
			if e != nil {
				return e
			}
			if (&Candidate{pkg: pkg}).Version() != rec.Base.Version {
				return harness.ErrSkillConflict
			}
		} else if _, e := s.root.Lstat(rec.Directory); !errors.Is(e, os.ErrNotExist) {
			return harness.ErrSkillConflict
		}
		if rec.ProposalID != "" {
			if _, err = p.tx.ExecContext(ctx, `UPDATE df_skill_proposals SET state_version=state_version+1,status='failed',payload=json_set(CAST(payload AS TEXT),'$.state_version',state_version+1,'$.status','failed','$.file_publication_pending',json('false'),'$.error','File publication aborted by host after verifying the original directory.') WHERE id=? AND status='publishing'`, rec.ProposalID); err != nil {
				return err
			}
		}
		if _, err = p.tx.ExecContext(ctx, `UPDATE df_skill_file_publication_results SET status='aborted',actor=? WHERE source_key=? AND name=? AND state_version=?`, actor, s.identity, target.Name, rec.Next.StateVersion); err != nil {
			return err
		}
		if _, err = p.tx.ExecContext(ctx, `DELETE FROM df_skill_file_publications WHERE source_key=? AND name=?`, s.identity, target.Name); err != nil {
			return err
		}
		if _, err = p.tx.ExecContext(ctx, `DELETE FROM df_skill_exports WHERE source_key=? AND name=? AND state_version=?`, s.identity, target.Name, rec.Next.StateVersion); err != nil {
			return err
		}
		// Consume the admitted state version so an aborted audit cannot later
		// be confused with an unrelated publication reusing the same number.
		if _, err = p.moveHead(head, head.Version, head.Enabled, head.Managed, head.Version, rec.Directory); err != nil {
			return err
		}
		return r.refreshSource(p, rec.Directory, nil)
	})
}
