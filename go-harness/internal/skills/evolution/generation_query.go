package evolution

import (
	"cmp"
	"context"
	"slices"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

func (s *Service) GenerationStatus() harness.SkillGenerationStatus {
	result := harness.SkillGenerationStatus{Version: "skill-generation-v1", Sources: []harness.SkillTarget{}}
	if s.generation == nil {
		return result
	}
	result.Enabled = true
	policy := s.generation.policy
	result.Budget = &policy
	for _, target := range s.generation.allowed {
		result.Sources = append(result.Sources, target)
	}
	slices.SortFunc(result.Sources, func(a, b harness.SkillTarget) int { return cmp.Compare(a.SourceID, b.SourceID) })
	return result
}

func (s *Service) PrepareGeneration(ctx context.Context, target harness.SkillTarget) (harness.SkillGenerationPreparation, error) {
	var result harness.SkillGenerationPreparation
	if s.generation == nil {
		return result, invalid("generation is not configured")
	}
	err := s.registry.WithPublication(ctx, target.SourceID, target.Name, func(tx *skills.PublicationTx) error {
		if tx.Target() != target {
			return harness.ErrPermissionDenied
		}
		if err := s.generation.checkSource(generationRecord{input: generationSnapshot{Target: target}}); err != nil {
			return err
		}
		var err error
		result.Target = target
		result.Base, err = tx.Current()
		if err != nil {
			return err
		}
		result.OverlayBase, err = tx.OverlayBase()
		return err
	})
	return result, err
}

func (s *Service) QueryGeneration(ctx context.Context, q harness.SkillQuery) (harness.SkillGenerationPage, error) {
	page := harness.SkillGenerationPage{Jobs: []harness.SkillGenerationJob{}}
	if q.Status != "" {
		return page, invalid("generation queries do not accept proposal status")
	}
	allowed, where, args, cursor, err := s.querySources(q, "skill-generation-v1")
	if err != nil {
		return page, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	var revision int64
	if err = tx.QueryRowContext(ctx, `SELECT revision FROM df_skill_generation_catalog WHERE id=1`).Scan(&revision); err != nil {
		return page, err
	}
	if q.Cursor != "" && cursor.Revision != revision {
		return page, harness.ErrSkillConflict
	}
	cursor.Revision = revision
	args = append(args, cursor.Key, q.Limit+1)
	rows, err := tx.QueryContext(ctx, `SELECT job_id FROM df_skill_generation_inputs WHERE `+where+` AND job_id>? ORDER BY job_id LIMIT ?`, args...)
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
		if len(page.Jobs) == q.Limit {
			page.Next = encodeCursor(cursor)
			break
		}
		r, err := readGeneration(ctx, tx, id)
		if err != nil {
			return page, err
		}
		if !checkTarget(allowed, r.input.Target) {
			return page, harness.ErrPermissionDenied
		}
		page.Jobs = append(page.Jobs, generationView(r))
		cursor.Key = id
	}
	return page, tx.Commit()
}

func (s *Service) GenerationReceipts(ctx context.Context, id string, after int64, limit int) ([]harness.SkillMaintenanceReceipt, error) {
	var result []harness.SkillMaintenanceReceipt
	err := s.withGeneration(ctx, id, func(tx *skills.PublicationTx, _ generationRecord) error {
		var err error
		result, err = skillmaintenance.ReadReceipts(ctx, tx.SQL(), id, after, limit)
		return err
	})
	return result, err
}

func (s *Service) CancelGeneration(ctx context.Context, id string, version int64) error {
	return s.withGeneration(ctx, id, func(tx *skills.PublicationTx, r generationRecord) error {
		var p Proposal
		var err error
		if r.proposal != "" {
			p, err = readProposal(ctx, tx.SQL(), r.proposal)
			if err != nil {
				return err
			}
			if p.Status == "published" {
				return harness.ErrSkillConflict
			}
		}
		if err = skillmaintenance.CancelTx(ctx, tx.SQL(), id, version); err != nil {
			return err
		}
		if p.ID != "" && (p.Status == "validating" || p.Status == "pending_review") {
			r.job.State = "cancelled"
			return closeGenerationProposal(ctx, tx.SQL(), r, p)
		}
		return nil
	})
}
