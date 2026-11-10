package evolution

import (
	"context"
	"database/sql"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"strings"
)

func (s *Service) observationCoverageTx(ctx context.Context, tx *sql.Tx, o harness.SkillAutoPatchObservation) (bool, error) {
	if o.PolicyVersion != "skill-evolution-v2" {
		return false, nil
	}
	r, err := readBehavior(ctx, tx, o.ProposalID)
	if err != nil {
		return false, err
	}
	if r.input.Suite == nil {
		return false, nil
	}
	allCovered := true
	err = walkBehaviorCases(ctx, tx, r.input, func(c harness.SkillBehaviorCase) error {
		if c.Kind != "targeted" && c.Kind != "regression" {
			return nil
		}
		covered, exact := true, 0
		for _, a := range c.Assertions {
			if a.Kind != "output_equals" && a.Kind != "file_equals" {
				continue
			}
			exact++
			var assertionCovered bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_auto_patch_evidence e JOIN df_skill_task_oracles v ON v.root_id=e.root_id LEFT JOIN df_skill_task_oracle_kinds k ON k.root_id=v.root_id WHERE e.proposal_id=? AND e.outcome='success' AND v.passed=1 AND v.expected=? AND trim(v.prompt)=? AND COALESCE(k.kind,'output_equals')=? AND COALESCE(k.path,'')=?)`, o.ProposalID, a.Expected, strings.TrimSpace(c.Prompt), a.Kind, a.Path).Scan(&assertionCovered); err != nil {
				return err
			}
			covered = covered && assertionCovered
		}
		allCovered = allCovered && exact > 0 && covered
		return nil
	})
	return allCovered, err
}
