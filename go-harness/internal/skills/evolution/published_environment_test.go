package evolution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

func TestPublishedEnvironmentChangeReevaluatesWithoutRepublishing(t *testing.T) {
	ctx := context.Background()
	f, g, v, evaluator := behaviorFixture(t)
	suite := behaviorSuite()
	regression := suite.Cases[0]
	regression.ID, regression.Kind, regression.Holdout = "regression", "regression", true
	suite.Cases = append(suite.Cases, regression)
	// Reopen to bind the full trusted suite before workers run.
	next, err := New(ctx, f.registry, v)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	configureGeneration(t, next, g)
	evaluator.result = func(in harness.SkillBehaviorInput) (bool, error) {
		return in.Variant != "base" || in.Case.Kind != "targeted", nil
	}
	configureBehavior(t, next, evaluator, suite)
	if err = next.ConfigureAutoPatch(*AutomaticSettings("primary").AutoPatchOptions()); err != nil {
		t.Fatal(err)
	}
	f.service = next
	job := queueGeneration(t, f, "published-environment")
	generationRun(t, next)
	generated, err := next.GetGeneration(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = next.RunAutoPatch(ctx); err != nil {
		t.Fatal(err)
	}
	o, err := readAutoPatchObservation(ctx, f.db.DB(), generated.ProposalID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.registry.WithPublication(ctx, o.Target.SourceID, o.Target.Name, func(pub *skills.PublicationTx) error {
		o.Status = "stable"
		return writeAutoPatchObservation(ctx, pub.SQL(), o, false)
	}); err != nil {
		t.Fatal(err)
	}
	head := f.head(t)
	restarted, err := New(ctx, f.registry, v)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	configureGeneration(t, restarted, g)
	changed := &fixtureBehaviorEvaluator{id: "environment-B", result: evaluator.result}
	configureBehavior(t, restarted, changed, suite)
	if err = restarted.ConfigureAutoPatch(*AutomaticSettings("primary").AutoPatchOptions()); err != nil {
		t.Fatal(err)
	}
	if err = restarted.RunAutoPatch(ctx); err != nil {
		t.Fatal(err)
	}
	o, err = readAutoPatchObservation(ctx, f.db.DB(), o.ProposalID)
	if err != nil || o.Status != "stable" || o.VerificationState != "rechecking" || f.head(t) != head {
		t.Fatal("stale environment misrepresented", o.VerificationState, err)
	}
	for range 2 {
		generationRun(t, restarted)
	}
	if err = restarted.RunAutoPatch(ctx); err != nil {
		t.Fatal(err)
	}
	o, err = readAutoPatchObservation(ctx, f.db.DB(), o.ProposalID)
	if err != nil || o.VerificationState != "current" || o.Status != "stable" || f.head(t) != head || changed.calls.Load() != 4 || g.calls.Load() != 1 {
		t.Fatal("reassessment did not preserve publication", o.VerificationState, changed.calls.Load(), g.calls.Load(), err)
	}
	if worked, err := restarted.RunGeneration(ctx); err != nil || worked {
		t.Fatal("unchanged environment recharged", worked, err)
	}
}

func TestAutomaticCandidateCannotBeLaunderedThroughPublicationEditOrImport(t *testing.T) {
	ctx := context.Background()
	f, _, _, _ := behaviorFixture(t)
	job := queueGeneration(t, f, "no-laundering")
	generationRun(t, f.service)
	generated, err := f.service.GetGeneration(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.service.Get(ctx, generated.ProposalID)
	if err != nil {
		t.Fatal(err)
	}
	var files map[string][]byte
	if err = f.registry.WithPublication(ctx, p.Target.SourceID, p.SkillName, func(pub *skills.PublicationTx) error {
		candidate, err := pub.ReadVersion(p.CandidateVersion)
		if err != nil {
			return err
		}
		files = candidate.Files()
		_, err = pub.Publish(p.CandidateVersion, p.Base, true)
		return err
	}); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("direct publication bypass", err)
	}
	if f.head(t) != p.Base {
		t.Fatal("failed bypass changed head")
	}
	content := string(files["SKILL.md"])
	_, err = f.registry.PrepareEdit(ctx, harness.SkillRef{SourceID: p.Target.SourceID, SourceIdentity: p.Target.SourceIdentity, Name: p.SkillName, Version: p.Base.Version, Hash: p.Base.Version}, "owner", "launder-edit", []harness.SkillFileEdit{{Path: "SKILL.md", Operation: "write", Content: &content}})
	if !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("edit publication bypass", err)
	}
	for _, query := range []string{`DELETE FROM df_skill_behavior_results WHERE evaluation_id IN (SELECT id FROM df_skill_behavior_evaluations WHERE proposal_id=?)`, `DELETE FROM df_skill_behavior_evaluations WHERE proposal_id=?`, `DELETE FROM df_skill_proposals WHERE id=?`} {
		if _, err = f.db.DB().Exec(query, p.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err = f.registry.WithPublication(ctx, p.Target.SourceID, p.SkillName, func(pub *skills.PublicationTx) error { _, e := pub.Publish(p.CandidateVersion, p.Base, true); return e }); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("erasing source metadata removed candidate provenance", err)
	}

	for _, source := range f.cfg.Sources {
		if source.ID == p.Target.SourceID {
			for path, content := range files {
				destination := filepath.Join(source.Root, p.SkillName, filepath.FromSlash(path))
				if err = os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(destination, content, 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	_, _ = f.registry.Install(ctx, p.Target.SourceID, p.SkillName)
	if f.head(t) != p.Base {
		t.Fatal("source import bypassed automatic adoption")
	}
}
