package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/learning"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills/evolution"
)

type experienceDiscoveryGenerator struct {
	seen                []harness.SkillGenerationInput
	extra               bool
	during              func()
	preserveFrontmatter bool
	descriptionOnly     bool
	extraInstruction    string
}

func (g *experienceDiscoveryGenerator) Identity() string { return "experience-generation-fixture-v1" }
func (g *experienceDiscoveryGenerator) Generate(ctx context.Context, input harness.SkillGenerationInput) (harness.SkillGeneratedCandidate, error) {
	g.seen = append(g.seen, input)
	if err := consolidationTestCall(ctx, "generation"); err != nil {
		return harness.SkillGeneratedCandidate{}, err
	}
	files := input.BaseFiles
	if files == nil {
		files = map[string][]byte{}
	}
	header := "---\nname: " + input.Target.Name + "\ndescription: Inspect structured JSON configuration\n---\n"
	originalMain := string(input.BaseFiles["SKILL.md"])
	if g.preserveFrontmatter {
		base := originalMain
		if end := strings.Index(strings.TrimPrefix(base, "---\n"), "\n---\n"); strings.HasPrefix(base, "---\n") && end >= 0 {
			header = base[:4+end+5]
		}
	}
	files["SKILL.md"] = []byte(header + "Read full JSON text before validating required fields\n")
	if g.descriptionOnly {
		base := strings.ReplaceAll(originalMain, "\r\n", "\n")
		if end := strings.Index(strings.TrimPrefix(base, "---\n"), "\n---\n"); strings.HasPrefix(base, "---\n") && end >= 0 {
			files["SKILL.md"] = []byte(header + base[4+end+5:])
		}
	}
	files["SKILL.md"] = append(files["SKILL.md"], []byte(g.extraInstruction)...)
	if g.extra {
		files["scripts/extra.sh"] = []byte("echo extra\n")
	}
	if g.during != nil {
		g.during()
	}
	return harness.SkillGeneratedCandidate{Reason: "Use complete structured input token=PRIVATE_GENERATION_REASON", Files: files}, nil
}

type experienceDiscoveryValidator struct {
	during   func()
	calls    int
	failures int
}

func (*experienceDiscoveryValidator) Identity() string { return "experience-review-fixture-v1" }
func (v *experienceDiscoveryValidator) Validate(ctx context.Context, in harness.SkillValidationInput) (harness.SkillValidationReport, error) {
	v.calls++
	for _, stage := range []string{"security", "quality"} {
		if err := consolidationTestCall(ctx, stage); err != nil {
			return harness.SkillValidationReport{}, err
		}
	}
	if v.during != nil {
		v.during()
	}
	if v.failures > 0 {
		v.failures--
		return harness.SkillValidationReport{}, harness.ErrSkillValidation
	}
	return harness.SkillValidationReport{ValidatorVersion: v.Identity(), BaseVersion: in.Proposal.Base.Version, CandidateVersion: in.Proposal.CandidateVersion, Security: "passed", Behavior: "not_run"}, nil
}

func experienceDiscoveryTest(t *testing.T) (*Service, harness.Session, *skills.Registry, *evolution.Service, *experienceDiscoveryGenerator, *experienceDiscoveryValidator, *matchingTestModel) {
	t.Helper()
	s, x, r, _, matcher := newMatchingTest(t)
	g, v := &experienceDiscoveryGenerator{}, &experienceDiscoveryValidator{}
	e, err := evolution.New(context.Background(), r, v)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err = e.ConfigureGeneration(context.Background(), g, consolidationTestOptions().Budget, []string{"project"}, false); err != nil {
		t.Fatal(err)
	}
	if err = e.ConfigureExperienceDiscoverySource(s.Store.Learning); err != nil {
		t.Fatal(err)
	}
	return s, x, r, e, g, v, matcher
}

func enableExperienceDiscovery(t *testing.T, e *evolution.Service) {
	t.Helper()
	if err := e.ConfigureDiscovery(harness.SkillDiscoveryOptions{DisableOperatorCorrections: true, ExperiencePatternsEnabled: true, RepeatThreshold: 2, RepeatWindowDays: 30, CooldownHours: 24, MaxDailyProposals: 5, MaxPendingProposals: 20}); err != nil {
		t.Fatal(err)
	}
}

func experienceMatch(t *testing.T, s *Service) string {
	t.Helper()
	for range 8 {
		if _, err := s.Store.Learning.QueueMatchOne(context.Background()); err != nil {
			t.Fatal(err)
		}
		var queued bool
		if err := s.Store.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM df_learning_match_inputs WHERE state='queued')`).Scan(&queued); err != nil {
			t.Fatal(err)
		}
		if queued {
			break
		}
	}
	if worked, err := s.Store.Learning.RunOne(context.Background()); err != nil || !worked {
		var state, reason string
		_ = s.Store.db.QueryRow(`SELECT state,reason FROM df_learning_match_inputs ORDER BY sequence DESC LIMIT 1`).Scan(&state, &reason)
		t.Fatal("matching did not run", worked, err, state, reason)
	}
	var id string
	if err := s.Store.db.QueryRow(`SELECT id FROM df_learning_matches ORDER BY rowid DESC LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func experienceTwoEpisodes(t *testing.T, s *Service, x harness.Session) string {
	t.Helper()
	consolidateExtract(t, s, x, "First independent complete JSON check")
	consolidateRun(t, s)
	if _, err := s.StartSkillLearningEpisode(context.Background(), "owner", x.ID); err != nil {
		t.Fatal(err)
	}
	consolidateExtract(t, s, x, "Another complete JSON check")
	consolidateRun(t, s)
	return experienceMatch(t, s)
}

func experienceDiscoveries(t *testing.T, e *evolution.Service) []harness.SkillDiscovery {
	t.Helper()
	for range 2 {
		if _, err := e.RunDiscovery(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	page, err := e.QueryDiscoveries(context.Background(), harness.SkillQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	return page.Discoveries
}

func experienceGenerate(t *testing.T, e *evolution.Service) harness.SkillProposal {
	t.Helper()
	if worked, err := e.RunGeneration(context.Background()); err != nil || !worked {
		t.Fatal(worked, err)
	}
	page, err := e.QueryProposals(context.Background(), harness.SkillQuery{Limit: 10})
	if err != nil || len(page.Proposals) != 1 {
		t.Fatal(page, err)
	}
	return page.Proposals[0]
}

func TestSkillLearningExperienceDiscoveryOptInIndependentEpisodesAndManualPublication(t *testing.T) {
	s, x, r, e, g, _, _ := experienceDiscoveryTest(t)
	ctx := context.Background()
	consolidateExtract(t, s, x, "Check configuration")
	consolidateRun(t, s)
	consolidateExtract(t, s, x, "Correct previous configuration check")
	consolidateRun(t, s)
	id := experienceMatch(t, s)
	if _, err := s.Store.Learning.Promotion(ctx, id, 2, time.Now().Add(-24*time.Hour), time.Now()); err == nil {
		t.Fatal("one multi-root episode authorized generation")
	}
	if _, err := s.StartSkillLearningEpisode(ctx, "owner", x.ID); err != nil {
		t.Fatal(err)
	}
	consolidateExtract(t, s, x, "Independent configuration check")
	consolidateRun(t, s)
	id = experienceMatch(t, s)
	if _, err := e.RunDiscovery(ctx); err == nil {
		t.Fatal("matching implicitly enabled discovery")
	}
	enableExperienceDiscovery(t, e)
	ds := experienceDiscoveries(t, e)
	if len(ds) != 1 || ds[0].Kind != "experience_pattern" || ds[0].Attribution != "unassessed" || len(ds[0].Evidence) != 2 || ds[0].Evidence[0].Experience.MatchID != id || ds[0].Evidence[0].EpisodeKey == ds[0].Evidence[1].EpisodeKey {
		t.Fatal("missing independently pinned evidence", ds)
	}
	var feedback int
	if err := s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_feedback`).Scan(&feedback); err != nil || feedback != 0 {
		t.Fatal("experience fabricated operator feedback", feedback, err)
	}
	p := experienceGenerate(t, e)
	if p.Status != "pending_review" || p.Trigger.Type != "experience_pattern" || len(g.seen) != 1 || !g.seen[0].InstructionOnly || len(g.seen[0].Evidence) < 1 || strings.Contains(p.Reason, "PRIVATE_GENERATION_REASON") {
		t.Fatal("not an instruction-only redacted pending review", p, g.seen)
	}
	if err := r.WithPublication(ctx, p.Target.SourceID, p.SkillName, func(tx *skills.PublicationTx) error {
		head, err := tx.Current()
		if err == nil && head != ds[0].Base {
			t.Fatal("generation automatically published", head)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if ds := experienceDiscoveries(t, e); len(ds) != 1 {
		t.Fatal("repeated scan duplicated discovery", ds)
	}
	p, err := e.Approve(ctx, p.ID, harness.SkillReview{ExpectedStateVersion: p.StateVersion, Actor: "owner", Note: "Reviewed the observed procedure"})
	if err != nil || p.Status != "published" {
		t.Fatal("manual publication failed", p, err)
	}
	control, _ := s.Store.Learning.Control(ctx, x.ID)
	if _, err = s.SetSkillLearningControl(ctx, "owner", x.ID, harness.SkillLearningControlRequest{Contribute: false, ExpectedEpoch: control.Epoch}); err != nil {
		t.Fatal(err)
	}
	if adopted, err := e.Get(ctx, p.ID); err != nil || adopted.Status != "published" {
		t.Fatal("revocation deleted adopted skill", adopted, err)
	}
}

func TestSkillLearningExperienceDiscoveryMatchingRelationsDoNotGrantNewTargets(t *testing.T) {
	for _, relation := range []string{"trigger_mismatch", "already_covered", "new_capability", "ambiguous", "irrelevant"} {
		t.Run(relation, func(t *testing.T) {
			s, x, _, e, g, _, matcher := experienceDiscoveryTest(t)
			matcher.relation = relation
			experienceTwoEpisodes(t, s, x)
			enableExperienceDiscovery(t, e)
			ds := experienceDiscoveries(t, e)
			if relation != "trigger_mismatch" {
				if len(ds) != 0 || len(g.seen) != 0 {
					t.Fatal("non-actionable matching relation granted a target", relation, ds)
				}
				return
			}
			if len(ds) != 1 || ds[0].Target.Name != "json-check" || !ds[0].Base.Exists {
				t.Fatal("precise trigger mismatch did not pin existing target", ds)
			}
			p := experienceGenerate(t, e)
			if p.Status != "pending_review" || p.Action != "edit" || !g.seen[0].InstructionOnly {
				t.Fatal("trigger mismatch granted creation or publication", p)
			}
		})
	}
}

func TestSkillLearningExperienceDiscoveryRevocationCleansUnpublishedTextAndVersions(t *testing.T) {
	s, x, _, e, _, _, _ := experienceDiscoveryTest(t)
	ctx := context.Background()
	experienceTwoEpisodes(t, s, x)
	enableExperienceDiscovery(t, e)
	ds := experienceDiscoveries(t, e)
	if len(ds) != 1 {
		t.Fatal(ds)
	}
	p := experienceGenerate(t, e)
	if p.Status != "pending_review" {
		t.Fatal(p)
	}
	var charge int64
	if err := s.Store.db.QueryRow(`SELECT sum(charged_tokens) FROM df_skill_maintenance_calls`).Scan(&charge); err != nil || charge < 1 {
		t.Fatal(charge, err)
	}
	control, _ := s.Store.Learning.Control(ctx, x.ID)
	if _, err := s.SetSkillLearningControl(ctx, "owner", x.ID, harness.SkillLearningControlRequest{Contribute: false, ExpectedEpoch: control.Epoch}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"df_skill_generation_inputs", "df_skill_proposals", "df_skill_discoveries", "df_learning_matches", "df_learning_patterns"} {
		var count int
		if err := s.Store.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("revoked derived text survived", table, count, err)
		}
	}
	var count int
	if err := s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_versions WHERE source_key=? AND name=? AND version=?`, p.Target.SourceIdentity, p.SkillName, p.CandidateVersion).Scan(&count); err != nil || count != 0 {
		t.Fatal("unpublished learning version survived", count, err)
	}
	if _, err := e.Approve(ctx, p.ID, harness.SkillReview{ExpectedStateVersion: p.StateVersion, Actor: "owner"}); !errors.Is(err, harness.ErrNotFound) {
		t.Fatal("revoked proposal published", err)
	}
	var retained int64
	if err := s.Store.db.QueryRow(`SELECT sum(charged_tokens) FROM df_skill_maintenance_calls`).Scan(&retained); err != nil || retained != charge {
		t.Fatal("revocation refunded spending", retained, charge, err)
	}
	if err := s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_versions WHERE version=?`, ds[0].Base.Version).Scan(&count); err != nil || count != 1 {
		t.Fatal("baseline deleted", count, err)
	}
}

func TestSkillLearningExperienceDiscoveryHeadReceiptAndGenerationOutputGuards(t *testing.T) {
	for _, mode := range []string{"head-before-discovery", "receipt-before-discovery", "head-during-generation", "consent-during-generation", "consent-during-review", "support-file"} {
		t.Run(mode, func(t *testing.T) {
			s, x, r, e, g, v, _ := experienceDiscoveryTest(t)
			ctx := context.Background()
			experienceTwoEpisodes(t, s, x)
			enableExperienceDiscovery(t, e)
			changeHead := func() {
				if err := r.SetEnabled(ctx, "project", "json-check", false); err != nil {
					t.Fatal(err)
				}
			}
			revoke := func() {
				control, _ := s.Store.Learning.Control(ctx, x.ID)
				if _, err := s.SetSkillLearningControl(ctx, "owner", x.ID, harness.SkillLearningControlRequest{Contribute: false, ExpectedEpoch: control.Epoch}); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "head-before-discovery" {
				changeHead()
			} else if mode == "receipt-before-discovery" {
				if _, err := s.Store.db.Exec(`UPDATE harness_tool_receipts SET version=version+1`); err != nil {
					t.Fatal(err)
				}
			}
			ds := experienceDiscoveries(t, e)
			if strings.HasSuffix(mode, "before-discovery") {
				if len(ds) != 0 {
					t.Fatal("changed source admitted", ds)
				}
				return
			}
			if len(ds) != 1 {
				t.Fatal(ds)
			}
			switch mode {
			case "head-during-generation":
				g.during = changeHead
			case "consent-during-generation":
				g.during = revoke
			case "consent-during-review":
				v.during = revoke
			case "support-file":
				g.extra = true
			}
			if worked, err := e.RunGeneration(ctx); err != nil || !worked {
				t.Fatal(worked, err)
			}
			var proposal, versions int
			if err := s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_proposals WHERE status='pending_review'`).Scan(&proposal); err != nil || proposal != 0 {
				t.Fatal("late/unsupported result became reviewable", proposal, err)
			}
			if mode == "support-file" || mode == "consent-during-generation" || mode == "consent-during-review" {
				if err := s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_versions`).Scan(&versions); err != nil || versions != 1 {
					t.Fatal("unsafe or revoked staged version survived", versions, err)
				}
			}
		})
	}
}

func TestSkillLearningExperienceDiscoveryApprovalRevalidatesLiveAuthority(t *testing.T) {
	for _, mode := range []string{"receipt", "pattern", "selection", "model", "consent", "host-disabled"} {
		t.Run(mode, func(t *testing.T) {
			s, x, r, e, g, _, _ := experienceDiscoveryTest(t)
			ctx := context.Background()
			experienceTwoEpisodes(t, s, x)
			enableExperienceDiscovery(t, e)
			ds := experienceDiscoveries(t, e)
			if len(ds) != 1 {
				t.Fatal(ds)
			}
			p := experienceGenerate(t, e)
			switch mode {
			case "receipt":
				if _, err := s.Store.db.Exec(`UPDATE harness_tool_receipts SET version=version+1`); err != nil {
					t.Fatal(err)
				}
			case "pattern":
				if _, err := s.StartSkillLearningEpisode(ctx, "owner", x.ID); err != nil {
					t.Fatal(err)
				}
				consolidateExtract(t, s, x, "A third independent configuration check")
				consolidateRun(t, s)
			case "selection", "model", "host-disabled":
				if err := s.Store.Learning.Close(); err != nil {
					t.Fatal(err)
				}
				opts := consolidationTestOptions()
				opts.Matching = &harness.SkillGenerationModelConfig{}
				if mode == "host-disabled" {
					opts = nil
				}
				l, err := learning.New(ctx, s.Store.db, opts)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = l.Close() })
				if mode != "host-disabled" {
					selection := harness.SkillSelection{}
					if mode == "selection" {
						selection.Names = []string{}
					}
					access, err := r.NewMatchAccess(selection)
					if err != nil {
						t.Fatal(err)
					}
					if err = l.Configure(ctx, &consolidationTestExtractor{observed: true}, nil); err != nil {
						t.Fatal(err)
					}
					var consolidator harness.SkillExperienceConsolidator = &consolidationTestModel{}
					if mode == "model" {
						consolidator = &experienceChangedConsolidator{}
					}
					if err = l.ConfigureConsolidation(consolidator); err != nil {
						t.Fatal(err)
					}
					if err = l.ConfigureMatching(&matchingTestModel{}, access); err != nil {
						t.Fatal(err)
					}
				}
				s.Store.Learning = l
				next, err := evolution.New(ctx, r, &experienceDiscoveryValidator{})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = next.Close() })
				if err = next.ConfigureGeneration(ctx, g, consolidationTestOptions().Budget, []string{"project"}, false); err != nil {
					t.Fatal(err)
				}
				if mode != "host-disabled" {
					if err = next.ConfigureExperienceDiscoverySource(l); err != nil {
						t.Fatal(err)
					}
				}
				e = next
			case "consent":
				control, _ := s.Store.Learning.Control(ctx, x.ID)
				if _, err := s.SetSkillLearningControl(ctx, "owner", x.ID, harness.SkillLearningControlRequest{Contribute: false, ExpectedEpoch: control.Epoch}); err != nil {
					t.Fatal(err)
				}
			}
			got, err := e.Approve(ctx, p.ID, harness.SkillReview{ExpectedStateVersion: p.StateVersion, Actor: "owner"})
			if mode == "consent" {
				if !errors.Is(err, harness.ErrNotFound) {
					t.Fatal("revoked proposal survived", got, err)
				}
			} else if !errors.Is(err, harness.ErrSkillConflict) || got.Status != "stale" {
				t.Fatal("invalid source remained publishable", got, err)
			}
			if err := r.WithPublication(ctx, "project", "json-check", func(tx *skills.PublicationTx) error {
				head, err := tx.Current()
				if err == nil && head != ds[0].Base {
					t.Fatal("approval changed head with invalid evidence", head)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if len(g.seen) != 1 {
				t.Fatal("approval called generation again", len(g.seen))
			}
		})
	}
}

type experienceChangedConsolidator struct{ consolidationTestModel }

func (*experienceChangedConsolidator) Identity() string { return "consolidation-fixture-v2" }

func TestSkillLearningExperienceDiscoverySavedCandidateAndProposalRestartReuse(t *testing.T) {
	for _, phase := range []string{"candidate", "proposal"} {
		t.Run(phase, func(t *testing.T) {
			s, x, r, e, g, v, _ := experienceDiscoveryTest(t)
			ctx := context.Background()
			experienceTwoEpisodes(t, s, x)
			enableExperienceDiscovery(t, e)
			ds := experienceDiscoveries(t, e)
			if len(ds) != 1 {
				t.Fatal(ds)
			}
			if phase == "candidate" {
				if _, err := s.Store.db.Exec(`CREATE TRIGGER fail_experience_proposal BEFORE INSERT ON df_skill_proposals BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
					t.Fatal(err)
				}
			} else {
				v.failures = 1
			}
			if worked, err := e.RunGeneration(ctx); err != nil || !worked {
				t.Fatal(worked, err)
			}
			job, err := e.GetGeneration(ctx, ds[0].GenerationID)
			if err != nil || job.State != "retry" || job.CandidateVersion == "" || len(g.seen) != 1 || (job.ProposalID == "") != (phase == "candidate") {
				t.Fatal("checkpoint not persisted", job, err)
			}
			if err = e.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err = s.Store.db.Exec(`DROP TRIGGER IF EXISTS fail_experience_proposal; UPDATE df_skill_maintenance_jobs SET retry_ns=0 WHERE id=?`, job.ID); err != nil {
				t.Fatal(err)
			}
			next, err := evolution.New(ctx, r, v)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = next.Close() })
			if err = next.ConfigureGeneration(ctx, g, consolidationTestOptions().Budget, []string{"project"}, false); err != nil {
				t.Fatal(err)
			}
			if err = next.ConfigureExperienceDiscoverySource(s.Store.Learning); err != nil {
				t.Fatal(err)
			}
			enableExperienceDiscovery(t, next)
			p := experienceGenerate(t, next)
			finished, err := next.GetGeneration(ctx, job.ID)
			if err != nil || p.Status != "pending_review" || finished.State != "completed" || finished.Attempts != 2 || finished.CandidateVersion != job.CandidateVersion || job.ProposalID != "" && finished.ProposalID != job.ProposalID || len(g.seen) != 1 {
				t.Fatal("recovery lost checkpoint or repeated generation", finished, p, err)
			}
			receipts, err := next.GenerationReceipts(ctx, job.ID, 0, 10)
			if err != nil {
				t.Fatal(err)
			}
			generationCalls := 0
			for _, receipt := range receipts {
				if receipt.Stage == "generation" {
					generationCalls++
				}
			}
			if generationCalls != 1 || len(receipts) != 1+v.calls*2 {
				t.Fatal("recovery replayed spending", receipts, v.calls)
			}
		})
	}
}

func TestSkillLearningExperienceDiscoveryRevocationPreservesIndependentRetention(t *testing.T) {
	for _, mode := range []string{"preexisting", "independent-proposal", "adopted-other-proposal"} {
		t.Run(mode, func(t *testing.T) {
			s, x, r, e, g, _, _ := experienceDiscoveryTest(t)
			ctx := context.Background()
			experienceTwoEpisodes(t, s, x)
			enableExperienceDiscovery(t, e)
			ds := experienceDiscoveries(t, e)
			if len(ds) != 1 {
				t.Fatal(ds)
			}
			var candidateVersion string
			if mode == "preexisting" {
				files := map[string][]byte{"SKILL.md": []byte("---\nname: json-check\ndescription: Inspect structured JSON configuration\n---\nRead full JSON text before validating required fields\n")}
				candidate, err := r.PrepareCandidate("json-check", files)
				if err != nil {
					t.Fatal(err)
				}
				if err := r.WithPublication(ctx, "project", "json-check", func(tx *skills.PublicationTx) error {
					var err error
					candidateVersion, err = tx.Stage(candidate)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			p := experienceGenerate(t, e)
			var independent harness.SkillProposal
			if mode == "preexisting" {
				if candidateVersion != p.CandidateVersion {
					t.Fatal("fixture did not reuse independent version", candidateVersion, p.CandidateVersion)
				}
				var owned bool
				if err := s.Store.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM df_learning_owned_versions WHERE version=?)`, candidateVersion).Scan(&owned); err != nil || owned {
					t.Fatal("learning claimed a preexisting immutable version", owned, err)
				}
			} else {
				var files map[string][]byte
				if err := r.WithPublication(ctx, "project", "json-check", func(tx *skills.PublicationTx) error {
					candidate, err := tx.ReadVersion(p.CandidateVersion)
					if err == nil {
						files = candidate.Files()
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				// A trusted local oracle reviews this unrelated manual proposal;
				// it performs no model call and has no learning provenance.
				other, err := evolution.New(ctx, r, &experienceLocalValidator{})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = other.Close() })
				independent, err = other.Submit(ctx, harness.SkillProposalRequest{SourceID: "project", Name: "json-check", RequestKey: "independent-retention", Action: "edit", Reason: "An independently authored candidate", Base: p.Base, Files: files})
				if err != nil || independent.Status != "pending_review" {
					t.Fatal("independent proposal not retained", independent, err)
				}
				if mode == "adopted-other-proposal" {
					independent, err = other.Approve(ctx, independent.ID, harness.SkillReview{ExpectedStateVersion: independent.StateVersion, Actor: "owner"})
					if err != nil || independent.Status != "published" {
						t.Fatal("independent adoption failed", independent, err)
					}
				}
			}
			control, _ := s.Store.Learning.Control(ctx, x.ID)
			if _, err := s.SetSkillLearningControl(ctx, "owner", x.ID, harness.SkillLearningControlRequest{Contribute: false, ExpectedEpoch: control.Epoch}); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Get(ctx, p.ID); !errors.Is(err, harness.ErrNotFound) {
				t.Fatal("revoked learning proposal survived", err)
			}
			if err := r.WithPublication(ctx, "project", "json-check", func(tx *skills.PublicationTx) error {
				_, err := tx.ReadVersion(p.CandidateVersion)
				return err
			}); err != nil {
				t.Fatal("independently retained immutable version deleted", err)
			}
			if mode != "preexisting" {
				if got, err := e.Get(ctx, independent.ID); err != nil || got.Status != independent.Status {
					t.Fatal("independent proposal changed", got, err)
				}
			}
			if len(g.seen) != 1 {
				t.Fatal("revocation called generation", len(g.seen))
			}
		})
	}
}

type experienceLocalValidator struct{}

func (*experienceLocalValidator) Validate(_ context.Context, in harness.SkillValidationInput) (harness.SkillValidationReport, error) {
	return harness.SkillValidationReport{ValidatorVersion: "independent-review-fixture", BaseVersion: in.Proposal.Base.Version, CandidateVersion: in.Proposal.CandidateVersion, Security: "passed", Behavior: "not_run"}, nil
}

func TestSkillLearningExperienceDiscoverySharedQuotaCrossTriggerDedupAndAtomicity(t *testing.T) {
	for _, mode := range []string{"daily", "pending", "cooldown", "cross-trigger", "fault"} {
		t.Run(mode, func(t *testing.T) {
			s, x, _, e, _, _, _ := experienceDiscoveryTest(t)
			ctx := context.Background()
			id := experienceTwoEpisodes(t, s, x)
			p, err := s.Store.Learning.Promotion(ctx, id, 2, time.Now().Add(-24*time.Hour), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			enableExperienceDiscovery(t, e)
			switch mode {
			case "daily":
				_, err = s.Store.db.Exec(`INSERT INTO df_skill_discovery_days VALUES(?,5)`, time.Now().UTC().Format("2006-01-02"))
			case "cooldown":
				_, err = s.Store.db.Exec(`INSERT INTO df_skill_discovery_cooldowns VALUES(?,?,?)`, p.Target.SourceIdentity, p.Target.Name, time.Now().Add(time.Hour).UnixNano())
			case "pending":
				for i := range 20 {
					_, err = e.EnqueueGeneration(ctx, harness.SkillGenerationRequest{SourceID: "project", Name: "different-" + strings.Repeat("a", i+1), RequestKey: "pending", Reason: "Operator task"})
					if err != nil {
						t.Fatal(err)
					}
				}
			case "cross-trigger":
				data := []string{"discovery-episode-target-version-v1", p.Evidence[0].Experience.IndependenceKey, p.Target.SourceIdentity, p.Target.Name, p.Head.Version}
				encoded, _ := json.Marshal(data)
				digest := sha256.Sum256(encoded)
				_, err = s.Store.db.Exec(`INSERT INTO df_skill_discovery_episode_consumed VALUES(?,?)`, hex.EncodeToString(digest[:]), "prior-operator-discovery")
			case "fault":
				_, err = s.Store.db.Exec(`CREATE TRIGGER deny_experience_binding BEFORE INSERT ON df_learning_promotion_dependencies BEGIN SELECT RAISE(ABORT,'binding fixture'); END`)
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "fault" {
				if _, err = e.RunDiscovery(ctx); err == nil {
					t.Fatal("injected binding failure ignored")
				}
				var count int
				if err = s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_generation_inputs`).Scan(&count); err != nil || count != 0 {
					t.Fatal("partial generation enqueue", count, err)
				}
			} else if ds := experienceDiscoveries(t, e); len(ds) != 0 {
				t.Fatal("shared gate bypassed", ds)
			}
			var count int
			if err = s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_discoveries`).Scan(&count); err != nil || count != 0 {
				t.Fatal("partial discovery", count, err)
			}
			if mode == "fault" {
				if err = s.Store.db.QueryRow(`SELECT COALESCE(sum(reservations),0) FROM df_skill_discovery_days`).Scan(&count); err != nil || count != 0 {
					t.Fatal("partial daily charge", count, err)
				}
			}
		})
	}
}

func TestSkillLearningExperienceDiscoveryQueuedCancellationNeverRegenerates(t *testing.T) {
	s, x, _, e, g, _, _ := experienceDiscoveryTest(t)
	ctx := context.Background()
	experienceTwoEpisodes(t, s, x)
	enableExperienceDiscovery(t, e)
	ds := experienceDiscoveries(t, e)
	if len(ds) != 1 {
		t.Fatal(ds)
	}
	job, err := e.GetGeneration(ctx, ds[0].GenerationID)
	if err != nil || job.State != "queued" {
		t.Fatal(job, err)
	}
	if err = e.CancelGeneration(ctx, job.ID, job.StateVersion); err != nil {
		t.Fatal(err)
	}
	if worked, err := e.RunGeneration(ctx); err != nil || worked || len(g.seen) != 0 {
		t.Fatal("cancelled job generated", worked, err, len(g.seen))
	}
	if again := experienceDiscoveries(t, e); len(again) != 1 || again[0].GenerationID != job.ID || again[0].State != "cancelled" {
		t.Fatal("cancellation refunded consumed episodes", again)
	}
	control, _ := s.Store.Learning.Control(ctx, x.ID)
	if _, err = s.SetSkillLearningControl(ctx, "owner", x.ID, harness.SkillLearningControlRequest{Contribute: false, ExpectedEpoch: control.Epoch}); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`SELECT count(*) FROM df_skill_generation_inputs`,
		`SELECT count(*) FROM df_skill_discoveries`,
	} {
		var count int
		if err = s.Store.db.QueryRow(query).Scan(&count); err != nil || count != 0 {
			t.Fatal("unpublished cancelled provenance survived", count, err)
		}
	}
	retained, err := skillmaintenance.ReadJob(ctx, s.Store.db, job.ID)
	if err != nil || retained.State != "cancelled" {
		t.Fatal("cancellation accounting lost", retained, err)
	}
}

func TestSkillLearningExperienceDiscoveryLegacyConsumedAliasDoesNotAddVote(t *testing.T) {
	s, x, _, e, g, _, _ := experienceDiscoveryTest(t)
	ctx := context.Background()
	first := consolidateExtract(t, s, x, "First part of one configuration task")
	consolidateRun(t, s)
	second := consolidateExtract(t, s, x, "Correction within the same configuration task")
	consolidateRun(t, s)
	if _, err := s.StartSkillLearningEpisode(ctx, "owner", x.ID); err != nil {
		t.Fatal(err)
	}
	consolidateExtract(t, s, x, "Independent configuration task")
	consolidateRun(t, s)
	id := experienceMatch(t, s)
	p, err := s.Store.Learning.Promotion(ctx, id, 2, time.Now().Add(-24*time.Hour), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	alias := first.RootBudgetID
	if first.RootBudgetID < second.RootBudgetID {
		alias = second.RootBudgetID
	}
	for _, ref := range p.Evidence {
		if ref.RootID == alias {
			t.Fatal("fixture selected the legacy consumed root instead of its alias")
		}
	}
	payload, _ := json.Marshal(map[string]any{"base": p.Head, "evidence": []any{map[string]any{"root_id": alias}}})
	digest := sha256.Sum256(payload)
	legacy, err := e.MaintenanceQueue().Enqueue(ctx, skillmaintenance.Request{SourceIdentity: p.Target.SourceIdentity, Name: p.Target.Name, Key: "legacy-dedup", Reference: "legacy-reference", InputDigest: strings.Repeat("e", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.MaintenanceQueue().Cancel(ctx, legacy.ID, legacy.StateVersion); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Store.db.Exec(`INSERT INTO df_skill_discoveries VALUES(?,?,?,?,?,?,?)`, "sd_legacy", p.Target.SourceIdentity, p.Target.Name, legacy.ID, payload, hex.EncodeToString(digest[:]), time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Store.db.Exec(`INSERT INTO df_skill_discovery_consumed VALUES(?,?)`, strings.Repeat("f", 64), "sd_legacy"); err != nil {
		t.Fatal(err)
	}
	enableExperienceDiscovery(t, e)
	for range 2 {
		if _, err = e.RunDiscovery(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err = s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_generation_inputs`).Scan(&count); err != nil || count != 0 || len(g.seen) != 0 {
		t.Fatal("legacy same-episode reservation was spent again", count, len(g.seen), err)
	}
}

func TestSkillLearningExperienceDiscoverySharedSourceDeletionCleansForeignProposal(t *testing.T) {
	s, x, r, _, _ := newMatchingTest(t)
	ctx := context.Background()
	_ = s.Store.Learning.Close()
	opts := consolidationTestOptions()
	opts.CrossSession, opts.Matching = true, &harness.SkillGenerationModelConfig{}
	l, err := learning.New(ctx, s.Store.db, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	// Use the exact same registry access, but newly explicit shared consent.
	access, err := r.NewMatchAccess(harness.SkillSelection{})
	if err != nil {
		t.Fatal(err)
	}
	if err = l.Configure(ctx, &consolidationTestExtractor{observed: true}, nil); err != nil {
		t.Fatal(err)
	}
	if err = l.ConfigureConsolidation(&consolidationTestModel{}); err != nil {
		t.Fatal(err)
	}
	if err = l.ConfigureMatching(&matchingTestModel{}, access); err != nil {
		t.Fatal(err)
	}
	s.Store.Learning = l
	// Construct the evolution bridge only after the replacement store is fixed.
	g, v := &experienceDiscoveryGenerator{}, &experienceDiscoveryValidator{}
	e, err := evolution.New(ctx, r, v)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err = e.ConfigureGeneration(ctx, g, opts.Budget, []string{"project"}, false); err != nil {
		t.Fatal(err)
	}
	if err = e.ConfigureExperienceDiscoverySource(l); err != nil {
		t.Fatal(err)
	}
	workspaceLearningConsent(t, s, x, true)
	consolidateExtract(t, s, x, "Shared first independent configuration")
	consolidateRun(t, s)
	y := workspaceLearningSession(t, s, x.CWD)
	workspaceLearningConsent(t, s, y, true)
	consolidateExtract(t, s, y, "Shared second independent configuration")
	consolidateRun(t, s)
	experienceMatch(t, s)
	enableExperienceDiscovery(t, e)
	if ds := experienceDiscoveries(t, e); len(ds) != 1 {
		t.Fatal(ds)
	}
	p := experienceGenerate(t, e)
	if p.Status != "pending_review" {
		t.Fatal(p)
	}
	if err = s.Coordinator.Detach(ctx, y.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DeleteSession(ctx, y.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Get(ctx, p.ID); !errors.Is(err, harness.ErrNotFound) {
		t.Fatal("foreign proposal survived shared source purge", err)
	}
	var original, versions int
	if err = s.Store.db.QueryRow(`SELECT count(*) FROM df_learning_experiences WHERE session_id=?`, x.ID).Scan(&original); err != nil || original != 1 {
		t.Fatal("unrelated original source erased", original, err)
	}
	if err = s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_versions`).Scan(&versions); err != nil || versions != 1 {
		t.Fatal("revoked shared candidate survived", versions, err)
	}
}
