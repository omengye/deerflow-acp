package evolution

import (
	"context"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/learning"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

var errExperienceCredential = invalid("experience candidate contains a credential-like literal")

func validateExperienceInstructions(data []byte) error {
	if err := learning.ValidatePromotionInstructions(string(data)); err != nil {
		return errExperienceCredential
	}
	return nil
}

func validateCandidateCredentials(candidate *skills.Candidate, experience bool) error {
	if experience {
		if err := validateExperienceInstructions(candidate.Files()["SKILL.md"]); err != nil {
			return err
		}
	}
	for _, finding := range candidate.Findings() {
		if finding.Code == "possible_credential" && (!experience || finding.Path != "SKILL.md") {
			return invalid("candidate contains a credential-like literal")
		}
	}
	return nil
}

func (s *Service) guardSubmittedCredentials(ctx context.Context, tx *skills.PublicationTx, candidate *skills.Candidate) error {
	experience := false
	if id, _ := ctx.Value(generationContextKey{}).(string); id != "" {
		if err := skillmaintenance.GuardTx(ctx, tx.SQL(), id); err != nil {
			return err
		}
		r, err := readGeneration(ctx, tx.SQL(), id)
		if err != nil {
			return err
		}
		if r.input.Target != tx.Target() || r.candidate != candidate.Version() {
			return harness.ErrSkillConflict
		}
		if r.input.DiscoveryID != "" {
			d, _, _, err := readDiscovery(ctx, tx.SQL(), r.input.DiscoveryID)
			if err != nil {
				return err
			}
			experience = d.kind() == "experience_pattern"
		}
	}
	return validateCandidateCredentials(candidate, experience)
}

// Saved candidates from an older host get the same check as fresh output. The
// durable discovery kind, not a caller's trigger label, identifies learning.
func (s *Service) guardExperienceCandidate(ctx context.Context, tx *skills.PublicationTx, r generationRecord) error {
	if r.input.DiscoveryID == "" || r.candidate == "" {
		return nil
	}
	d, _, _, err := readDiscovery(ctx, tx.SQL(), r.input.DiscoveryID)
	if err != nil || d.kind() != "experience_pattern" {
		return err
	}
	candidate, err := tx.ReadVersion(r.candidate)
	if err != nil {
		return err
	}
	return validateCandidateCredentials(candidate, true)
}
