package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills/evolution"
)

func enableNewExperienceDiscovery(t *testing.T, e *evolution.Service) {
	t.Helper()
	if err := e.ConfigureDiscovery(harness.SkillDiscoveryOptions{DisableOperatorCorrections: true, ExperiencePatternsEnabled: true, ExperienceNewSkillsEnabled: true, NewSkillSourceID: "project", RepeatThreshold: 2, RepeatWindowDays: 30, CooldownHours: 24, MaxDailyProposals: 5, MaxPendingProposals: 20}); err != nil {
		t.Fatal(err)
	}
}

func TestSkillLearningExperienceNewSkillReadableTargetManualOnlyAndRestart(t *testing.T) {
	s, x, r, e, g, v, matcher := experienceDiscoveryTest(t)
	ctx := context.Background()
	matcher.relation, matcher.suggestedName = "new_capability", "structured-json-audit"
	consolidateExtract(t, s, x, "One complete structured configuration check")
	consolidateRun(t, s)
	experienceMatch(t, s)
	enableNewExperienceDiscovery(t, e)
	if ds := experienceDiscoveries(t, e); len(ds) != 0 || len(g.seen) != 0 {
		t.Fatal("one episode authorized new skill generation", ds)
	}
	if _, err := s.StartSkillLearningEpisode(ctx, "owner", x.ID); err != nil {
		t.Fatal(err)
	}
	consolidateExtract(t, s, x, "Independent complete structured configuration check")
	consolidateRun(t, s)
	id := experienceMatch(t, s)
	ds := experienceDiscoveries(t, e)
	if len(ds) != 1 || ds[0].Target.SourceID != "project" || ds[0].Target.Name != matcher.suggestedName || ds[0].Base != (harness.SkillHead{}) || ds[0].Evidence[0].Experience.MatchID != id {
		t.Fatal("host did not fix absent named target", ds)
	}
	p := experienceGenerate(t, e)
	if p.Action != "create" || p.Status != "pending_review" || p.SkillName != matcher.suggestedName || len(p.ChangedFiles) != 1 || p.ChangedFiles[0] != "SKILL.md" || p.Evaluation.Behavior != "not_run" || len(g.seen) != 1 || !g.seen[0].InstructionOnly || len(g.seen[0].BaseFiles) != 0 {
		t.Fatal("new capability was not a one-file manual candidate", p, g.seen)
	}
	if err := r.WithPublication(ctx, "project", p.SkillName, func(pub *skills.PublicationTx) error {
		head, err := pub.Current()
		if err == nil && head != (harness.SkillHead{}) {
			t.Fatal("new capability auto-installed", head)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := evolution.New(ctx, r, v)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	if err = restarted.ConfigureGeneration(ctx, g, consolidationTestOptions().Budget, []string{"project"}, false); err != nil {
		t.Fatal(err)
	}
	if err = restarted.ConfigureExperienceDiscoverySource(s.Store.Learning); err != nil {
		t.Fatal(err)
	}
	// No discovery configuration on reopen; explicit approval still requires
	// the exact live learning source, target policy and immutable candidate.
	p, err = restarted.Approve(ctx, p.ID, harness.SkillReview{ExpectedStateVersion: p.StateVersion, Actor: "owner"})
	if err != nil || p.Status != "published" || len(g.seen) != 1 {
		t.Fatal("new capability manual recovery failed", p, err)
	}
	control, _ := s.Store.Learning.Control(ctx, x.ID)
	if _, err = s.SetSkillLearningControl(ctx, "owner", x.ID, harness.SkillLearningControlRequest{Contribute: false, ExpectedEpoch: control.Epoch}); err != nil {
		t.Fatal(err)
	}
	if adopted, err := restarted.Get(ctx, p.ID); err != nil || adopted.Status != "published" {
		t.Fatal("forget removed explicitly adopted asset", adopted, err)
	}
}

func TestSkillLearningExperienceNewSkillOptInOutputAndRevocationGuards(t *testing.T) {
	for _, mode := range []string{"separate-gate", "missing-name", "collision", "extra-file", "revocation", "new-head", "renamed-revision", "enqueue-fault", "pending-before", "pending-after"} {
		t.Run(mode, func(t *testing.T) {
			s, x, r, e, g, _, matcher := experienceDiscoveryTest(t)
			ctx := context.Background()
			matcher.relation, matcher.suggestedName = "new_capability", "structured-json-audit"
			if mode == "missing-name" {
				matcher.suggestedName = ""
			} else if mode == "collision" {
				matcher.suggestedName = "json-check"
			}
			experienceTwoEpisodes(t, s, x)
			if mode == "separate-gate" {
				enableExperienceDiscovery(t, e)
			} else {
				enableNewExperienceDiscovery(t, e)
			}
			pending := func() {
				t.Helper()
				other, err := evolution.New(ctx, r, &experienceLocalValidator{})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = other.Close() })
				p, err := other.Submit(ctx, harness.SkillProposalRequest{SourceID: "project", Name: matcher.suggestedName, RequestKey: "independent-new-capability", Action: "create", Reason: "Independent manually authored capability", Files: map[string][]byte{"SKILL.md": []byte("---\nname: " + matcher.suggestedName + "\ndescription: Independent procedure\n---\nVerify independent input\n")}})
				if err != nil || p.Status != "pending_review" {
					t.Fatal("independent pending new name failed", p, err)
				}
			}
			if mode == "pending-before" {
				pending()
				if ds := experienceDiscoveries(t, e); len(ds) != 0 || len(g.seen) != 0 {
					t.Fatal("pending same-name capability bypassed creation guard", ds)
				}
				return
			}
			if mode == "enqueue-fault" {
				if _, err := s.Store.db.Exec(`CREATE TRIGGER reject_capability_reservation BEFORE INSERT ON df_learning_new_skill_consumed BEGIN SELECT RAISE(FAIL,'fixture reservation failure'); END`); err != nil {
					t.Fatal(err)
				}
				if _, err := e.RunDiscovery(ctx); err == nil {
					t.Fatal("faulted creation reservation committed")
				}
				for _, query := range []string{
					`SELECT count(*) FROM df_skill_generation_inputs`,
					`SELECT count(*) FROM df_skill_discoveries`,
					`SELECT count(*) FROM df_learning_promotion_dependencies`,
					`SELECT count(*) FROM df_learning_new_skill_consumed`,
					`SELECT count(*) FROM df_skill_discovery_episode_consumed`,
					`SELECT COALESCE(sum(reservations),0) FROM df_skill_discovery_days`,
				} {
					var count int
					if err := s.Store.db.QueryRow(query).Scan(&count); err != nil || count != 0 {
						t.Fatal("new capability enqueue was not atomic", query, count, err)
					}
				}
				if _, err := s.Store.db.Exec(`DROP TRIGGER reject_capability_reservation`); err != nil {
					t.Fatal(err)
				}
				if ds := experienceDiscoveries(t, e); len(ds) != 1 || len(g.seen) != 0 {
					t.Fatal("creation did not recover after atomic rollback", ds)
				}
				return
			}
			ds := experienceDiscoveries(t, e)
			if mode == "separate-gate" || mode == "missing-name" || mode == "collision" {
				if len(ds) != 0 || len(g.seen) != 0 {
					t.Fatal("new skill bypassed grant/name/collision", mode, ds)
				}
				return
			}
			if mode == "renamed-revision" {
				p := experienceGenerate(t, e)
				if _, err := e.Reject(ctx, p.ID, harness.SkillReview{ExpectedStateVersion: p.StateVersion, Actor: "owner"}); err != nil {
					t.Fatal(err)
				}
				matcher.suggestedName = "renamed-json-audit"
				for _, task := range []string{"Third independent full JSON task", "Fourth independent full JSON task"} {
					if _, err := s.StartSkillLearningEpisode(ctx, "owner", x.ID); err != nil {
						t.Fatal(err)
					}
					consolidateExtract(t, s, x, task)
					consolidateRun(t, s)
				}
				experienceMatch(t, s)
				if again := experienceDiscoveries(t, e); len(again) != 1 || len(g.seen) != 1 {
					t.Fatal("renamed new revision bypassed lifetime creation dedup", again, len(g.seen))
				}
				return
			}
			if len(ds) != 1 {
				t.Fatal(ds)
			}
			if mode == "extra-file" {
				g.extra = true
				if worked, err := e.RunGeneration(ctx); err != nil || !worked {
					t.Fatal(worked, err)
				}
				job, err := e.GetGeneration(ctx, ds[0].GenerationID)
				if err != nil || job.State != "failed" || job.ErrorCode != "invalid_candidate" || job.CandidateVersion != "" {
					t.Fatal("new skill script/support file retained", job, err)
				}
				return
			}
			p := experienceGenerate(t, e)
			if mode == "pending-after" {
				pending()
				stale, err := e.Approve(ctx, p.ID, harness.SkillReview{ExpectedStateVersion: p.StateVersion, Actor: "owner"})
				if !errors.Is(err, harness.ErrSkillConflict) || stale.Status != "stale" || len(g.seen) != 1 {
					t.Fatal("pending name added after review remained publishable", stale, err)
				}
				return
			}
			if mode == "new-head" {
				candidate, err := r.PrepareCandidate(p.SkillName, map[string][]byte{"SKILL.md": []byte("---\nname: " + p.SkillName + "\ndescription: Independently supplied procedure\n---\nAnother retained procedure\n")})
				if err != nil {
					t.Fatal(err)
				}
				if err = r.WithPublication(ctx, "project", p.SkillName, func(pub *skills.PublicationTx) error {
					version, err := pub.Stage(candidate)
					if err == nil {
						_, err = pub.Publish(version, harness.SkillHead{}, true)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				stale, err := e.Approve(ctx, p.ID, harness.SkillReview{ExpectedStateVersion: p.StateVersion, Actor: "owner"})
				if !errors.Is(err, harness.ErrSkillConflict) || stale.Status != "stale" {
					t.Fatal("new-name collision silently rebased", stale, err)
				}
				return
			}
			control, _ := s.Store.Learning.Control(ctx, x.ID)
			if _, err := s.SetSkillLearningControl(ctx, "owner", x.ID, harness.SkillLearningControlRequest{Contribute: false, ExpectedEpoch: control.Epoch}); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Get(ctx, p.ID); !errors.Is(err, harness.ErrNotFound) {
				t.Fatal("revoked new candidate survived", err)
			}
			var count int
			if err := s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_versions WHERE source_key=? AND name=? AND version=?`, p.Target.SourceIdentity, p.SkillName, p.CandidateVersion).Scan(&count); err != nil || count != 0 {
				t.Fatal("revoked one-file staged bytes survived", count, err)
			}
			if err := s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_discovery_episode_consumed`).Scan(&count); err != nil || count != 2 {
				t.Fatal("forget refunded independent consumption", count, err)
			}
			if err := s.Store.db.QueryRow(`SELECT count(*) FROM df_learning_new_skill_consumed`).Scan(&count); err != nil || count != 1 {
				t.Fatal("forget refunded creation reservation", count, err)
			}
		})
	}
}
