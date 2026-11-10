package evolution

import (
	"context"
	"database/sql"
	"errors"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
	"path/filepath"
)

func (s *Service) automaticWorkspace(ctx context.Context, tx *sql.Tx, p Proposal) (string, error) {
	var job string
	if err := tx.QueryRowContext(ctx, `SELECT job_id FROM df_skill_generation_inputs WHERE proposal_id=?`, p.ID).Scan(&job); err != nil {
		return "", err
	}
	r, err := readGeneration(ctx, tx, job)
	if err != nil {
		return "", err
	}
	if r.input.DiscoveryID == "" {
		if r.input.Workspace != "" {
			if !filepath.IsAbs(r.input.Workspace) || p.Target.Scope == harness.SkillScopeWorkspace && !session.SameWorkspace(r.input.Workspace, p.Target.Workspace) {
				return "", harness.ErrPermissionDenied
			}
			return r.input.Workspace, nil
		}
		if p.Target.Scope == harness.SkillScopeWorkspace && filepath.IsAbs(p.Target.Workspace) {
			return p.Target.Workspace, nil
		}
		return "", harness.ErrPermissionDenied
	}
	d, _, _, err := readDiscovery(ctx, tx, r.input.DiscoveryID)
	if err != nil {
		return "", err
	}
	workspace := ""
	for _, e := range d.Evidence {
		var cwd string
		err = tx.QueryRowContext(ctx, `SELECT s.cwd FROM budget_roots b JOIN harness_sessions s ON s.id=b.session_id WHERE b.id=?`, e.RootID).Scan(&cwd)
		if errors.Is(err, sql.ErrNoRows) || !filepath.IsAbs(cwd) {
			return "", harness.ErrPermissionDenied
		}
		if err != nil {
			return "", err
		}
		if workspace != "" && !session.SameWorkspace(workspace, cwd) {
			return "", harness.ErrPermissionDenied
		}
		workspace = cwd
	}
	if workspace == "" {
		return "", harness.ErrPermissionDenied
	}
	if p.Target.Scope == harness.SkillScopeWorkspace && !session.SameWorkspace(workspace, p.Target.Workspace) {
		return "", harness.ErrPermissionDenied
	}
	return workspace, nil
}
