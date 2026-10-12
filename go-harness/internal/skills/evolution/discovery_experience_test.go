package evolution

import (
	"context"
	"errors"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSkillLearningExperienceDiscoveryNeedsHostAuthorityAndRemainsManualOnly(t *testing.T) {
	f, _, _ := generationFixture(t)
	opts := discoveryOptions()
	opts.ExperiencePatternsEnabled = true
	if err := f.service.ConfigureDiscovery(opts); err == nil {
		t.Fatal("experience discovery configured without host learning authority")
	}
	if err := f.service.ConfigureExperienceDiscoverySource(nil); err == nil {
		t.Fatal("nil learning source granted evidence authority")
	}
	ctx := context.WithValue(context.Background(), autoPatchContextKey{}, true)
	err := f.service.guardAutoPatch(ctx, nil, Proposal{Trigger: Trigger{Type: "experience_pattern"}}, ValidationInput{})
	if !errors.Is(err, errAutoPatchReview) {
		t.Fatal("learning proposal admitted automatic publication", err)
	}
}

func TestSkillLearningExperienceNewSkillDiscoveryNeedsIndependentGrant(t *testing.T) {
	for _, mode := range []string{"missing-experience-gate", "missing-source", "unauthorized-source", "no-learning-authority"} {
		t.Run(mode, func(t *testing.T) {
			f, _, _ := generationFixture(t)
			o := discoveryOptions()
			o.ExperienceNewSkillsEnabled, o.ExperiencePatternsEnabled, o.NewSkillSourceID = true, true, "project"
			switch mode {
			case "missing-experience-gate":
				o.ExperiencePatternsEnabled = false
			case "missing-source":
				o.NewSkillSourceID = ""
			case "unauthorized-source":
				o.NewSkillSourceID = "foreign"
			}
			if err := f.service.ConfigureDiscovery(o); err == nil {
				t.Fatal("new capability creation implicitly authorized", mode)
			}
		})
	}
	d := discoverySnapshot{Kind: "experience_pattern", Options: discoveryOptions(), Target: harness.SkillTarget{SourceID: "project", Name: "structured-json-audit"}, EpisodeDedup: true, Evidence: make([]harness.SkillDiscoveryEvidence, 2)}
	d.Options.ExperiencePatternsEnabled = true
	if validDiscoverySnapshot(d) {
		t.Fatal("experience editing implicitly authorized absent head")
	}
	d.Options.ExperienceNewSkillsEnabled, d.Options.NewSkillSourceID = true, "project"
	if !validDiscoverySnapshot(d) {
		t.Fatal("explicit new capability target rejected")
	}
	d.Base.StateVersion = 1
	if validDiscoverySnapshot(d) {
		t.Fatal("tombstone treated as a fresh capability name")
	}
}

func TestSkillLearningExperienceDiscoveryExistingTriggerUsesUnifiedConsumption(t *testing.T) {
	f, _, _ := discoveryFixture(t)
	ctx := context.Background()
	first := discoveryCorrection(t, f, "", "first")
	second := discoveryCorrection(t, f, "", "second")
	signal, err := f.service.Feedback(ctx, first.SignalID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.db.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	key, err := discoveryEpisodeTx(ctx, tx, signal.Signal.RootID)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	discoverySteps(t, f.service, 2)
	ds := discoveries(t, f.service)
	if len(ds) != 1 || len(ds[0].Evidence) != 2 || ds[0].Evidence[0].EpisodeKey == ds[0].Evidence[1].EpisodeKey || first.SignalID == second.SignalID {
		t.Fatal(ds)
	}
	tx, err = f.db.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	again, err := discoveryEpisodeTx(ctx, tx, signal.Signal.RootID)
	if err != nil || again != key {
		t.Fatal("pinned independence lost", again, key, err)
	}
	for _, ref := range ds[0].Evidence {
		consumed, err := discoveryEpisodeConsumedTx(ctx, tx, ref.RootID, ref.EpisodeKey, ds[0].Target, ds[0].Base.Version)
		if err != nil || !consumed {
			t.Fatal("operator discovery missing unified consumption", consumed, err)
		}
	}
}

func TestSkillLearningReviewDraftSnapshotRequiresOptInAndCannotClaimFullThreshold(t *testing.T) {
	d := discoverySnapshot{Kind: "experience_pattern", Options: discoveryOptions(),
		Base:         harness.SkillHead{Exists: true, Enabled: true, Version: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		EpisodeDedup: true, ReviewDraft: true, Evidence: make([]harness.SkillDiscoveryEvidence, 1)}
	d.Options.ExperiencePatternsEnabled = true
	if validDiscoverySnapshot(d) {
		t.Fatal("review draft enabled without its host gate")
	}
	d.Options.ExperienceReviewDraftsEnabled = true
	if !validDiscoverySnapshot(d) {
		t.Fatal("one-episode review draft rejected")
	}
	d.ReviewDraft = false
	if validDiscoverySnapshot(d) {
		t.Fatal("single episode claimed full automatic evidence")
	}
	d.ReviewDraft = true
	d.Evidence = append(d.Evidence, harness.SkillDiscoveryEvidence{})
	if validDiscoverySnapshot(d) {
		t.Fatal("review draft mislabeled full-threshold evidence")
	}
}
