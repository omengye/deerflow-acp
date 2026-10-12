package evolution

import (
	"context"
	"errors"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

func TestSkillLearningCredentialGuardRequiresHostDiscovery(t *testing.T) {
	ctx := context.Background()
	f, g, v := generationFixture(t)
	files := skillFiles(`Read full input; api_key="$API_KEY"`)
	candidate, err := f.registry.PrepareCandidate("research", files)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCandidateCredentials(candidate, true); err != nil {
		t.Fatal("host-scoped learning rejected a generic placeholder", err)
	}
	files["references/notes.md"] = []byte(`api_key="$API_KEY"`)
	support, err := f.registry.PrepareCandidate("research", files)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCandidateCredentials(support, true); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatal("learning placeholder exception extended to support files", err)
	}
	delete(files, "references/notes.md")
	for _, origin := range []string{"manual_agent", "automatic"} {
		request := f.request(t, origin, "Unused fixture")
		request.Files, request.Origin = files, origin
		request.Trigger.Type = "experience_pattern"
		if _, err := f.service.Submit(ctx, request); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatal("caller trigger/automatic origin obtained learning exception", origin, err)
		}
	}
	g.body = func(context.Context, skillmaintenance.GenerationInput) (skillmaintenance.Candidate, error) {
		return skillmaintenance.Candidate{Reason: "Use environment-based authentication", Files: files}, nil
	}
	job := queueGeneration(t, f, "operator-placeholder")
	generationRun(t, f.service)
	failed, err := f.service.GetGeneration(ctx, job.ID)
	if err != nil || failed.State != "failed" || failed.ErrorCode != "invalid_candidate" || failed.CandidateVersion != "" || failed.ProposalID != "" || v.calls.Load() != 0 {
		t.Fatal("operator generation bypassed its existing credential rule", failed, err)
	}
	var retained int
	if err := f.db.DB().QueryRow(`SELECT count(*) FROM df_skill_versions`).Scan(&retained); err != nil || retained != 1 {
		t.Fatal("unscoped output retained candidate data", retained, err)
	}
}
