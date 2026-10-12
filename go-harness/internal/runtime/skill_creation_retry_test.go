package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/learning"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills/evolution"
)

func creationRetryRequest(t *testing.T, s *Service, x harness.Session, e *evolution.Service, job, key string) harness.SkillLearningNewSkillRetryRequest {
	t.Helper()
	p := learningReviewPattern(t, s, x)
	prepared, err := e.PrepareNewSkillRetry(context.Background(), x.ID, harness.SkillLearningNewSkillRetryPreparationRequest{
		PatternID: p.ID, ExpectedPatternRevision: p.Revision, ExpectedStateVersion: p.Review.StateVersion, Scope: "session", PreviousGenerationID: job})
	if err != nil || prepared.MaxRetries != 3 || prepared.Request.ExpectedHead != (harness.SkillHead{}) || prepared.Request.OverlayBase != nil {
		t.Fatal("creation retry did not prepare exact absent target", prepared, err)
	}
	prepared.Request.RequestKey = key
	return prepared.Request
}

func expireCreationRetryCooldown(t *testing.T, s *Service) {
	t.Helper()
	if _, err := s.Store.db.Exec(`UPDATE df_skill_discovery_cooldowns SET until_ns=0`); err != nil {
		t.Fatal(err)
	}
}

func TestSkillLearningCreationRetryCancelledAtomicReceiptAndManualPublish(t *testing.T) {
	s, x, r, e, g, _, matcher := experienceDiscoveryTest(t)
	ctx := context.Background()
	matcher.relation, matcher.suggestedName = "new_capability", "structured-json-audit"
	experienceTwoEpisodes(t, s, x)
	enableNewExperienceDiscovery(t, e)
	ds := experienceDiscoveries(t, e)
	if len(ds) != 1 {
		t.Fatal(ds)
	}
	job, _ := e.GetGeneration(ctx, ds[0].GenerationID)
	p := learningReviewPattern(t, s, x)
	prepare := harness.SkillLearningNewSkillRetryPreparationRequest{PatternID: p.ID, ExpectedPatternRevision: p.Revision, ExpectedStateVersion: 0, Scope: "session", PreviousGenerationID: job.ID}
	if _, err := e.PrepareNewSkillRetry(ctx, x.ID, prepare); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatal("active creation was retryable", err)
	}
	if err := e.CancelGeneration(ctx, job.ID, job.StateVersion); err != nil {
		t.Fatal(err)
	}
	request := creationRetryRequest(t, s, x, e, job.ID, "explicit-cancel-retry")
	if _, err := e.RetryNewSkill(ctx, x.ID, request); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatal("retry bypassed cooldown", err)
	}
	expireCreationRetryCooldown(t, s)
	var original []byte
	_ = s.Store.db.QueryRow(`SELECT payload FROM df_skill_discoveries WHERE id=?`, ds[0].ID).Scan(&original)
	if _, err := s.Store.db.Exec(`CREATE TRIGGER fail_creation_retry_receipt BEFORE INSERT ON df_learning_new_skill_retries BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.RetryNewSkill(ctx, x.ID, request); err == nil {
		t.Fatal("partial retry committed")
	}
	for query, want := range map[string]int{
		`SELECT count(*) FROM df_skill_generation_inputs`: 1, `SELECT count(*) FROM df_skill_discoveries`: 1,
		`SELECT count(*) FROM df_learning_promotion_dependencies`: 1, `SELECT count(*) FROM df_learning_new_skill_retry_spent`: 0,
		`SELECT count(*) FROM df_skill_discovery_episode_consumed`: 2, `SELECT sum(reservations) FROM df_skill_discovery_days`: 1,
	} {
		var count int
		if err := s.Store.db.QueryRow(query).Scan(&count); err != nil || count != want {
			t.Fatal("retry was not atomic", query, count, want, err)
		}
	}
	if learningReviewPattern(t, s, x).Review.StateVersion != 0 {
		t.Fatal("failed enqueue advanced review")
	}
	if _, err := s.Store.db.Exec(`DROP TRIGGER fail_creation_retry_receipt`); err != nil {
		t.Fatal(err)
	}
	result, err := e.RetryNewSkill(ctx, x.ID, request)
	if err != nil || result.Review.StateVersion != 1 || result.Retry.RetryNumber != 1 || result.Retry.EvidenceReservationID != ds[0].ID || result.GenerationID == job.ID {
		t.Fatal(result, err)
	}
	replay, err := e.RetryNewSkill(ctx, x.ID, request)
	if err != nil || replay != result {
		t.Fatal("unknown result did not replay original receipt", replay, err)
	}
	changed := request
	changed.Target.Name = "renamed-json-audit"
	if _, err = e.RetryNewSkill(ctx, x.ID, changed); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatal("key reused with another name", err)
	}
	if ds = experienceDiscoveries(t, e); len(ds) != 2 {
		t.Fatal("automatic discovery retried creation", ds)
	}
	if worked, err := e.RunGeneration(ctx); err != nil || !worked {
		t.Fatal(worked, err)
	}
	generated, err := e.GetGeneration(ctx, result.GenerationID)
	if err != nil || generated.State != "completed" || len(g.seen) != 1 || !g.seen[0].InstructionOnly {
		t.Fatal(generated, err, g.seen)
	}
	proposal, err := e.Get(ctx, generated.ProposalID)
	if err != nil || proposal.Status != "pending_review" || proposal.Author != "host_creation_retry" {
		t.Fatal(proposal, err)
	}
	if err = r.WithPublication(ctx, request.Target.SourceID, request.Target.Name, func(pub *skills.PublicationTx) error {
		head, err := pub.Current()
		if err == nil && head != (harness.SkillHead{}) {
			t.Fatal("retry published without human", head)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var originalAfter []byte
	_ = s.Store.db.QueryRow(`SELECT payload FROM df_skill_discoveries WHERE id=?`, result.Retry.EvidenceReservationID).Scan(&originalAfter)
	if string(original) != string(originalAfter) {
		t.Fatal("retry rewrote original snapshot")
	}
	proposal, err = e.Approve(ctx, proposal.ID, harness.SkillReview{ExpectedStateVersion: proposal.StateVersion, Actor: "owner"})
	if err != nil || proposal.Status != "published" {
		t.Fatal(proposal, err)
	}
	replay, err = e.RetryNewSkill(ctx, x.ID, request)
	if err != nil || replay != result || len(g.seen) != 1 {
		t.Fatal("receipt reexecuted published retry", replay, err)
	}
}

func TestSkillLearningCreationRetryRejectedChainHasBoundedNonrefundableReservations(t *testing.T) {
	s, x, _, e, g, _, matcher := experienceDiscoveryTest(t)
	ctx := context.Background()
	matcher.relation, matcher.suggestedName = "new_capability", "structured-json-audit"
	experienceTwoEpisodes(t, s, x)
	enableNewExperienceDiscovery(t, e)
	ds := experienceDiscoveries(t, e)
	proposal := experienceGenerate(t, e)
	if _, err := e.Reject(ctx, proposal.ID, harness.SkillReview{ExpectedStateVersion: proposal.StateVersion, Actor: "owner"}); err != nil {
		t.Fatal(err)
	}
	previous := ds[0].GenerationID
	for _, key := range []string{"retry-one", "retry-two", "retry-three"} {
		request := creationRetryRequest(t, s, x, e, previous, key)
		expireCreationRetryCooldown(t, s)
		result, err := e.RetryNewSkill(ctx, x.ID, request)
		if err != nil {
			t.Fatal(result, err)
		}
		previous = result.GenerationID
		job, err := e.GetGeneration(ctx, previous)
		if err != nil {
			t.Fatal(err)
		}
		if err = e.CancelGeneration(ctx, previous, job.StateVersion); err != nil {
			t.Fatal(err)
		}
	}
	p := learningReviewPattern(t, s, x)
	if _, err := e.PrepareNewSkillRetry(ctx, x.ID, harness.SkillLearningNewSkillRetryPreparationRequest{
		PatternID: p.ID, ExpectedPatternRevision: p.Revision, ExpectedStateVersion: p.Review.StateVersion, Scope: "session", PreviousGenerationID: previous}); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatal("retry cap bypassed", err)
	}
	for query, want := range map[string]int{`SELECT count(*) FROM df_learning_new_skill_consumed`: 1, `SELECT count(*) FROM df_learning_new_skill_retry_spent`: 3, `SELECT count(*) FROM df_skill_discovery_episode_consumed`: 2, `SELECT sum(reservations) FROM df_skill_discovery_days`: 4} {
		var count int
		if err := s.Store.db.QueryRow(query).Scan(&count); err != nil || count != want {
			t.Fatal(query, count, want, err)
		}
	}
	if len(g.seen) != 1 {
		t.Fatal("manual enqueue made generation calls", g.seen)
	}
	if len(experienceDiscoveries(t, e)) != 4 {
		t.Fatal("automatic retry reservation bypassed")
	}
}

func TestSkillLearningCreationRetryLiveRevocationRestartAndRehashedBinding(t *testing.T) {
	s, x, r, e, g, v, matcher := experienceDiscoveryTest(t)
	ctx := context.Background()
	matcher.relation, matcher.suggestedName = "new_capability", "structured-json-audit"
	experienceTwoEpisodes(t, s, x)
	enableNewExperienceDiscovery(t, e)
	ds := experienceDiscoveries(t, e)
	job, _ := e.GetGeneration(ctx, ds[0].GenerationID)
	if err := e.CancelGeneration(ctx, job.ID, job.StateVersion); err != nil {
		t.Fatal(err)
	}
	request := creationRetryRequest(t, s, x, e, job.ID, "retry-restart")
	foreign := workspaceLearningSession(t, s, x.CWD)
	if _, err := e.RetryNewSkill(ctx, foreign.ID, request); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("directory granted private retry", err)
	}
	expireCreationRetryCooldown(t, s)
	result, err := e.RetryNewSkill(ctx, x.ID, request)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Close(); err != nil {
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
	// Discovery is disabled: receipt retrieval works without enabling new work.
	replay, err := restarted.RetryNewSkill(ctx, x.ID, request)
	if err != nil || replay != result {
		t.Fatal(replay, err)
	}
	enableNewExperienceDiscovery(t, restarted)
	var payload []byte
	if err = s.Store.db.QueryRow(`SELECT payload FROM df_skill_discoveries WHERE id=?`, result.DiscoveryID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]any
	_ = json.Unmarshal(payload, &snapshot)
	snapshot["creation_retry"].(map[string]any)["retry_number"] = float64(2)
	corrupt, _ := json.Marshal(snapshot)
	if _, err = s.Store.db.Exec(`UPDATE df_skill_discoveries SET payload=?,payload_sha=? WHERE id=?`, corrupt, evidenceHash(string(corrupt)), result.DiscoveryID); err != nil {
		t.Fatal(err)
	}
	if worked, err := restarted.RunGeneration(ctx); err != nil || !worked {
		t.Fatal(worked, err)
	}
	fenced, err := restarted.GetGeneration(ctx, result.GenerationID)
	if err != nil || fenced.State != "failed" || len(g.seen) != 0 {
		t.Fatal("rehash admitted corrupt retry before call", fenced, err)
	}
	control, _ := s.Store.Learning.Control(ctx, x.ID)
	if _, err = s.SetSkillLearningControl(ctx, "owner", x.ID, harness.SkillLearningControlRequest{Contribute: false, ExpectedEpoch: control.Epoch}); err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.RetryNewSkill(ctx, x.ID, request); err == nil {
		t.Fatal("receipt resurrected withdrawn learning source")
	}
	for query, want := range map[string]int{`SELECT count(*) FROM df_learning_new_skill_retries`: 0, `SELECT count(*) FROM df_learning_new_skill_retry_spent`: 1, `SELECT count(*) FROM df_learning_new_skill_consumed`: 1, `SELECT count(*) FROM df_skill_discovery_episode_consumed`: 2} {
		var count int
		if err := s.Store.db.QueryRow(query).Scan(&count); err != nil || count != want {
			t.Fatal(query, count, want, err)
		}
	}
}

func TestSkillLearningCreationRetryExactPinsQuotaAndExpiredEvidence(t *testing.T) {
	s, x, r, e, _, _, matcher := experienceDiscoveryTest(t)
	ctx := context.Background()
	matcher.relation, matcher.suggestedName = "new_capability", "structured-json-audit"
	experienceTwoEpisodes(t, s, x)
	enableNewExperienceDiscovery(t, e)
	ds := experienceDiscoveries(t, e)
	job, _ := e.GetGeneration(ctx, ds[0].GenerationID)
	if err := e.CancelGeneration(ctx, job.ID, job.StateVersion); err != nil {
		t.Fatal(err)
	}
	request := creationRetryRequest(t, s, x, e, job.ID, "exact-pins")
	expireCreationRetryCooldown(t, s)
	for _, mutate := range []func(*harness.SkillLearningNewSkillRetryRequest){
		func(r *harness.SkillLearningNewSkillRetryRequest) { r.ExpectedGenerationStateVersion++ },
		func(r *harness.SkillLearningNewSkillRetryRequest) { r.ExpectedStateVersion++ },
		func(r *harness.SkillLearningNewSkillRetryRequest) { r.ExpectedRetryNumber++ },
		func(r *harness.SkillLearningNewSkillRetryRequest) { r.EvidenceReservationID = "sd_another" },
		func(r *harness.SkillLearningNewSkillRetryRequest) { r.Target.Name = "renamed-json-audit" },
		func(r *harness.SkillLearningNewSkillRetryRequest) { r.Target.SourceID = "other" },
		func(r *harness.SkillLearningNewSkillRetryRequest) { r.Target.SourceIdentity = fmt.Sprintf("%064x", 1) },
		func(r *harness.SkillLearningNewSkillRetryRequest) {
			r.PreviousProposalID = "proposal_fake"
			r.ExpectedProposalStateVersion = 1
		},
	} {
		invalid := request
		mutate(&invalid)
		if _, err := e.RetryNewSkill(ctx, x.ID, invalid); err == nil {
			t.Fatal("retry changed pinned authorization", invalid)
		}
	}
	other, err := evolution.New(ctx, r, &experienceLocalValidator{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	p, err := other.Submit(ctx, harness.SkillProposalRequest{SourceID: "project", Name: request.Target.Name, RequestKey: "independent-pending", Action: "create", Reason: "Independent host capability",
		Files: map[string][]byte{"SKILL.md": []byte("---\nname: " + request.Target.Name + "\ndescription: Other authored procedure\n---\nRead input\n")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.RetryNewSkill(ctx, x.ID, request); err == nil {
		t.Fatal("same name pending creation bypassed")
	}
	if _, err = other.Reject(ctx, p.ID, harness.SkillReview{ExpectedStateVersion: p.StateVersion, Actor: "owner"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Store.db.Exec(`UPDATE df_skill_discovery_days SET reservations=5`); err != nil {
		t.Fatal(err)
	}
	if _, err = e.RetryNewSkill(ctx, x.ID, request); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatal("daily quota bypassed", err)
	}
	if _, err = s.Store.db.Exec(`UPDATE df_skill_discovery_days SET reservations=1`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Store.db.Exec(`UPDATE harness_learning_outbox SET ended_at=?`, time.Now().Add(-31*24*time.Hour).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err = e.RetryNewSkill(ctx, x.ID, request); err == nil {
		t.Fatal("expired episode window became fresh votes")
	}
	var count int
	if err = s.Store.db.QueryRow(`SELECT count(*) FROM df_learning_new_skill_retry_spent`).Scan(&count); err != nil || count != 0 {
		t.Fatal("rejected authority consumed retry", count, err)
	}
}

func TestSkillLearningCreationRetryReceiptCannotRehashNewReviewVersion(t *testing.T) {
	s, x, _, e, _, _, matcher := experienceDiscoveryTest(t)
	ctx := context.Background()
	matcher.relation, matcher.suggestedName = "new_capability", "structured-json-audit"
	experienceTwoEpisodes(t, s, x)
	enableNewExperienceDiscovery(t, e)
	ds := experienceDiscoveries(t, e)
	job, _ := e.GetGeneration(ctx, ds[0].GenerationID)
	if err := e.CancelGeneration(ctx, job.ID, job.StateVersion); err != nil {
		t.Fatal(err)
	}
	request := creationRetryRequest(t, s, x, e, job.ID, "immutable-receipt")
	expireCreationRetryCooldown(t, s)
	result, err := e.RetryNewSkill(ctx, x.ID, request)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []struct {
		name    string
		read    string
		write   string
		corrupt string
	}{
		{"request key", `SELECT request_key FROM df_skill_maintenance_jobs WHERE id=?`, `UPDATE df_skill_maintenance_jobs SET request_key=? WHERE id=?`, "creation-retry:sd_foreign"},
		{"job kind", `SELECT job_kind FROM df_skill_maintenance_jobs WHERE id=?`, `UPDATE df_skill_maintenance_jobs SET job_kind=? WHERE id=?`, "skill_behavior"},
		{"owner kind", `SELECT owner_kind FROM df_skill_maintenance_jobs WHERE id=?`, `UPDATE df_skill_maintenance_jobs SET owner_kind=? WHERE id=?`, "workspace"},
	} {
		var original string
		if err := s.Store.db.QueryRow(field.read, result.GenerationID).Scan(&original); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Store.db.Exec(field.write, field.corrupt, result.GenerationID); err != nil {
			t.Fatal(err)
		}
		_, receiptErr := e.RetryNewSkill(ctx, x.ID, request)
		if _, err := s.Store.db.Exec(field.write, original, result.GenerationID); err != nil {
			t.Fatal(err)
		}
		if receiptErr == nil {
			t.Fatal("receipt accepted altered generation", field.name)
		}
	}
	if replay, err := e.RetryNewSkill(ctx, x.ID, request); err != nil || replay != result {
		t.Fatal("job identity restoration changed original receipt", replay, err)
	}
	var data []byte
	if err = s.Store.db.QueryRow(`SELECT payload FROM df_learning_new_skill_retries WHERE id=?`, result.ID).Scan(&data); err != nil {
		t.Fatal(err)
	}
	var snap map[string]any
	_ = json.Unmarshal(data, &snap)
	snap["review"].(map[string]any)["adjudication_id"] = "ladj_fake"
	var shaped struct {
		Actor   string                                    `json:"actor"`
		Request harness.SkillLearningNewSkillRetryRequest `json:"request"`
		Review  harness.SkillLearningPatternReview        `json:"review"`
		Binding learning.NewSkillRetryBinding             `json:"binding"`
	}
	altered, _ := json.Marshal(snap)
	_ = json.Unmarshal(altered, &shaped)
	shaped.Binding.AuthorizationDigest = learning.NewSkillRetryAuthorization(shaped.Actor, shaped.Request, shaped.Review, shaped.Binding)
	corrupt, _ := json.Marshal(shaped)
	identity, _ := json.Marshal([]any{learning.NewSkillRetryPolicyVersion, shaped})
	if _, err = s.Store.db.Exec(`UPDATE df_learning_new_skill_retries SET id=?,payload=?,payload_sha=? WHERE id=?`, "lretry_"+fmt.Sprintf("%x", sha256.Sum256(identity)), corrupt, fmt.Sprintf("%x", sha256.Sum256(corrupt)), result.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = e.RetryNewSkill(ctx, x.ID, request); err == nil {
		t.Fatal("rehash fabricated current review authorization")
	}
}

func TestSkillLearningCreationRetryIgnoreKeepsOriginalReceiptWithoutReactivation(t *testing.T) {
	s, x, _, e, g, _, matcher := experienceDiscoveryTest(t)
	ctx := context.Background()
	matcher.relation, matcher.suggestedName = "new_capability", "structured-json-audit"
	experienceTwoEpisodes(t, s, x)
	enableNewExperienceDiscovery(t, e)
	ds := experienceDiscoveries(t, e)
	job, _ := e.GetGeneration(ctx, ds[0].GenerationID)
	if err := e.CancelGeneration(ctx, job.ID, job.StateVersion); err != nil {
		t.Fatal(err)
	}
	request := creationRetryRequest(t, s, x, e, job.ID, "ignore-retry")
	expireCreationRetryCooldown(t, s)
	result, err := e.RetryNewSkill(ctx, x.ID, request)
	if err != nil {
		t.Fatal(err)
	}
	p := learningReviewPattern(t, s, x)
	if _, err = s.Store.Learning.SetPatternIgnored(ctx, x.ID, harness.SkillLearningPatternIgnoreRequest{PatternID: p.ID, ExpectedPatternRevision: p.Revision,
		ExpectedStateVersion: p.Review.StateVersion, Scope: "session", RequestKey: "ignore-after-retry", Ignored: true}); err != nil {
		t.Fatal(err)
	}
	replay, err := e.RetryNewSkill(ctx, x.ID, request)
	if err != nil || replay != result || len(g.seen) != 0 {
		t.Fatal("original receipt reactivated ignored creation", replay, err)
	}
	if p = learningReviewPattern(t, s, x); !p.Review.Ignored || p.Review.StateVersion != 2 {
		t.Fatal("receipt changed review", p.Review)
	}
	if _, err = e.GetGeneration(ctx, result.GenerationID); err == nil {
		t.Fatal("receipt resurrected removed producer")
	}
	var count int
	if err = s.Store.db.QueryRow(`SELECT count(*) FROM df_learning_new_skill_retry_spent`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}

func TestSkillLearningCreationRetryWorkspaceNonmemberActorRevokesDerivedChain(t *testing.T) {
	s, x, r, original, g, v, matcher := experienceDiscoveryTest(t)
	ctx := context.Background()
	_ = original.Close()
	_ = s.Store.Learning.Close()
	options := consolidationTestOptions()
	options.CrossSession = true
	options.Matching = &harness.SkillGenerationModelConfig{}
	l, err := learning.New(ctx, s.Store.db, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
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
	matcher.relation, matcher.suggestedName = "new_capability", "structured-json-audit"
	if err = l.ConfigureMatching(matcher, access); err != nil {
		t.Fatal(err)
	}
	s.Store.Learning = l
	e, err := evolution.New(ctx, r, v)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	if err = e.ConfigureGeneration(ctx, g, options.Budget, []string{"project"}, false); err != nil {
		t.Fatal(err)
	}
	if err = e.ConfigureExperienceDiscoverySource(l); err != nil {
		t.Fatal(err)
	}
	workspaceLearningConsent(t, s, x, true)
	experienceTwoEpisodes(t, s, x)
	enableNewExperienceDiscovery(t, e)
	ds := experienceDiscoveries(t, e)
	job, _ := e.GetGeneration(ctx, ds[0].GenerationID)
	if err = e.CancelGeneration(ctx, job.ID, job.StateVersion); err != nil {
		t.Fatal(err)
	}
	y := workspaceLearningSession(t, s, x.CWD)
	workspaceLearningConsent(t, s, y, true)
	page, err := l.WorkspacePatterns(ctx, y.ID, "", 10)
	if err != nil || len(page.Patterns) != 1 {
		t.Fatal(page, err)
	}
	p := page.Patterns[0]
	request := harness.SkillLearningNewSkillRetryPreparationRequest{PatternID: p.ID, ExpectedPatternRevision: p.Revision, ExpectedStateVersion: p.Review.StateVersion, Scope: "session", PreviousGenerationID: job.ID}
	if _, err = e.PrepareNewSkillRetry(ctx, y.ID, request); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("workspace retry through private scope", err)
	}
	request.Scope = "workspace"
	prepared, err := e.PrepareNewSkillRetry(ctx, y.ID, request)
	if err != nil {
		t.Fatal(err)
	}
	prepared.Request.RequestKey = "nonmember-actor-retry"
	expireCreationRetryCooldown(t, s)
	result, err := e.RetryNewSkill(ctx, y.ID, prepared.Request)
	if err != nil {
		t.Fatal(err)
	}
	workspaceLearningConsent(t, s, y, false)
	for query, want := range map[string]int{`SELECT count(*) FROM df_learning_patterns`: 0, `SELECT count(*) FROM df_learning_new_skill_retries`: 0,
		`SELECT count(*) FROM df_skill_generation_inputs`: 0, `SELECT count(*) FROM df_learning_new_skill_retry_spent`: 1, `SELECT count(*) FROM df_learning_experiences`: 2} {
		var count int
		if err = s.Store.db.QueryRow(query).Scan(&count); err != nil || count != want {
			t.Fatal("actor revocation retained derived content or removed raw evidence", query, count, want, err)
		}
	}
	if _, err = e.GetGeneration(ctx, result.GenerationID); err == nil {
		t.Fatal("nonmember actor withdrawal retained producer")
	}
}

func TestSkillLearningCreationRetryFailedOutputRestartsAsFreshManualCandidate(t *testing.T) {
	s, x, r, e, g, v, matcher := experienceDiscoveryTest(t)
	ctx := context.Background()
	matcher.relation, matcher.suggestedName = "new_capability", "structured-json-audit"
	experienceTwoEpisodes(t, s, x)
	enableNewExperienceDiscovery(t, e)
	ds := experienceDiscoveries(t, e)
	g.extra = true
	if worked, err := e.RunGeneration(ctx); err != nil || !worked {
		t.Fatal(worked, err)
	}
	job, err := e.GetGeneration(ctx, ds[0].GenerationID)
	if err != nil || job.State != "failed" || job.ErrorCode != "invalid_candidate" {
		t.Fatal(job, err)
	}
	request := creationRetryRequest(t, s, x, e, job.ID, "retry-failed-output")
	expireCreationRetryCooldown(t, s)
	result, err := e.RetryNewSkill(ctx, x.ID, request)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Close(); err != nil {
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
	enableNewExperienceDiscovery(t, restarted)
	g.extra = false
	if worked, err := restarted.RunGeneration(ctx); err != nil || !worked {
		t.Fatal(worked, err)
	}
	newJob, err := restarted.GetGeneration(ctx, result.GenerationID)
	if err != nil || newJob.State != "completed" || len(g.seen) != 2 {
		t.Fatal(newJob, err, len(g.seen))
	}
	p, err := restarted.Get(ctx, newJob.ProposalID)
	if err != nil || p.Status != "pending_review" || len(p.ChangedFiles) != 1 || p.ChangedFiles[0] != "SKILL.md" {
		t.Fatal(p, err)
	}
	original, err := restarted.GetGeneration(ctx, job.ID)
	if err != nil || original.State != "failed" || original.CandidateVersion != "" {
		t.Fatal("retry rewrote failed original", original, err)
	}
}
