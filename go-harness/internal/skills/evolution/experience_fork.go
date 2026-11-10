package evolution

import (
	"context"
	"database/sql"
	"errors"
	"slices"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

type experienceFork struct {
	Target harness.SkillTarget `json:"target"`
	Head   harness.SkillHead   `json:"head"`
}

// A shared or read-only skill may be a valid baseline without being a writable
// target in this workspace. Its improvement gets an independent managed name.
func (s *Service) experienceTarget(ctx context.Context, m harness.SkillExperienceMatch) (harness.SkillTarget, *experienceFork, error) {
	o := s.discovery.options
	if m.Relation == "new_capability" {
		t, err := s.experienceSource.NewPromotionTarget(ctx, m.SessionID, o.DestinationSourceID, m.SuggestedName)
		return t, nil, err
	}
	if m.Target == nil || m.Head == nil || !m.Head.Exists || !m.Head.Enabled {
		return harness.SkillTarget{}, nil, errDiscoveryEvidence
	}
	target := *m.Target
	allowed, writable := s.generation.allowed[target.SourceID]
	allowed.Name = target.Name
	writable = writable && allowed == target && m.OverlayBase == nil
	builtin, err := s.registry.BuiltinSources()
	if err != nil {
		return harness.SkillTarget{}, nil, err
	}
	if slices.Contains(builtin, target.SourceID) {
		writable = false
	}
	var workspace string
	if err := s.db.QueryRowContext(ctx, `SELECT cwd FROM harness_sessions WHERE id=?`, m.SessionID).Scan(&workspace); err != nil {
		return harness.SkillTarget{}, nil, err
	}
	var scope string
	err = s.db.QueryRowContext(ctx, `SELECT workspace FROM df_skill_routes WHERE source_key=? AND name=?`, target.SourceIdentity, target.Name).Scan(&scope)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return harness.SkillTarget{}, nil, err
	}
	if err == nil && !session.SameWorkspace(scope, workspace) {
		writable = false
	}
	if writable {
		return target, nil, nil
	}
	name := skills.WorkspaceSkillName(target.Name+"-extension", workspace)
	target, err = s.experienceSource.NewPromotionTarget(ctx, m.SessionID, o.DestinationSourceID, name)
	if err != nil {
		return harness.SkillTarget{}, nil, err
	}
	return target, &experienceFork{Target: *m.Target, Head: *m.Head}, nil
}

func validateExperienceFork(ctx context.Context, tx *sql.Tx, sessionID string, d discoverySnapshot, original harness.SkillTarget, head harness.SkillHead) error {
	if d.Fork == nil || d.Fork.Target != original || d.Fork.Head != head || d.Base != (harness.SkillHead{}) || d.OverlayBase != nil || d.Target.SourceID != d.Options.DestinationSourceID {
		return errDiscoveryEvidence
	}
	var workspace string
	if err := tx.QueryRowContext(ctx, `SELECT cwd FROM harness_sessions WHERE id=?`, sessionID).Scan(&workspace); err != nil {
		return err
	}
	if d.Target.Name != skills.WorkspaceSkillName(original.Name+"-extension", workspace) {
		return errDiscoveryEvidence
	}
	return nil
}
