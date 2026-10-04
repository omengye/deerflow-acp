package evolution

import "github.com/omengye/deerflow-acp/go-harness/harness"

var (
	ErrConflict   = harness.ErrSkillConflict
	ErrValidation = harness.ErrSkillValidation
)

type ValidationInput = harness.SkillValidationInput
type Publication = harness.SkillPublication
type Validator = harness.SkillValidator
type Proposal = harness.SkillProposal
type Request = harness.SkillProposalRequest
type Trigger = harness.SkillTrigger
type Report = harness.SkillValidationReport
type Review = harness.SkillReview
type Rollback = harness.SkillRollback
