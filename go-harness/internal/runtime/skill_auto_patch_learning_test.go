package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills/evolution"
)

type autoPatchLearningEvaluator struct {
	mode  string
	calls int
}

func (*autoPatchLearningEvaluator) Identity() string { return "learning-behavior-fixture-v1" }
func (e *autoPatchLearningEvaluator) Evaluate(ctx context.Context, in harness.SkillBehaviorInput) (harness.SkillBehaviorResult, error) {
	if err := consolidationTestCall(ctx, "behavior"); err != nil {
		return harness.SkillBehaviorResult{}, err
	}
	e.calls++
	passed, code := true, ""
	if in.Variant == "base" && (in.Case.Kind == "targeted" && e.mode != "unchanged" && e.mode != "regression-only" || in.Case.Kind == "regression" && e.mode == "regression-only") {
		passed, code = false, "assertion_failed"
	}
	if e.mode == "baseline-timeout" && in.Variant == "base" && in.Case.Kind == "targeted" {
		passed, code = false, "timeout"
	}
	if e.mode == "regression" && in.Variant == "candidate" && in.Case.Kind == "regression" {
		passed, code = false, "assertion_failed"
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(in.Variant)))
	out := harness.SkillBehaviorResult{CaseID: in.CaseID, Variant: in.Variant, Version: in.Version, Repetition: in.Repetition, Passed: passed, StopReason: "completed", ErrorCode: code, OutputDigest: digest, TraceDigest: digest, ModelCalls: 1, InputTokens: 10, OutputTokens: 5}
	for _, a := range in.Case.Assertions {
		out.Checks = append(out.Checks, harness.SkillBehaviorCheck{ID: a.ID, Passed: passed, ActualDigest: digest})
	}
	return out, nil
}

func autoPatchLearningSuite() harness.SkillBehaviorSuite {
	cases := []harness.SkillBehaviorCase{}
	for _, kind := range []string{"targeted", "regression"} {
		cases = append(cases, harness.SkillBehaviorCase{ID: kind, Kind: kind, Prompt: "Verify structured input", Repetitions: 1, Files: map[string]string{}, Fixtures: []harness.SkillBehaviorFixture{}, Assertions: []harness.SkillBehaviorAssertion{{ID: "verified", Kind: "output_equals", Expected: "verified"}}})
	}
	return harness.SkillBehaviorSuite{Version: "skill-behavior-v1", ID: "json-verification", SourceID: "project", Name: "json-check", Cases: cases}
}

func learningAutoPatchOptions(enabled bool) harness.SkillAutoPatchOptions {
	return harness.SkillAutoPatchOptions{ExperiencePatternsEnabled: enabled, MaxChangedLines: 40, MaxDailyPublications: 2, ObservationHours: 72, MinObservations: 3, RollbackFailureThreshold: 2}
}

func adoptLearningBase(t *testing.T, r *skills.Registry) {
	t.Helper()
	ctx := context.Background()
	var base harness.SkillHead
	var files map[string][]byte
	if err := r.WithPublication(ctx, "project", "json-check", func(tx *skills.PublicationTx) error {
		var err error
		base, err = tx.Current()
		if err != nil {
			return err
		}
		c, err := tx.ReadVersion(base.Version)
		if err == nil {
			files = c.Files()
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// The adopted baseline must not preserve the matching fixture's secrets.
	for _, marker := range []string{" token=PRIVATE_SKILL_MATCH_DESCRIPTION", " token=PRIVATE_SKILL_MATCH_BODY"} {
		files["SKILL.md"] = bytes.ReplaceAll(files["SKILL.md"], []byte(marker), nil)
	}
	files["SKILL.md"] = append(files["SKILL.md"], []byte("Reviewed baseline instruction\n")...)
	files["references/notes.txt"] = []byte("Approved support text; must remain unchanged.\n")
	local, err := evolution.New(ctx, r, &experienceLocalValidator{})
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	p, err := local.Submit(ctx, harness.SkillProposalRequest{SourceID: "project", Name: "json-check", RequestKey: "adopt-before-learning", Action: "edit", Reason: "Explicitly adopt managed baseline", Base: base, Files: files})
	if err != nil {
		t.Fatal(err)
	}
	if p, err = local.Approve(ctx, p.ID, harness.SkillReview{ExpectedStateVersion: p.StateVersion, Actor: "owner"}); err != nil || p.Status != "published" {
		t.Fatal("baseline not explicitly adopted", p, err)
	}
}

func autoPatchLearningProposal(t *testing.T, e *evolution.Service, discovery harness.SkillDiscovery) harness.SkillProposal {
	t.Helper()
	if worked, err := e.RunGeneration(context.Background()); err != nil || !worked {
		t.Fatal("generation did not run", worked, err)
	}
	j, err := e.GetGeneration(context.Background(), discovery.GenerationID)
	if err != nil {
		t.Fatal(err)
	}
	if j.ProposalID == "" {
		t.Fatalf("generation produced no proposal: state=%s phase=%s error=%s", j.State, j.Phase, j.ErrorCode)
	}
	p, err := e.Get(context.Background(), j.ProposalID)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSkillLearningAutoPatchAdmissionAndRevocation(t *testing.T) {
	for _, mode := range []string{"on", "old-grant", "imported", "new-skill", "description", "unchanged", "regression-only", "baseline-timeout", "regression", "receipt", "consent", "daily-shared", "observing-same-target", "atomic-observation"} {
		t.Run(mode, func(t *testing.T) {
			s, x, r, e, g, _, matcher := experienceDiscoveryTest(t)
			ctx := context.Background()
			if mode != "imported" {
				adoptLearningBase(t, r)
			} else {
				// Test unmanaged admission independently of the credential guard.
				// The matching fixture's fake secrets would reject generation first.
				path := filepath.Join(x.CWD, "skills", "json-check", "SKILL.md")
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				for _, marker := range []string{" token=PRIVATE_SKILL_MATCH_DESCRIPTION", " token=PRIVATE_SKILL_MATCH_BODY"} {
					body = bytes.ReplaceAll(body, []byte(marker), nil)
				}
				if err = os.WriteFile(path, body, 0600); err != nil {
					t.Fatal(err)
				}
				if _, err = r.Install(ctx, "project", "json-check"); err != nil {
					t.Fatal(err)
				}
			}
			g.preserveFrontmatter = mode != "description" && mode != "new-skill"
			if mode == "new-skill" {
				matcher.relation, matcher.suggestedName = "new_capability", "structured-json-audit"
			}
			evaluator := &autoPatchLearningEvaluator{mode: mode}
			if err := e.ConfigureBehavior(evaluator, harness.SkillBehaviorOptions{Suites: []harness.SkillBehaviorSuite{autoPatchLearningSuite()}}); err != nil {
				t.Fatal(err)
			}
			experienceTwoEpisodes(t, s, x)
			if mode == "new-skill" {
				enableNewExperienceDiscovery(t, e)
			} else {
				enableExperienceDiscovery(t, e)
			}
			if err := e.ConfigureAutoPatch(learningAutoPatchOptions(mode != "old-grant")); err != nil {
				t.Fatal(err)
			}
			ds := experienceDiscoveries(t, e)
			if len(ds) != 1 {
				t.Fatal(ds)
			}
			p := autoPatchLearningProposal(t, e, ds[0])
			wantCalls := 4
			if mode == "new-skill" {
				wantCalls = 0 // An existing target's suite cannot approve a new capability.
			}
			if evaluator.calls != wantCalls || len(g.seen) != 1 {
				t.Fatal("not separately evaluated baseline/candidate pairs", evaluator.calls, len(g.seen))
			}
			switch mode {
			case "receipt":
				if _, err := s.Store.db.Exec(`UPDATE harness_tool_receipts SET version=version+1`); err != nil {
					t.Fatal(err)
				}
			case "consent":
				control, _ := s.Store.Learning.Control(ctx, x.ID)
				if _, err := s.SetSkillLearningControl(ctx, "owner", x.ID, harness.SkillLearningControlRequest{Contribute: false, ExpectedEpoch: control.Epoch}); err != nil {
					t.Fatal(err)
				}
			case "daily-shared", "observing-same-target":
				// A preexisting nonlearning observation uses the same daily and
				// per-target limits. Isolate quota lookup without another model job.
				count, status := 2, "passed"
				if mode == "observing-same-target" {
					count, status = 1, "observing"
					if _, err := s.Store.db.Exec(`UPDATE df_skill_auto_patch_scan SET monitor_cursor='zzzz'`); err != nil {
						t.Fatal(err)
					}
				}
				for i := range count {
					// Copy the already valid proposal row solely to satisfy the
					// observation FK; it has no generation job or publish authority.
					id := fmt.Sprintf("quota-fixture-%d", i)
					if _, err := s.Store.db.Exec(`INSERT INTO df_skill_proposals(id,source_key,name,status,state_version,request_key,request_sha,payload)
 SELECT ?,source_key,name,'rejected',state_version,?,request_sha,payload FROM df_skill_proposals WHERE id=?`, id, id, p.ID); err != nil {
						t.Fatal(err)
					}
					if _, err := s.Store.db.Exec(`INSERT INTO df_skill_auto_patch_observations(proposal_id,source_key,name,status,started_ns,payload,payload_sha) VALUES(?,?,?,?,?,?,?)`, id, p.Target.SourceIdentity, p.SkillName, status, time.Now().UTC().UnixNano(), []byte("fixture"), "fixture"); err != nil {
						t.Fatal(err)
					}
				}
			case "atomic-observation":
				if _, err := s.Store.db.Exec(`CREATE TRIGGER fail_learning_observation BEFORE INSERT ON df_skill_auto_patch_observations BEGIN SELECT RAISE(ABORT,'fixture observation fault'); END`); err != nil {
					t.Fatal(err)
				}
			}
			err := e.RunAutoPatch(ctx)
			if (err != nil) != (mode == "atomic-observation") {
				t.Fatal("automatic publication error", mode, err)
			}
			got, getErr := e.Get(ctx, p.ID)
			if mode == "consent" {
				if getErr == nil {
					t.Fatal("revoked unpublished proposal survived", got)
				}
			} else if getErr != nil {
				t.Fatal(getErr)
			}
			published := mode == "on"
			if mode != "consent" && (got.Status == "published") != published {
				t.Fatal("automatic admission", mode, got)
			}
			if err := r.WithPublication(ctx, "project", p.SkillName, func(tx *skills.PublicationTx) error {
				head, err := tx.Current()
				if err != nil {
					return err
				}
				if published {
					if head.Version != p.CandidateVersion || !head.Managed {
						t.Fatal("approved version missing", head)
					}
					candidate, err := tx.ReadVersion(head.Version)
					if err != nil || !bytes.Equal(candidate.Files()["references/notes.txt"], []byte("Approved support text; must remain unchanged.\n")) {
						t.Fatal("support files changed", err)
					}
				} else if head != ds[0].Base {
					t.Fatal("ineligible automatic publication changed head", head, ds[0].Base)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var observations int
			if err := s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_auto_patch_observations WHERE proposal_id=?`, p.ID).Scan(&observations); err != nil || observations != map[bool]int{false: 0, true: 1}[published] {
				t.Fatal("observation/publication not atomic", observations, err)
			}
			if published {
				page, err := e.QueryAutoPatch(ctx, harness.SkillQuery{Limit: 10})
				if err != nil || len(page.Observations) != 1 {
					t.Fatal(page, err)
				}
				source := page.Observations[0].LearningSource
				if source == nil || source.GenerationID != ds[0].GenerationID || source.DiscoveryID != ds[0].ID || source.PatternRevision != ds[0].Evidence[0].Experience.PatternRevision || source.SourceState != "valid_at_publication" {
					t.Fatal("missing pinned publication source", source)
				}
			}
			if mode == "atomic-observation" {
				if _, err := s.Store.db.Exec(`DROP TRIGGER fail_learning_observation`); err != nil {
					t.Fatal(err)
				}
				if err := e.RunAutoPatch(ctx); err != nil {
					t.Fatal("rolled-back transaction did not recover", err)
				}
				if got, err := e.Get(ctx, p.ID); err != nil || got.Status != "published" {
					t.Fatal("recovery did not publish", got, err)
				}
			}
		})
	}
}

func TestSkillLearningAutoPatchPublishedWithdrawalRestartAndRollback(t *testing.T) {
	for _, mode := range []string{"rollback", "insufficient-evidence"} {
		t.Run(mode, func(t *testing.T) { autoPatchLearningObservation(t, mode) })
	}
}

func TestSkillLearningAutoPatchPendingRestartRechecksPublicationPolicy(t *testing.T) {
	s, x, r, e, g, v, _ := experienceDiscoveryTest(t)
	ctx := context.Background()
	adoptLearningBase(t, r)
	g.preserveFrontmatter = true
	evaluator := &autoPatchLearningEvaluator{}
	if err := e.ConfigureBehavior(evaluator, harness.SkillBehaviorOptions{Suites: []harness.SkillBehaviorSuite{autoPatchLearningSuite()}}); err != nil {
		t.Fatal(err)
	}
	experienceTwoEpisodes(t, s, x)
	enableExperienceDiscovery(t, e)
	if err := e.ConfigureAutoPatch(learningAutoPatchOptions(true)); err != nil {
		t.Fatal(err)
	}
	ds := experienceDiscoveries(t, e)
	if len(ds) != 1 {
		t.Fatal(ds)
	}
	p := autoPatchLearningProposal(t, e, ds[0])
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	// All reopens share the original pending candidate. Policy changes may
	// exclude automatic publication, never silently re-evaluate or rebase it.
	for _, mode := range []string{"gate-off", "discovery-off", "suite-changed", "discovery-policy-changed", "unchanged"} {
		t.Run(mode, func(t *testing.T) {
			next, err := evolution.New(ctx, r, v)
			if err != nil {
				t.Fatal(err)
			}
			defer next.Close()
			if err := next.ConfigureGeneration(ctx, g, consolidationTestOptions().Budget, []string{"project"}, false); err != nil {
				t.Fatal(err)
			}
			suite := autoPatchLearningSuite()
			if mode == "suite-changed" {
				suite.Cases[0].Prompt = "A newly authorized different fixture prompt"
			}
			if err := next.ConfigureBehavior(evaluator, harness.SkillBehaviorOptions{Suites: []harness.SkillBehaviorSuite{suite}}); err != nil {
				t.Fatal(err)
			}
			if err := next.ConfigureExperienceDiscoverySource(s.Store.Learning); err != nil {
				t.Fatal(err)
			}
			if mode != "discovery-off" {
				options := harness.SkillDiscoveryOptions{DisableOperatorCorrections: true, ExperiencePatternsEnabled: true, RepeatThreshold: 2, RepeatWindowDays: 30, CooldownHours: 24, MaxDailyProposals: 5, MaxPendingProposals: 20}
				if mode == "discovery-policy-changed" {
					options.RepeatWindowDays = 29
				}
				if err := next.ConfigureDiscovery(options); err != nil {
					t.Fatal(err)
				}
			}
			if err := next.ConfigureAutoPatch(learningAutoPatchOptions(mode != "gate-off" && mode != "discovery-off")); err != nil {
				t.Fatal(err)
			}
			if err := next.RunAutoPatch(ctx); err != nil {
				t.Fatal(err)
			}
			got, err := next.Get(ctx, p.ID)
			if err != nil || (got.Status == "published") != (mode == "unchanged") || len(g.seen) != 1 || evaluator.calls != 4 {
				t.Fatal("restart bypassed pinned publication policy or recharged candidate", mode, got, err, len(g.seen), evaluator.calls)
			}
		})
	}
}

func autoPatchLearningObservation(t *testing.T, mode string) {
	s, x, r, e, g, v, _ := experienceDiscoveryTest(t)
	ctx := context.Background()
	adoptLearningBase(t, r)
	g.preserveFrontmatter = true
	evaluator := &autoPatchLearningEvaluator{}
	if err := e.ConfigureBehavior(evaluator, harness.SkillBehaviorOptions{Suites: []harness.SkillBehaviorSuite{autoPatchLearningSuite()}}); err != nil {
		t.Fatal(err)
	}
	experienceTwoEpisodes(t, s, x)
	enableExperienceDiscovery(t, e)
	if err := e.ConfigureAutoPatch(learningAutoPatchOptions(true)); err != nil {
		t.Fatal(err)
	}
	ds := experienceDiscoveries(t, e)
	if len(ds) != 1 {
		t.Fatal(ds)
	}
	p := autoPatchLearningProposal(t, e, ds[0])
	if err := e.RunAutoPatch(ctx); err != nil {
		t.Fatal(err)
	}
	page, err := e.QueryAutoPatch(ctx, harness.SkillQuery{Limit: 10})
	if err != nil || len(page.Observations) != 1 {
		t.Fatal(page, err)
	}
	o := page.Observations[0]
	var read skills.ReadObservation
	snapshot, err := r.Snapshot(ctx, harness.SkillSelection{Workspace: x.CWD})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.WithReadObserver(func(_ context.Context, value skills.ReadObservation) error { read = value; return nil }).Get(ctx, p.SkillName); err != nil {
		t.Fatal(err)
	}
	control, _ := s.Store.Learning.Control(ctx, x.ID)
	if _, err := s.SetSkillLearningControl(ctx, "owner", x.ID, harness.SkillLearningControlRequest{Contribute: false, ExpectedEpoch: control.Epoch}); err != nil {
		t.Fatal(err)
	}
	if got, err := e.Get(ctx, p.ID); err != nil || got.Status != "published" {
		t.Fatal("withdrawal removed adopted proposal", got, err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := evolution.New(ctx, r, v)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	if err := restarted.ConfigureGeneration(ctx, g, consolidationTestOptions().Budget, []string{"project"}, false); err != nil {
		t.Fatal(err)
	}
	if err := restarted.ConfigureBehavior(evaluator, harness.SkillBehaviorOptions{Suites: []harness.SkillBehaviorSuite{autoPatchLearningSuite()}}); err != nil {
		t.Fatal(err)
	}
	// Learning source and its publication gate are disabled, but saved quality
	// observation/rollback is independent and remains active under auto_patch.
	if err := restarted.ConfigureAutoPatch(learningAutoPatchOptions(false)); err != nil {
		t.Fatal(err)
	}
	if err := restarted.RunAutoPatch(ctx); err != nil {
		t.Fatal(err)
	}
	page, err = restarted.QueryAutoPatch(ctx, harness.SkillQuery{Limit: 10})
	if err != nil || page.Observations[0].LearningSource.SourceState != "withdrawn" || page.Observations[0].Failures != 0 || page.Observations[0].Status != "observing" {
		t.Fatal("withdrawal fabricated quality vote or stopped observation", page, err)
	}
	if mode == "insufficient-evidence" {
		// Advance only the observation fixture clock in its saved payload, not
		// authority receipts, model records or effective user configuration.
		expired := page.Observations[0]
		expired.StartedAt = time.Now().UTC().Add(-73 * time.Hour)
		expired.Deadline = expired.StartedAt.Add(72 * time.Hour)
		expired.UpdatedAt = expired.StartedAt
		data, err := json.Marshal(expired)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Store.db.Exec(`UPDATE df_skill_auto_patch_observations SET started_ns=?,payload=?,payload_sha=? WHERE proposal_id=?`, expired.StartedAt.UnixNano(), data, fmt.Sprintf("%x", sha256.Sum256(data)), p.ID); err != nil {
			t.Fatal(err)
		}
		if err := restarted.RunAutoPatch(ctx); err != nil {
			t.Fatal(err)
		}
		page, err := restarted.QueryAutoPatch(ctx, harness.SkillQuery{Limit: 10})
		if err != nil || len(page.Observations) != 1 || page.Observations[0].Status != "insufficient_evidence" || page.Observations[0].Failures != 0 || page.Observations[0].Successes != 0 || page.Observations[0].RollbackRevision != 0 {
			t.Fatal("no-feedback learning observation claimed quality or rollback", page, err)
		}
		return
	}
	for i := range 2 {
		next, err := s.NewSession(ctx, "owner", x.CWD)
		if err != nil {
			t.Fatal(err)
		}
		req := learningTestRun(t, s, next, fmt.Sprintf("Confirm new version quality %d", i))
		// Digest-only runtime read envelope, as in collector fixtures. No real
		// model or source text is needed to verify monitoring integration.
		if _, err := s.Store.db.Exec(`INSERT INTO harness_skill_reads(run_id,session_id,root_id,attempt_id,role_id,source_id,source_key,scope,name,version,path,file_hash,bytes,kind,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			req.RunID, next.ID, req.RootBudgetID, req.RunID, "", p.Target.SourceID, p.Target.SourceIdentity, p.Target.Scope, p.SkillName, p.CandidateVersion, read.File.Path, read.File.Hash, read.File.Bytes, "skill_load", o.StartedAt.Add(time.Duration(i+1)*time.Millisecond).Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
		learningTestFinish(t, s, req, "end_turn")
		for range 10 {
			worked, err := restarted.CollectEvidence(ctx, 100)
			if err != nil {
				t.Fatal(err)
			}
			if !worked {
				break
			}
		}
		signals, err := restarted.QuerySignals(ctx, harness.SkillQuery{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		var signal harness.SkillSignal
		for _, item := range signals.Signals {
			if item.RootID == req.RootBudgetID {
				signal = item
			}
		}
		evidence, err := restarted.SignalEvidence(ctx, signal.ID, "", 10)
		if err != nil || len(evidence.Reads) != 1 {
			t.Fatal("missing real read attribution", evidence, err)
		}
		if _, err := restarted.SubmitFeedback(ctx, harness.SkillFeedbackRequest{SignalID: signal.ID, ExpectedSignalStateVersion: signal.StateVersion, RequestKey: fmt.Sprintf("confirm-quality-%d", i), Kind: "quality_failure", Summary: "Confirmed incorrect structured check", ReadIDs: []int64{evidence.Reads[0].ID}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := restarted.RunAutoPatch(ctx); err != nil {
		t.Fatal(err)
	}
	page, err = restarted.QueryAutoPatch(ctx, harness.SkillQuery{Limit: 10})
	if err != nil || page.Observations[0].Status != "rolled_back" || page.Observations[0].Failures != 2 || page.Observations[0].LearningSource.SourceState != "withdrawn" {
		t.Fatal("confirmed regressions failed to roll back adopted learning patch", page, err)
	}
	if err := r.WithPublication(ctx, "project", "json-check", func(tx *skills.PublicationTx) error {
		head, err := tx.Current()
		if err == nil && head.Version != p.Base.Version {
			t.Fatal("wrong rollback baseline", head)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before := page.Observations[0].RollbackRevision
	if err := restarted.RunAutoPatch(ctx); err != nil {
		t.Fatal(err)
	}
	page, err = restarted.QueryAutoPatch(ctx, harness.SkillQuery{Limit: 10})
	if err != nil || page.Observations[0].RollbackRevision != before || len(g.seen) != 1 || evaluator.calls != 4 {
		t.Fatal("restart/monitor replay regenerated or republished", page, err, len(g.seen), evaluator.calls)
	}
}
