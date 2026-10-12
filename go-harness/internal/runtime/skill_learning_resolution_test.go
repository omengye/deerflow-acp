package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/learning"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills/evolution"
)

func TestSkillLearningLaterVerificationAutomaticallyResolvesOriginalTask(t *testing.T) {
	s, session, registry, extractor, matcher := newMatchingTest(t)
	ctx := context.Background()
	generator, validator := &experienceDiscoveryGenerator{}, &experienceDiscoveryValidator{}
	e, err := evolution.New(ctx, registry, validator)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err = e.ConfigureGeneration(ctx, generator, consolidationTestOptions().Budget, []string{"project"}, false); err != nil {
		t.Fatal(err)
	}
	if err = e.ConfigureExperienceDiscoverySource(s.Store.Learning); err != nil {
		t.Fatal(err)
	}
	enableExperienceReviewDrafts(t, e, false)
	extractor.observed = false
	first := learningTestRun(t, s, session, "Inspect the complete JSON configuration and required fields")
	learningTestFinish(t, s, first, "end_turn")
	learningTestQueue(t, s)
	matchingRun(t, s)
	consolidateRun(t, s)
	before, err := s.Store.Learning.Patterns(ctx, session.ID, "", 10)
	if err != nil || len(before.Patterns) != 1 || before.Patterns[0].EvidenceState != "unresolved" || before.Patterns[0].ObservedEpisodes != 0 {
		t.Fatal("unverified input unexpectedly authorized evolution", before, err)
	}
	original := before.Patterns[0].ExperienceIDs[0]
	extractor.observed = true
	consolidateExtract(t, s, session, "Correct the incomplete inspection: read full JSON text and verify required fields")
	consolidateRun(t, s)
	after, err := s.Store.Learning.Patterns(ctx, session.ID, "", 10)
	if err != nil || len(after.Patterns) != 1 || after.Patterns[0].ID != before.Patterns[0].ID || after.Patterns[0].Revision != 2 ||
		after.Patterns[0].EvidenceState != "insufficient_evidence" || after.Patterns[0].SupportEpisodes != 1 || after.Patterns[0].ObservedEpisodes != 1 || len(after.Patterns[0].ExperienceIDs) != 2 {
		t.Fatal("later source-verified correction remained permanently unresolved or fabricated independent votes", after, err)
	}
	match := experienceMatch(t, s)
	if _, err = s.Store.Learning.Promotion(ctx, match, 2, time.Now().Add(-time.Hour), time.Now()); err == nil {
		t.Fatal("resolved same-task history authorized automatic publication")
	}
	discoveries := experienceDiscoveries(t, e)
	if len(discoveries) != 1 || !discoveries[0].ReviewDraft || len(discoveries[0].Evidence) != 1 || len(matcher.seen) != 1 {
		t.Fatal("resolved task did not automatically reach review-only drafting", discoveries, matcher.seen)
	}
	proposal := experienceGenerate(t, e)
	if proposal.Status != "pending_review" || len(generator.seen) != 1 {
		t.Fatal("resolved task required manual evolution", proposal, generator.seen)
	}
	published, err := e.Approve(ctx, proposal.ID, harness.SkillReview{ExpectedStateVersion: proposal.StateVersion, Actor: "owner"})
	if err != nil || published.Status != "published" {
		t.Fatal("generated resolved-task proposal could not be reviewed", published, err)
	}
	sourceRequest := harness.SkillLearningSourceRequest{Kind: "pattern", ID: after.Patterns[0].ID, ExpectedRevision: 2, Scope: "session", ExperienceID: original}
	source, err := s.Store.Learning.Source(ctx, session.ID, sourceRequest)
	if err != nil || source.Quality != "unassessed" || len(source.References) != 1 || source.References[0].RunID != first.RunID || source.References[0].Kind != "user" {
		t.Fatal("resolved history lost original attribution", source, err)
	}
	if err = s.Store.Learning.Close(); err != nil {
		t.Fatal(err)
	}
	disabled, err := learning.New(ctx, s.Store.db, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = disabled.Close() })
	s.Store.Learning = disabled
	page, err := disabled.Patterns(ctx, session.ID, "", 10)
	if err != nil || len(page.Patterns) != 1 || page.Patterns[0].EvidenceState != "insufficient_evidence" || page.Patterns[0].SourceState != "not_revalidated" {
		t.Fatal("disabled restart lost verified historical projection", page, err)
	}
	if got, err := disabled.Source(ctx, session.ID, sourceRequest); err != nil || len(got.References) != 1 || got.References[0].ID != source.References[0].ID {
		t.Fatal("disabled restart lost historical source inspection", got, err)
	}
}

type learningResolutionExtractor struct {
	base   *consolidationTestExtractor
	filter string
}

func (e *learningResolutionExtractor) Identity() string { return e.base.Identity() }
func (e *learningResolutionExtractor) Extract(ctx context.Context, input harness.SkillLearningInput) ([]harness.SkillExperienceCandidate, error) {
	candidates, err := e.base.Extract(ctx, input)
	if err != nil || !e.base.observed || len(candidates) != 1 {
		return candidates, err
	}
	var latest int64
	for _, ref := range input.References {
		if ref.Kind == "user" {
			latest = max(latest, ref.Sequence)
		}
	}
	refs := map[string]harness.SkillLearningReference{}
	for _, ref := range input.References {
		refs[ref.ID] = ref
	}
	filtered := []string{}
	for _, id := range candidates[0].References {
		ref := refs[id]
		if e.filter == "latest-user-only" && ref.Kind == "user" && ref.Sequence != latest || e.filter == "original-user-only" && ref.Kind == "user" && ref.Sequence == latest {
			continue
		}
		filtered = append(filtered, id)
	}
	candidates[0].References = filtered
	return candidates, nil
}

func TestSkillLearningLaterVerificationCannotResolveUncoveredOrLaterIssues(t *testing.T) {
	for _, mode := range []string{"latest-user-only", "original-user-only", "old-receipt-only", "different-episode", "later-unresolved", "conflicting"} {
		t.Run(mode, func(t *testing.T) {
			s, x, registry, base, matcher := newMatchingTest(t)
			ctx := context.Background()
			base.observed = false
			extractor := &learningResolutionExtractor{base: base}
			model := &consolidationTestModel{}
			learningInFlightRestart(t, s, registry, extractor, model, matcher)
			consolidateExtract(t, s, x, "Unverified full configuration check")
			consolidateRun(t, s)
			base.observed = true
			if mode == "latest-user-only" || mode == "original-user-only" {
				extractor.filter = mode
			}
			if mode == "different-episode" {
				if _, err := s.StartSkillLearningEpisode(ctx, "owner", x.ID); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "conflicting" {
				model.relation = "conflicting"
			}
			if mode == "old-receipt-only" {
				req := learningTestRun(t, s, x, "Correct prior issue but do not perform a new verification")
				learningTestFinish(t, s, req, "end_turn")
				learningTestQueue(t, s)
				matchingRun(t, s)
			} else {
				consolidateExtract(t, s, x, "Correct prior issue with a complete configuration check")
			}
			consolidateRun(t, s)
			if mode == "later-unresolved" {
				base.observed = false
				consolidateExtract(t, s, x, "There is a new unverified issue after the completed correction")
				consolidateRun(t, s)
			}
			page, err := s.Store.Learning.Patterns(ctx, x.ID, "", 10)
			wantCount, wantState := 1, "unresolved"
			if mode == "conflicting" {
				wantCount, wantState = 2, "conflicting"
			}
			if err != nil || len(page.Patterns) != wantCount {
				t.Fatal(page, err)
			}
			for _, p := range page.Patterns {
				if p.EvidenceState != wantState {
					t.Fatal("uncovered original or later issue disappeared", p)
				}
			}
			for range 8 {
				if _, err = s.Store.Learning.QueueMatchOne(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if len(matcher.seen) != 0 {
				t.Fatal("blocked pattern reached matching model", matcher.seen)
			}
			var queued int
			if err = s.Store.db.QueryRow(`SELECT count(*) FROM df_learning_matches`).Scan(&queued); err != nil || queued != 0 {
				t.Fatal("blocked pattern created a paid match", queued, err)
			}
		})
	}
}

func TestSkillLearningResolvedTaskCheckpointRevalidatesLinksAndLiveSources(t *testing.T) {
	for _, mode := range []string{"valid-restart", "rehashed-link", "rehashed-policy", "removed-link", "receipt-changed", "original-event-changed"} {
		t.Run(mode, func(t *testing.T) {
			s, x, registry, extractor, matcher := newMatchingTest(t)
			ctx := context.Background()
			extractor.observed = false
			first := consolidateExtract(t, s, x, "Unverified original JSON check")
			consolidateRun(t, s)
			extractor.observed = true
			later := consolidateExtract(t, s, x, "Correct the original JSON check with complete text")
			consolidateRun(t, s)
			var jobID string
			if err := s.Store.db.QueryRow(`SELECT job_id FROM df_learning_consolidations ORDER BY rowid DESC LIMIT 1`).Scan(&jobID); err != nil {
				t.Fatal(err)
			}
			var raw []byte
			if err := s.Store.db.QueryRow(`SELECT payload FROM df_learning_pattern_revisions WHERE revision=2`).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var snap struct {
				Resolutions []struct{ Prior, Verified string }
			}
			if json.Unmarshal(raw, &snap) != nil || len(snap.Resolutions) != 1 {
				t.Fatal("resolution link was not durably saved", string(raw))
			}
			switch mode {
			case "rehashed-link", "rehashed-policy", "removed-link":
				var payload map[string]any
				if err := json.Unmarshal(raw, &payload); err != nil {
					t.Fatal(err)
				}
				if mode == "rehashed-link" {
					payload["resolutions"] = []any{map[string]string{"prior": snap.Resolutions[0].Prior, "verified": snap.Resolutions[0].Prior}}
				} else if mode == "removed-link" {
					delete(payload, "resolutions")
				} else {
					payload["resolution_policy"] = "future-unchecked"
				}
				changed, _ := json.Marshal(payload)
				sha := fmt.Sprintf("%x", sha256.Sum256(changed))
				if _, err := s.Store.db.Exec(`UPDATE df_learning_pattern_revisions SET payload=?,payload_sha=? WHERE revision=2`, changed, sha); err != nil {
					t.Fatal(err)
				}
			case "receipt-changed":
				if _, err := s.Store.db.Exec(`UPDATE harness_tool_receipts SET receipt=json_set(CAST(receipt AS TEXT),'$.result[0].text','changed-local-evidence') WHERE run_id=?`, later.RunID); err != nil {
					t.Fatal(err)
				}
			case "original-event-changed":
				if _, err := s.Store.Append(ctx, harness.RunEvent{SessionID: x.ID, RunID: first.RunID, Kind: "text_delta", AgentName: "deerflow", Text: "late source mutation"}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.Store.db.Exec(`UPDATE df_skill_maintenance_jobs SET state='queued',token='',lease_ns=0,retry_ns=0 WHERE id=?`, jobID); err != nil {
				t.Fatal(err)
			}
			model := &consolidationTestModel{}
			learningInFlightRestart(t, s, registry, extractor, model, matcher)
			matchingRun(t, s)
			job, err := skillmaintenance.ReadJob(ctx, s.Store.db, jobID)
			wantState := "failed"
			if mode == "valid-restart" {
				wantState = "completed"
			}
			if err != nil || job.State != wantState || len(model.seen) != 0 || len(matcher.seen) != 0 {
				t.Fatal("resolved checkpoint bypassed source guards or repeated model work", job, model.seen, matcher.seen, err)
			}
		})
	}
}

func TestSkillLearningResolutionRehashedUnsupportedLinkCannotCertifyHistory(t *testing.T) {
	s, x, registry, base, matcher := newMatchingTest(t)
	ctx := context.Background()
	base.observed = false
	extractor := &learningResolutionExtractor{base: base}
	learningInFlightRestart(t, s, registry, extractor, &consolidationTestModel{}, matcher)
	consolidateExtract(t, s, x, "Unverified original check")
	consolidateRun(t, s)
	page, err := s.Store.Learning.Patterns(ctx, x.ID, "", 10)
	if err != nil || len(page.Patterns) != 1 {
		t.Fatal(page, err)
	}
	prior := page.Patterns[0].ExperienceIDs[0]
	base.observed, extractor.filter = true, "latest-user-only"
	consolidateExtract(t, s, x, "Different observed check that does not cite the original issue")
	consolidateRun(t, s)
	var raw []byte
	if err = s.Store.db.QueryRow(`SELECT payload FROM df_learning_pattern_revisions WHERE revision=2`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err = json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	pattern := payload["pattern"].(map[string]any)
	if pattern["evidence_state"] != "unresolved" {
		t.Fatal("fixture did not preserve unsupported original issue", pattern)
	}
	var verified string
	for _, member := range payload["members"].([]any) {
		id := member.(map[string]any)["id"].(string)
		if id != prior {
			verified = id
		}
	}
	payload["resolutions"] = []any{map[string]string{"prior": prior, "verified": verified}}
	pattern["evidence_state"] = "insufficient_evidence"
	changed, _ := json.Marshal(payload)
	if _, err = s.Store.db.Exec(`UPDATE df_learning_pattern_revisions SET payload=?,payload_sha=? WHERE revision=2`, changed, fmt.Sprintf("%x", sha256.Sum256(changed))); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Store.Learning.Patterns(ctx, x.ID, "", 10); err == nil {
		t.Fatal("structurally valid rehashed link bypassed fixed source coverage")
	}
	for range 8 {
		if _, err = s.Store.Learning.QueueMatchOne(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var matches int
	if err = s.Store.db.QueryRow(`SELECT count(*) FROM df_learning_matches`).Scan(&matches); err != nil || matches != 0 || len(matcher.seen) != 0 {
		t.Fatal("unsupported rehashed resolution authorized paid matching", matches, matcher.seen, err)
	}
}

func TestSkillLearningLaterDelegatedVerificationResolvesOriginalTask(t *testing.T) {
	s, x, registry, extractor, matcher := newMatchingTest(t)
	ctx := context.Background()
	x = learningDelegationSession(t, s, x)
	native := learningDelegationNative(t, s)
	extractor.observed = false
	first := learningTestRun(t, s, x, "Incomplete JSON check without verification")
	learningTestFinish(t, s, first, "end_turn")
	learningTestQueue(t, s)
	matchingRun(t, s)
	consolidateRun(t, s)
	extractor.observed = true
	parent := learningTestRun(t, s, x, "Correct the original check using a delegated complete local verification")
	child := learningDelegationCreate(t, s, native, parent, "later-resolution")
	learningTestFinish(t, s, parent, "end_turn")
	learningDelegationComplete(t, child, "read_file")
	learningTestQueue(t, s)
	matchingRun(t, s)
	consolidateRun(t, s)
	page, err := s.Store.Learning.Patterns(ctx, x.ID, "", 10)
	if err != nil || len(page.Patterns) != 1 || page.Patterns[0].EvidenceState != "insufficient_evidence" || page.Patterns[0].SupportEpisodes != 1 || page.Patterns[0].ObservedEpisodes != 1 || len(page.Patterns[0].ExperienceIDs) != 2 {
		t.Fatal("later delegated verification left the same original task permanently blocked", page, err)
	}
	experienceMatch(t, s)
	generator := &experienceDiscoveryGenerator{}
	e, err := evolution.New(ctx, registry, &experienceDiscoveryValidator{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err = e.ConfigureGeneration(ctx, generator, consolidationTestOptions().Budget, []string{"project"}, false); err != nil {
		t.Fatal(err)
	}
	if err = e.ConfigureExperienceDiscoverySource(s.Store.Learning); err != nil {
		t.Fatal(err)
	}
	enableExperienceReviewDrafts(t, e, false)
	discoveries := experienceDiscoveries(t, e)
	if len(discoveries) != 1 || !discoveries[0].ReviewDraft || len(discoveries[0].Evidence) != 1 || discoveries[0].Evidence[0].RootID != parent.RootBudgetID || len(matcher.seen) != 1 {
		t.Fatal("delegated resolution lost the later human-root observation", discoveries, matcher.seen)
	}
	p := experienceGenerate(t, e)
	if p, err = e.Approve(ctx, p.ID, harness.SkillReview{ExpectedStateVersion: p.StateVersion, Actor: "owner"}); err != nil || p.Status != "published" || len(generator.seen) != 1 {
		t.Fatal("delegated resolution required manual skill evolution", p, generator.seen, err)
	}
}

func TestSkillLearningResolvedTaskWithdrawalCleansGeneratedDraft(t *testing.T) {
	s, x, registry, extractor, _ := newMatchingTest(t)
	ctx := context.Background()
	extractor.observed = false
	consolidateExtract(t, s, x, "Unverified original full-text task")
	consolidateRun(t, s)
	extractor.observed = true
	consolidateExtract(t, s, x, "Correct the original full-text task and verify fields")
	consolidateRun(t, s)
	experienceMatch(t, s)
	generator, validator := &experienceDiscoveryGenerator{}, &experienceDiscoveryValidator{}
	e, err := evolution.New(ctx, registry, validator)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err = e.ConfigureGeneration(ctx, generator, consolidationTestOptions().Budget, []string{"project"}, false); err != nil {
		t.Fatal(err)
	}
	if err = e.ConfigureExperienceDiscoverySource(s.Store.Learning); err != nil {
		t.Fatal(err)
	}
	enableExperienceReviewDrafts(t, e, false)
	discoveries := experienceDiscoveries(t, e)
	if len(discoveries) != 1 {
		t.Fatal(discoveries)
	}
	p := experienceGenerate(t, e)
	var charges int
	if err = s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_maintenance_calls`).Scan(&charges); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetSkillLearningControl(ctx, "owner", x.ID, harness.SkillLearningControlRequest{ExpectedEpoch: 1, Contribute: false}); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Approve(ctx, p.ID, harness.SkillReview{ExpectedStateVersion: p.StateVersion, Actor: "owner"}); err == nil {
		t.Fatal("withdrawn resolved task was published")
	}
	var patterns, proposals, after, consumed int
	if err = s.Store.db.QueryRow(`SELECT count(*) FROM df_learning_pattern_revisions`).Scan(&patterns); err != nil {
		t.Fatal(err)
	}
	if err = s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_proposals WHERE id=?`, p.ID).Scan(&proposals); err != nil {
		t.Fatal(err)
	}
	if err = s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_maintenance_calls`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if err = s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_discovery_episode_consumed`).Scan(&consumed); err != nil || patterns != 0 || proposals != 0 || after != charges || consumed != 1 {
		t.Fatal("resolved historical dependency escaped revocation or refunded consumed work", patterns, proposals, after, charges, consumed, err)
	}
}

func TestSkillLearningSharedLineageCannotResolveAnotherConversationsHistory(t *testing.T) {
	s, x, _, _ := newWorkspaceLearningTest(t, false)
	ctx := context.Background()
	if err := s.Store.Learning.Close(); err != nil {
		t.Fatal(err)
	}
	options := consolidationTestOptions()
	options.CrossSession = true
	l, err := learning.New(ctx, s.Store.db, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	extractor, model := &consolidationTestExtractor{}, &consolidationTestModel{}
	if err = l.Configure(ctx, extractor, nil); err != nil {
		t.Fatal(err)
	}
	if err = l.ConfigureConsolidation(model); err != nil {
		t.Fatal(err)
	}
	s.Store.Learning = l
	sourceControl := workspaceLearningConsent(t, s, x, true)
	consolidateExtract(t, s, x, "Original unverified full configuration task")
	consolidateRun(t, s)
	page, err := l.WorkspacePatterns(ctx, x.ID, "", 10)
	if err != nil || len(page.Patterns) != 1 {
		t.Fatal(page, err)
	}
	linked := workspaceLearningSession(t, s, x.CWD)
	control := workspaceLearningConsent(t, s, linked, true)
	if _, err = l.LinkEpisodeLineage(ctx, linked.ID, harness.SkillLearningLineageRequest{SourceSessionID: x.ID, SourceEpisodeID: page.Patterns[0].EpisodeIDs[0], ExpectedEpoch: control.Epoch, ExpectedSourceEpoch: sourceControl.Epoch}); err != nil {
		t.Fatal(err)
	}
	extractor.observed = true
	consolidateExtract(t, s, linked, "A copied task completed in another conversation")
	consolidateRun(t, s)
	page, err = l.WorkspacePatterns(ctx, x.ID, "", 10)
	if err != nil || len(page.Patterns) != 1 || page.Patterns[0].EvidenceState != "unresolved" || page.Patterns[0].SupportEpisodes != 1 || page.Patterns[0].ObservedEpisodes != 1 || len(page.Patterns[0].ExperienceIDs) != 2 {
		t.Fatal("shared origin certified another conversation or added independent votes", page, err)
	}
	consolidateExtract(t, s, x, "Correct the original task in its actual conversation with full text")
	consolidateRun(t, s)
	page, err = l.WorkspacePatterns(ctx, linked.ID, "", 10)
	if err != nil || len(page.Patterns) != 1 || page.Patterns[0].EvidenceState != "insufficient_evidence" || page.Patterns[0].SupportEpisodes != 1 || page.Patterns[0].ObservedEpisodes != 1 || len(page.Patterns[0].ExperienceIDs) != 3 {
		t.Fatal("actual original task correction failed to resolve its own history", page, err)
	}
	shared := false
	if _, err = l.SetControl(ctx, x.ID, harness.SkillLearningControlRequest{Contribute: true, ExpectedEpoch: sourceControl.Epoch, WorkspaceShared: &shared}); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err = s.Store.db.QueryRow(`SELECT count(*) FROM df_learning_pattern_revisions`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatal("resolved shared history escaped transitive source withdrawal", remaining, err)
	}
}

func learningResolutionLegacyQueued(t *testing.T, s *Service) string {
	t.Helper()
	ctx := context.Background()
	var jobID, id, policy, model string
	var data []byte
	if err := s.Store.db.QueryRow(`SELECT job_id,id,payload,policy_version,model_identity FROM df_learning_consolidations ORDER BY rowid DESC LIMIT 1`).Scan(&jobID, &id, &data, &policy, &model); err != nil {
		t.Fatal(err)
	}
	// Recreate the pre-policy field order; JSON object order is part of the
	// existing digest, so a map re-encoding is not a legacy snapshot.
	var ordered struct {
		ID                 string                                    `json:"id"`
		Session            string                                    `json:"session"`
		Owner              string                                    `json:"owner"`
		Shared             bool                                      `json:"workspace_shared,omitempty"`
		Seed               json.RawMessage                           `json:"seed"`
		Patterns           json.RawMessage                           `json:"patterns"`
		View               harness.SkillExperienceConsolidationInput `json:"view"`
		MaxDailyModelCalls int64                                     `json:"max_daily_model_calls"`
		MaxDailyTokens     int64                                     `json:"max_daily_tokens"`
	}
	if err := json.Unmarshal(data, &ordered); err != nil {
		t.Fatal(err)
	}
	ordered.ID = ""
	identity, _ := json.Marshal([]any{policy, model, ordered})
	legacyID := "lcon_" + fmt.Sprintf("%x", sha256.Sum256(identity))
	ordered.ID = legacyID
	payload, _ := json.Marshal(ordered)
	sha := fmt.Sprintf("%x", sha256.Sum256(payload))
	job, err := skillmaintenance.ReadJob(ctx, s.Store.db, jobID)
	if err != nil || job.State != "queued" {
		t.Fatal(job, err)
	}
	key, _ := json.Marshal([]string{string(job.Request.Kind), string(job.Request.OwnerKind), job.Request.OwnerKey, legacyID})
	storageKey := "learning/" + fmt.Sprintf("%x", sha256.Sum256(key))
	tx, err := s.Store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE df_learning_consolidations SET id=?,payload=?,payload_sha=? WHERE id=?`, legacyID, payload, sha, id); err == nil {
		_, err = tx.ExecContext(ctx, `UPDATE df_skill_maintenance_jobs SET reference=?,request_key=?,owner_request_key=?,input_digest=? WHERE id=?`, legacyID, storageKey, legacyID, sha, jobID)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return jobID
}

func TestSkillLearningResolutionLegacyJobsKeepFixedSemanticsUntilNewInput(t *testing.T) {
	s, x, registry, extractor, matcher := newMatchingTest(t)
	ctx := context.Background()
	extractor.observed = false
	consolidateExtract(t, s, x, "Unverified legacy full configuration check")
	consolidateQueued(t, s)
	learningResolutionLegacyQueued(t, s)
	matchingRun(t, s)
	extractor.observed = true
	consolidateExtract(t, s, x, "Completed legacy correction with full text")
	consolidateQueued(t, s)
	jobID := learningResolutionLegacyQueued(t, s)
	matchingRun(t, s)
	var raw []byte
	if err := s.Store.db.QueryRow(`SELECT payload FROM df_learning_pattern_revisions WHERE revision=2`).Scan(&raw); err != nil || strings.Contains(string(raw), `"resolution_policy"`) || strings.Contains(string(raw), `"resolutions"`) {
		t.Fatal("old paid output silently acquired new resolution semantics", string(raw), err)
	}
	page, err := s.Store.Learning.Patterns(ctx, x.ID, "", 10)
	if err != nil || len(page.Patterns) != 1 || page.Patterns[0].EvidenceState != "unresolved" {
		t.Fatal("legacy projection was upgraded without new input", page, err)
	}
	if _, err = s.Store.db.Exec(`UPDATE df_skill_maintenance_jobs SET state='queued',token='',lease_ns=0,retry_ns=0 WHERE id=?`, jobID); err != nil {
		t.Fatal(err)
	}
	model := &consolidationTestModel{}
	learningInFlightRestart(t, s, registry, extractor, model, matcher)
	matchingRun(t, s)
	job, err := skillmaintenance.ReadJob(ctx, s.Store.db, jobID)
	if err != nil || job.State != "completed" || len(model.seen) != 0 {
		t.Fatal("legacy saved result was invalidated or regenerated", job, model.seen, err)
	}
	consolidateExtract(t, s, x, "Another ordinary followup verifies the complete original task")
	consolidateRun(t, s)
	page, err = s.Store.Learning.Patterns(ctx, x.ID, "", 10)
	if err != nil || len(page.Patterns) != 1 || page.Patterns[0].Revision != 3 || page.Patterns[0].EvidenceState != "insufficient_evidence" || page.Patterns[0].SupportEpisodes != 1 || page.Patterns[0].ObservedEpisodes != 1 || len(page.Patterns[0].ExperienceIDs) != 3 {
		t.Fatal("new real input could not evolve the original legacy pattern", page, err)
	}
	if err = s.Store.db.QueryRow(`SELECT payload FROM df_learning_pattern_revisions WHERE revision=2`).Scan(&raw); err != nil || strings.Contains(string(raw), `"resolution_policy"`) {
		t.Fatal("new revision rewrote legacy history", string(raw), err)
	}
	experienceMatch(t, s)
	if len(matcher.seen) != 1 {
		t.Fatal("new revision required manual rematching", matcher.seen)
	}
}
