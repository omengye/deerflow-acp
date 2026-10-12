package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/learning"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

func learningReviewPattern(t *testing.T, s *Service, x harness.Session) harness.SkillExperiencePattern {
	t.Helper()
	page, err := s.Store.Learning.Patterns(context.Background(), x.ID, "", 10)
	if err != nil || len(page.Patterns) != 1 || page.Patterns[0].Review == nil {
		t.Fatal(page, err)
	}
	return page.Patterns[0]
}

func learningAdjudicationRequest(t *testing.T, r *skills.Registry, p harness.SkillExperiencePattern, name, key string) harness.SkillLearningPatternAdjudicationRequest {
	t.Helper()
	request := harness.SkillLearningPatternAdjudicationRequest{PatternID: p.ID, ExpectedPatternRevision: p.Revision,
		ExpectedStateVersion: p.Review.StateVersion, Scope: "session", RequestKey: key, Relation: "body_gap", Reason: "Explicit host target review token=PRIVATE_HOST_REASON"}
	if err := r.WithPublication(context.Background(), "project", name, func(pub *skills.PublicationTx) error {
		request.Target = pub.Target()
		var err error
		request.ExpectedHead, err = pub.Current()
		if err == nil {
			request.OverlayBase, err = pub.OverlayBase()
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return request
}

func TestSkillLearningReviewIgnoreIsAtomicScopedAndDoesNotRefund(t *testing.T) {
	s, x, r, e, g, _, matcher := experienceDiscoveryTest(t)
	ctx := context.Background()
	id := experienceTwoEpisodes(t, s, x)
	enableExperienceDiscovery(t, e)
	ds := experienceDiscoveries(t, e)
	if len(ds) != 1 {
		t.Fatal(ds)
	}
	proposal := experienceGenerate(t, e)
	p := learningReviewPattern(t, s, x)
	request := harness.SkillLearningPatternIgnoreRequest{PatternID: p.ID, ExpectedPatternRevision: p.Revision,
		ExpectedStateVersion: 0, Scope: "session", RequestKey: "ignore-original", Ignored: true}
	var consumedBefore, callsBefore int
	if err := s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_discovery_episode_consumed`).Scan(&consumedBefore); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_maintenance_calls`).Scan(&callsBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.db.Exec(`CREATE TRIGGER learning_review_receipt_fail BEFORE INSERT ON df_learning_review_receipts BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.Learning.SetPatternIgnored(ctx, x.ID, request); err == nil {
		t.Fatal("receipt failure committed partial ignore")
	}
	if got := learningReviewPattern(t, s, x); got.Review.Ignored || got.Review.StateVersion != 0 {
		t.Fatal("review changed despite rollback", got.Review)
	}
	if _, err := e.Get(ctx, proposal.ID); err != nil {
		t.Fatal("failed ignore erased original proposal", err)
	}
	if _, err := s.Store.db.Exec(`DROP TRIGGER learning_review_receipt_fail`); err != nil {
		t.Fatal(err)
	}
	foreign := workspaceLearningSession(t, s, x.CWD)
	if _, err := s.Store.Learning.SetPatternIgnored(ctx, foreign.ID, request); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("same directory granted private review", err)
	}
	result, err := s.Store.Learning.SetPatternIgnored(ctx, x.ID, request)
	if err != nil || !result.Review.Ignored || result.Review.StateVersion != 1 {
		t.Fatal(result, err)
	}
	replay, err := s.Store.Learning.SetPatternIgnored(ctx, x.ID, request)
	if err != nil || replay.Review != result.Review {
		t.Fatal("original receipt was not replayed", replay, err)
	}
	if _, err = e.Get(ctx, proposal.ID); err == nil {
		t.Fatal("ignored unpublished proposal survived")
	}
	if _, err = s.Store.Learning.Promotion(ctx, id, 2, time.Now().Add(-time.Hour), time.Now()); err == nil {
		t.Fatal("ignored source admitted generation")
	}
	if ids, err := s.Store.Learning.PromotionIDs(ctx, "", true); err != nil || len(ids) != 0 {
		t.Fatal(ids, err)
	}
	request.Ignored = false
	if _, err = s.Store.Learning.SetPatternIgnored(ctx, x.ID, request); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatal("changed request key was replayed", err)
	}
	request.RequestKey = "restore-original"
	if _, err = s.Store.Learning.SetPatternIgnored(ctx, x.ID, request); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatal("stale version restored ignored source", err)
	}
	request.ExpectedStateVersion = 1
	result, err = s.Store.Learning.SetPatternIgnored(ctx, x.ID, request)
	if err != nil || result.Review.Ignored || result.Review.StateVersion != 2 {
		t.Fatal(result, err)
	}
	if got := experienceDiscoveries(t, e); len(got) != 0 {
		t.Fatal("restore refunded original promotion", got)
	}
	var consumedAfter, callsAfter int
	_ = s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_discovery_episode_consumed`).Scan(&consumedAfter)
	_ = s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_maintenance_calls`).Scan(&callsAfter)
	if consumedBefore != 2 || consumedAfter != consumedBefore || callsAfter != callsBefore || len(g.seen) != 1 || len(matcher.seen) != 1 {
		t.Fatal("review reset dedup, spent calls or silently retried", consumedBefore, consumedAfter, callsBefore, callsAfter)
	}
	if err = r.WithPublication(ctx, "project", "json-check", func(pub *skills.PublicationTx) error {
		head, err := pub.Current()
		if err == nil && head != ds[0].Base {
			t.Fatal("ignore changed installed skill", head)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSkillLearningReviewAdjudicationPinsNewHeadAndRequiresManualPublication(t *testing.T) {
	s, x, r, e, _, _, matcher := experienceDiscoveryTest(t)
	ctx := context.Background()
	original := experienceTwoEpisodes(t, s, x)
	matcherCalls := len(matcher.seen)
	enableExperienceDiscovery(t, e)
	if len(experienceDiscoveries(t, e)) != 1 {
		t.Fatal("missing original discovery")
	}
	oldProposal := experienceGenerate(t, e)
	p := learningReviewPattern(t, s, x)
	request := learningAdjudicationRequest(t, r, p, "json-check", "current-head")
	if err := r.SetEnabled(ctx, "project", "json-check", false); err != nil {
		t.Fatal(err)
	}
	if err := r.SetEnabled(ctx, "project", "json-check", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.Learning.AdjudicatePattern(ctx, x.ID, request); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatal("manual target silently rebased stale head", err)
	}
	request = learningAdjudicationRequest(t, r, p, "json-check", "current-head")
	result, err := s.Store.Learning.AdjudicatePattern(ctx, x.ID, request)
	if err != nil || result.Match == nil || result.Match.Origin != "host_adjudicated" || result.Match.Quality != "unassessed" ||
		result.Match.SourceState != "checked_at_adjudication" || len(result.Match.ActualReads) != 0 || strings.Contains(result.Match.Reason, "PRIVATE_HOST_REASON") ||
		result.Review.StateVersion != 1 || result.Review.AdjudicationID != result.Match.ID {
		t.Fatal(result, err)
	}
	if _, err = s.Store.Learning.Promotion(ctx, original, 2, time.Now().Add(-time.Hour), time.Now()); err == nil {
		t.Fatal("original model match remained authority after manual decision")
	}
	if _, err = e.Get(ctx, oldProposal.ID); err == nil {
		t.Fatal("old unpublished proposal remained usable")
	}
	promotion, err := s.Store.Learning.Promotion(ctx, result.Match.ID, 2, time.Now().Add(-time.Hour), time.Now())
	if err != nil || !promotion.Manual || promotion.Target != request.Target || promotion.Head != request.ExpectedHead {
		t.Fatal(promotion, err)
	}
	page, err := s.Store.Learning.Matches(ctx, x.ID, "", 10)
	if err != nil || len(page.Matches) != 2 || page.Matches[0].Origin != "host_adjudicated" || page.Matches[0].SourceState != "not_revalidated" ||
		page.Matches[0].Review == nil || page.Matches[0].Review.AdjudicationID != result.Match.ID || len(matcher.seen) != matcherCalls {
		t.Fatal("manual history overwrote model result or fabricated a model call", page, err)
	}
	replay, err := s.Store.Learning.AdjudicatePattern(ctx, x.ID, request)
	if err != nil || replay.Review != result.Review || replay.Match.ID != result.Match.ID {
		t.Fatal(replay, err)
	}
	// Same target/version evidence remains spent even for a new host decision.
	if len(experienceDiscoveries(t, e)) != 0 {
		t.Fatal("manual decision refunded same target votes")
	}
}

func TestSkillLearningReviewRetargetBeyondShortlistAndLiveRevocation(t *testing.T) {
	s, x, r, e, g, _, matcher := experienceDiscoveryTest(t)
	ctx := context.Background()
	for _, name := range []string{"a-check", "b-check", "c-check", "d-check", "e-check", "f-check", "g-check", "h-check", "zz-target"} {
		root := filepath.Join(x.CWD, "skills", name)
		if err := os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: General verification\n---\nInspect input\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Install(ctx, "project", name); err != nil {
			t.Fatal(err)
		}
		if err := r.SetEnabled(ctx, "project", name, true); err != nil {
			t.Fatal(err)
		}
	}
	experienceTwoEpisodes(t, s, x)
	if len(matcher.seen) != 1 || len(matcher.seen[0].Skills) != 8 {
		t.Fatal(matcher.seen)
	}
	for _, skill := range matcher.seen[0].Skills {
		if skill.Name == "zz-target" {
			t.Fatal("fixture target was already in shortlist")
		}
	}
	p := learningReviewPattern(t, s, x)
	request := learningAdjudicationRequest(t, r, p, "zz-target", "explicit-outside-shortlist")
	result, err := s.Store.Learning.AdjudicatePattern(ctx, x.ID, request)
	if err != nil || result.Match == nil || result.Match.Target.Name != "zz-target" || len(result.Match.ActualReads) != 0 {
		t.Fatal(result, err)
	}
	enableExperienceDiscovery(t, e)
	ds := experienceDiscoveries(t, e)
	if len(ds) != 1 || ds[0].Evidence[0].Experience.MatchID != result.Match.ID {
		t.Fatal(ds)
	}
	proposal := experienceGenerate(t, e)
	if proposal.Status != "pending_review" || proposal.Trigger.Type != "experience_pattern" || len(g.seen) != 1 || !g.seen[0].InstructionOnly {
		t.Fatal(proposal, g.seen)
	}
	// Changing a real receipt after manual review cannot leave cached authority.
	if _, err = s.Store.db.Exec(`UPDATE harness_tool_receipts SET version=version+1 WHERE state='completed'`); err != nil {
		t.Fatal(err)
	}
	if _, err = e.Approve(ctx, proposal.ID, harness.SkillReview{ExpectedStateVersion: proposal.StateVersion, Actor: "owner"}); err == nil {
		t.Fatal("manual approval ignored live receipt mutation")
	}
	control, err := s.Store.Learning.Control(ctx, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Store.Learning.SetControl(ctx, x.ID, harness.SkillLearningControlRequest{Contribute: false, ExpectedEpoch: control.Epoch}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"df_learning_adjudications", "df_learning_review_receipts", "df_learning_pattern_reviews"} {
		var count int
		if err = s.Store.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("revocation retained manual derived text", table, count, err)
		}
	}
	if _, err = s.Store.Learning.AdjudicatePattern(ctx, x.ID, request); err == nil {
		t.Fatal("forgotten manual receipt resurrected source")
	}
}

func TestSkillLearningReviewIgnoreDuringMatchingFencesLateOutput(t *testing.T) {
	s, x, _, _, matcher := newMatchingTest(t)
	ctx := context.Background()
	consolidateExtract(t, s, x, "Check structured input")
	consolidateRun(t, s)
	p := learningReviewPattern(t, s, x)
	matchingQueue(t, s)
	matcher.during = func() {
		_, err := s.Store.Learning.SetPatternIgnored(ctx, x.ID, harness.SkillLearningPatternIgnoreRequest{
			PatternID: p.ID, ExpectedPatternRevision: p.Revision, Scope: "session", RequestKey: "ignore-during-call", Ignored: true})
		if err != nil {
			t.Fatal(err)
		}
	}
	if worked, err := s.Store.Learning.RunOne(ctx); !worked || err != nil && !errors.Is(err, skillmaintenance.ErrConflict) {
		t.Fatal(worked, err)
	}
	page, err := s.Store.Learning.Matches(ctx, x.ID, "", 10)
	if err != nil || len(page.Matches) != 0 {
		t.Fatal("late matching output survived ignore", page, err)
	}
	_, state := matchingJob(t, s)
	if state != "cancelled" || len(matcher.seen) != 1 {
		t.Fatal(state, matcher.seen)
	}
}

func TestSkillLearningReviewWorkspaceScopeAndOperatorRevocation(t *testing.T) {
	s, x, _, _ := newWorkspaceLearningTest(t, true)
	ctx := context.Background()
	workspaceLearningConsent(t, s, x, true)
	consolidateExtract(t, s, x, "Source configuration task")
	consolidateRun(t, s)
	y := workspaceLearningSession(t, s, x.CWD)
	workspaceLearningConsent(t, s, y, true)
	page, err := s.Store.Learning.WorkspacePatterns(ctx, y.ID, "", 10)
	if err != nil || len(page.Patterns) != 1 {
		t.Fatal(page, err)
	}
	p := page.Patterns[0]
	matchingQueue(t, s)
	matchingRun(t, s)
	matches, err := s.Store.Learning.WorkspaceMatches(ctx, y.ID, "", 10)
	if err != nil || len(matches.Matches) != 1 {
		t.Fatal(matches, err)
	}
	ignore := harness.SkillLearningPatternIgnoreRequest{PatternID: p.ID, ExpectedPatternRevision: p.Revision, Scope: "session", RequestKey: "workspace-review", Ignored: true}
	if _, err = s.Store.Learning.SetPatternIgnored(ctx, y.ID, ignore); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("shared suggestion controlled through private scope", err)
	}
	ignore.Scope = "workspace"
	private := workspaceLearningSession(t, s, x.CWD)
	if _, err = s.Store.Learning.SetPatternIgnored(ctx, private.ID, ignore); err == nil {
		t.Fatal("unconsented session controlled shared suggestion")
	}
	foreign := workspaceLearningSession(t, s, t.TempDir())
	workspaceLearningConsent(t, s, foreign, true)
	if _, err = s.Store.Learning.SetPatternIgnored(ctx, foreign.ID, ignore); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("shared suggestion crossed workspace", err)
	}
	result, err := s.Store.Learning.SetPatternIgnored(ctx, y.ID, ignore)
	if err != nil || !result.Review.Ignored {
		t.Fatal(result, err)
	}
	ignore.RequestKey, ignore.ExpectedStateVersion, ignore.Ignored = "workspace-restore", 1, false
	if _, err = s.Store.Learning.SetPatternIgnored(ctx, y.ID, ignore); err != nil {
		t.Fatal(err)
	}
	// The acting session need not be a member, but its grant is still revocable.
	target, head := *matches.Matches[0].Target, *matches.Matches[0].Head
	request := harness.SkillLearningPatternAdjudicationRequest{PatternID: p.ID, ExpectedPatternRevision: p.Revision, ExpectedStateVersion: 2,
		Scope: "workspace", RequestKey: "third-session-adjudication", Target: target, ExpectedHead: head, Relation: "body_gap", Reason: "Host review of shared procedure"}
	result, err = s.Store.Learning.AdjudicatePattern(ctx, y.ID, request)
	if err != nil || result.Match == nil {
		t.Fatal(result, err)
	}
	workspaceLearningConsent(t, s, y, false)
	var count int
	if err = s.Store.db.QueryRow(`SELECT count(*) FROM df_learning_patterns`).Scan(&count); err != nil || count != 0 {
		t.Fatal("operator consent withdrawal retained derived pattern", count, err)
	}
	if err = s.Store.db.QueryRow(`SELECT count(*) FROM df_learning_experiences WHERE session_id=?`, x.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("operator revocation deleted independent raw source", count, err)
	}
}

func TestSkillLearningReviewPersistedAdjudicationReconstructsProjection(t *testing.T) {
	s, x, r, _, _ := newMatchingTest(t)
	ctx := context.Background()
	consolidateExtract(t, s, x, "First complete input")
	consolidateRun(t, s)
	p := learningReviewPattern(t, s, x)
	request := learningAdjudicationRequest(t, r, p, "json-check", "tamper-review")
	result, err := s.Store.Learning.AdjudicatePattern(ctx, x.ID, request)
	if err != nil {
		t.Fatal(err)
	}
	var data []byte
	if err = s.Store.db.QueryRow(`SELECT result FROM df_learning_adjudications WHERE id=?`, result.Match.ID).Scan(&data); err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err = json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	object["match"].(map[string]any)["origin"] = "model_matched"
	changed, _ := json.Marshal(object)
	if _, err = s.Store.db.Exec(`UPDATE df_learning_adjudications SET result=?,result_sha=? WHERE id=?`, changed, fmt.Sprintf("%x", sha256.Sum256(changed)), result.Match.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Store.Learning.Matches(ctx, x.ID, "", 10); err == nil {
		t.Fatal("rehashing fabricated model origin bypassed reconstruction")
	}
	if _, err = s.Store.db.Exec(`UPDATE df_learning_adjudications SET result=?,result_sha=? WHERE id=?`, data, fmt.Sprintf("%x", sha256.Sum256(data)), result.Match.ID); err != nil {
		t.Fatal(err)
	}
	var receipt []byte
	if err = s.Store.db.QueryRow(`SELECT result FROM df_learning_review_receipts WHERE session_id=? AND request_key=?`, x.ID, request.RequestKey).Scan(&receipt); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(receipt, &object); err != nil {
		t.Fatal(err)
	}
	object["match"].(map[string]any)["origin"] = "model_matched"
	changed, _ = json.Marshal(object)
	if _, err = s.Store.db.Exec(`UPDATE df_learning_review_receipts SET result=?,result_sha=? WHERE session_id=? AND request_key=?`,
		changed, fmt.Sprintf("%x", sha256.Sum256(changed)), x.ID, request.RequestKey); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Store.Learning.AdjudicatePattern(ctx, x.ID, request); err == nil {
		t.Fatal("rehashed historical receipt fabricated model decision")
	}
}

func TestSkillLearningReviewManualTargetDoesNotGrantAutoPatch(t *testing.T) {
	s, x, r, e, g, _, _ := experienceDiscoveryTest(t)
	ctx := context.Background()
	adoptLearningBase(t, r)
	g.preserveFrontmatter = true
	evaluator := &autoPatchLearningEvaluator{mode: "on"}
	if err := e.ConfigureBehavior(evaluator, harness.SkillBehaviorOptions{Suites: []harness.SkillBehaviorSuite{autoPatchLearningSuite()}}); err != nil {
		t.Fatal(err)
	}
	experienceTwoEpisodes(t, s, x)
	p := learningReviewPattern(t, s, x)
	request := learningAdjudicationRequest(t, r, p, "json-check", "manual-publication-only")
	if _, err := s.Store.Learning.AdjudicatePattern(ctx, x.ID, request); err != nil {
		t.Fatal(err)
	}
	enableExperienceDiscovery(t, e)
	if err := e.ConfigureAutoPatch(learningAutoPatchOptions(true)); err != nil {
		t.Fatal(err)
	}
	ds := experienceDiscoveries(t, e)
	if len(ds) != 1 {
		t.Fatal(ds)
	}
	proposal := autoPatchLearningProposal(t, e, ds[0])
	if proposal.Evaluation.Behavior != "passed" || evaluator.calls != 4 {
		t.Fatal("fixture did not meet existing auto-patch behavior gate", proposal, evaluator.calls)
	}
	if err := e.RunAutoPatch(ctx); err != nil {
		t.Fatal(err)
	}
	proposal, err := e.Get(ctx, proposal.ID)
	if err != nil || proposal.Status != "pending_review" {
		t.Fatal("host target choice silently granted automatic publication", proposal, err)
	}
	proposal, err = e.Approve(ctx, proposal.ID, harness.SkillReview{ExpectedStateVersion: proposal.StateVersion, Actor: "owner"})
	if err != nil || proposal.Status != "published" {
		t.Fatal("explicit human publication was not available", proposal, err)
	}
	ignored, err := s.Store.Learning.SetPatternIgnored(ctx, x.ID, harness.SkillLearningPatternIgnoreRequest{PatternID: p.ID, ExpectedPatternRevision: p.Revision,
		ExpectedStateVersion: 1, Scope: "session", RequestKey: "ignore-after-adoption", Ignored: true})
	if err != nil || !ignored.Review.Ignored {
		t.Fatal(ignored, err)
	}
	if err = r.WithPublication(ctx, "project", "json-check", func(pub *skills.PublicationTx) error {
		head, err := pub.Current()
		if err == nil && head.Version != proposal.CandidateVersion {
			t.Fatal("ignore removed human-adopted skill", head)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSkillLearningReviewRestartDisabledReceiptAndIgnoredRevisions(t *testing.T) {
	s, x, r, _, matcher := newMatchingTest(t)
	ctx := context.Background()
	consolidateExtract(t, s, x, "First source")
	consolidateRun(t, s)
	p := learningReviewPattern(t, s, x)
	request := learningAdjudicationRequest(t, r, p, "json-check", "restart-review")
	result, err := s.Store.Learning.AdjudicatePattern(ctx, x.ID, request)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Store.Learning.Close(); err != nil {
		t.Fatal(err)
	}
	l, err := learning.New(ctx, s.Store.db, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Store.Learning = l
	defer l.Close()
	replay, err := l.AdjudicatePattern(ctx, x.ID, request)
	if err != nil || replay.Match == nil || replay.Match.ID != result.Match.ID || replay.Review != result.Review {
		t.Fatal("disabled host could not report historical mutation receipt", replay, err)
	}
	newRequest := request
	newRequest.RequestKey, newRequest.ExpectedStateVersion = "new-while-disabled", 1
	if _, err = l.AdjudicatePattern(ctx, x.ID, newRequest); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("disabled host admitted a new manual decision", err)
	}
	ignored, err := l.SetPatternIgnored(ctx, x.ID, harness.SkillLearningPatternIgnoreRequest{PatternID: p.ID, ExpectedPatternRevision: p.Revision,
		ExpectedStateVersion: 1, Scope: "session", RequestKey: "ignore-while-disabled", Ignored: true})
	if err != nil || !ignored.Review.Ignored {
		t.Fatal("disabling learning prevented withdrawal", ignored, err)
	}
	_ = l.Close()
	options := consolidationTestOptions()
	options.Matching = &harness.SkillGenerationModelConfig{}
	l, err = learning.New(ctx, s.Store.db, options)
	if err != nil {
		t.Fatal(err)
	}
	s.Store.Learning = l
	defer l.Close()
	if err = l.Configure(ctx, &consolidationTestExtractor{observed: true}, nil); err != nil {
		t.Fatal(err)
	}
	if err = l.ConfigureConsolidation(&consolidationTestModel{}); err != nil {
		t.Fatal(err)
	}
	access, err := r.NewMatchAccess(harness.SkillSelection{})
	if err != nil {
		t.Fatal(err)
	}
	if err = l.ConfigureMatching(matcher, access); err != nil {
		t.Fatal(err)
	}
	if _, err = s.StartSkillLearningEpisode(ctx, "owner", x.ID); err != nil {
		t.Fatal(err)
	}
	consolidateExtract(t, s, x, "Second source")
	consolidateRun(t, s)
	p = learningReviewPattern(t, s, x)
	if p.Revision != 2 || !p.Review.Ignored || p.Review.StateVersion != 2 {
		t.Fatal("new evidence cleared persistent ignore", p)
	}
	if worked, err := l.QueueMatchOne(ctx); err != nil || worked {
		t.Fatal("ignored future revision queued matching", worked, err)
	}
	// A receipt may predate newer evidence; it cannot repeat the old mutation.
	replay, err = l.AdjudicatePattern(ctx, x.ID, request)
	if err != nil || replay.Review.PatternRevision != 1 || replay.Match.ID != result.Match.ID {
		t.Fatal("new pattern revision changed the historical receipt", replay, err)
	}
	if p = learningReviewPattern(t, s, x); !p.Review.Ignored || p.Review.StateVersion != 2 || p.Review.AdjudicationID != "" {
		t.Fatal("replaying historical receipt reactivated old decision", p.Review)
	}
}

func TestSkillLearningReviewAdjudicationRejectsUnauthorizedTargetsAndBlockedEvidence(t *testing.T) {
	s, x, r, _, _ := newMatchingTest(t)
	ctx := context.Background()
	consolidateExtract(t, s, x, "First observed source")
	consolidateRun(t, s)
	p := learningReviewPattern(t, s, x)
	valid := learningAdjudicationRequest(t, r, p, "json-check", "valid-review")
	for _, change := range []func(*harness.SkillLearningPatternAdjudicationRequest){
		func(r *harness.SkillLearningPatternAdjudicationRequest) { r.Target.SourceID = "foreign" },
		func(r *harness.SkillLearningPatternAdjudicationRequest) {
			r.Target.SourceIdentity = strings.Repeat("f", 64)
		},
		func(r *harness.SkillLearningPatternAdjudicationRequest) { r.Target.Workspace = t.TempDir() },
		func(r *harness.SkillLearningPatternAdjudicationRequest) { r.Target.Name = "missing-target" },
		func(r *harness.SkillLearningPatternAdjudicationRequest) { r.ExpectedHead.StateVersion++ },
		func(r *harness.SkillLearningPatternAdjudicationRequest) { r.ExpectedPatternRevision++ },
		func(r *harness.SkillLearningPatternAdjudicationRequest) { r.ExpectedStateVersion++ },
		func(r *harness.SkillLearningPatternAdjudicationRequest) { r.Relation = "new_capability" },
	} {
		request := valid
		change(&request)
		if _, err := s.Store.Learning.AdjudicatePattern(ctx, x.ID, request); err == nil {
			t.Fatal("invalid host target granted authority", request)
		}
	}
	if err := r.SetEnabled(ctx, "project", "json-check", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.Learning.AdjudicatePattern(ctx, x.ID, valid); err == nil {
		t.Fatal("disabled target admitted host decision")
	}
	var count int
	if err := s.Store.db.QueryRow(`SELECT count(*) FROM df_learning_adjudications`).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed requests created manual records", count, err)
	}

	blocked, bx, br, extractor, _ := newMatchingTest(t)
	extractor.observed = false
	consolidateExtract(t, blocked, bx, "Unverified claim")
	consolidateRun(t, blocked)
	bp := learningReviewPattern(t, blocked, bx)
	if bp.EvidenceState != "unresolved" {
		t.Fatal("blocked fixture is not unresolved", bp.EvidenceState)
	}
	if _, err := blocked.Store.Learning.AdjudicatePattern(ctx, bx.ID, learningAdjudicationRequest(t, br, bp, "json-check", "blocked-review")); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("human target selection blessed unverified evidence", err)
	}
}
