package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
	"github.com/omengye/deerflow-acp/go-harness/internal/learning"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

func sourceExperienceRequest(e harness.SkillExperience) harness.SkillLearningSourceRequest {
	return harness.SkillLearningSourceRequest{Kind: "experience", ID: e.ID, ExpectedRevision: e.Revision, Scope: "session", ExperienceID: e.ID}
}

func sourceExperience(t *testing.T, s *Service, x harness.Session) harness.SkillExperience {
	t.Helper()
	page, err := s.Store.Learning.Experiences(context.Background(), x.ID, "", 10)
	if err != nil || len(page.Experiences) == 0 {
		t.Fatal(page, err)
	}
	return page.Experiences[0]
}

func TestSkillLearningSourceReadOnlyRedactedCitationsAndDisabledRecovery(t *testing.T) {
	s, x, extractor := newLearningTest(t, true)
	ctx := context.Background()
	req := learningTestRun(t, s, x, "Read JSON token=PRIVATE_SOURCE_SECRET")
	learningTestFinish(t, s, req, "end_turn")
	learningTestQueue(t, s)
	if worked, err := s.Store.Learning.RunOne(ctx); err != nil || !worked {
		t.Fatal(worked, err)
	}
	e := sourceExperience(t, s, x)
	request := sourceExperienceRequest(e)
	before := learningCount(t, s.Store, "df_skill_maintenance_jobs")
	got, err := s.Store.Learning.Source(ctx, x.ID, request)
	if err != nil || got.SessionID != x.ID || got.SourceSessionID != x.ID || got.Source != request || got.SourceState != "checked_at_read" || got.Quality != "unassessed" || got.ConsentEpoch != 1 || len(got.References) != 1 || got.References[0].Kind != "user" || got.References[0].ID != e.Candidate.References[0] {
		t.Fatal(got, err)
	}
	encoded, _ := json.Marshal(got)
	for _, forbidden := range []string{"PRIVATE_SOURCE_SECRET", "PRIVATE_HIDDEN_REASONING", "PRIVATE_CHILD_REPLY", "verification pending"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatal("uncited or private source leaked", forbidden)
		}
	}
	if len(extractor.seen) != 1 || learningCount(t, s.Store, "df_skill_maintenance_jobs") != before {
		t.Fatal("read queued paid learning work")
	}
	continuation := learningTestRun(t, s, x, "Continue the same task; this new turn is still running")
	continuing, err := s.Store.Learning.Source(ctx, x.ID, request)
	continuingJSON, _ := json.Marshal(continuing)
	if err != nil || string(continuingJSON) != string(encoded) {
		t.Fatal("uncaptured future turn hid historical cited source", continuing, err)
	}
	learningTestFinish(t, s, continuation, "end_turn")
	foreign := workspaceLearningSession(t, s, x.CWD)
	if _, err = s.Store.Learning.Source(ctx, foreign.ID, request); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("directory granted private source access", err)
	}
	changed := request
	changed.ExpectedRevision++
	if _, err = s.Store.Learning.Source(ctx, x.ID, changed); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatal("old experience revision accepted", err)
	}
	for _, mutate := range []func(*harness.SkillLearningSourceRequest){
		func(r *harness.SkillLearningSourceRequest) { r.Kind = "match" },
		func(r *harness.SkillLearningSourceRequest) { r.Scope = "workspace" },
		func(r *harness.SkillLearningSourceRequest) { r.ExperienceID = "lexp_foreign" },
	} {
		invalid := request
		mutate(&invalid)
		if _, err = s.Store.Learning.Source(ctx, x.ID, invalid); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatal("invalid source request accepted", invalid, err)
		}
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
	reloaded, err := disabled.Source(ctx, x.ID, request)
	reencoded, _ := json.Marshal(reloaded)
	if err != nil || string(reencoded) != string(encoded) {
		t.Fatal("disabled paid work blocked private inspection", reloaded, err)
	}
	if _, err = s.SetSkillLearningControl(ctx, "owner", x.ID, harness.SkillLearningControlRequest{ExpectedEpoch: 1, Contribute: false}); err != nil {
		t.Fatal(err)
	}
	if got, err = disabled.Source(ctx, x.ID, request); !errors.Is(err, harness.ErrNotFound) || len(got.References) != 0 {
		t.Fatal("withdrawn source remained readable", got, err)
	}
}

func TestSkillLearningSourceToolReceiptLiveBytesAndIdentityGuards(t *testing.T) {
	s, x, _, _ := newConsolidationTest(t)
	ctx := context.Background()
	req := consolidateExtract(t, s, x, "Inspect structured configuration")
	e := sourceExperience(t, s, x)
	request := sourceExperienceRequest(e)
	got, err := s.Store.Learning.Source(ctx, x.ID, request)
	if err != nil || len(got.References) != 2 || got.ExternalContextPolicy != "hints_only" {
		t.Fatal(got, err)
	}
	var receipt bool
	for _, ref := range got.References {
		receipt = receipt || ref.Kind == "tool_observation" && ref.State == "completed" && ref.ToolName == "read_file" && ref.Version > 0 && !ref.Truncated && strings.Contains(ref.Text, "verified")
	}
	if !receipt {
		t.Fatal("missing saved local observation", got)
	}
	consolidateRun(t, s)
	p := learningReviewPattern(t, s, x)
	patternRequest := harness.SkillLearningSourceRequest{Kind: "pattern", ID: p.ID, ExpectedRevision: p.Revision, Scope: "session", ExperienceID: e.ID}
	if inspected, err := s.Store.Learning.Source(ctx, x.ID, patternRequest); err != nil || len(inspected.References) != 2 || inspected.SourceSessionID != x.ID {
		t.Fatal("private pattern member source unavailable", inspected, err)
	}
	if _, err := s.Store.Learning.SetPatternIgnored(ctx, x.ID, harness.SkillLearningPatternIgnoreRequest{PatternID: p.ID,
		ExpectedPatternRevision: p.Revision, ExpectedStateVersion: p.Review.StateVersion, Scope: "session", RequestKey: "ignore-inspected-source", Ignored: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.Learning.Source(ctx, x.ID, patternRequest); err != nil {
		t.Fatal("ignore deleted or hid retained learning source", err)
	}
	if _, err = s.Store.db.Exec(`UPDATE harness_tool_receipts SET version=version+1 WHERE run_id=?`, req.RunID); err != nil {
		t.Fatal(err)
	}
	if corrupt, err := s.Store.Learning.Source(ctx, x.ID, request); err == nil || len(corrupt.References) != 0 {
		t.Fatal("changed receipt returned stale saved source", corrupt, err)
	}
	if _, err = s.Store.db.Exec(`UPDATE harness_tool_receipts SET version=version-1 WHERE run_id=?`, req.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Store.Learning.Source(ctx, x.ID, request); err != nil {
		t.Fatal(err)
	}
	var original []byte
	if err = s.Store.db.QueryRow(`SELECT payload FROM df_learning_extractions WHERE id=?`, got.ExtractionID).Scan(&original); err != nil {
		t.Fatal(err)
	}
	var snap map[string]any
	_ = json.Unmarshal(original, &snap)
	refs := snap["view"].(map[string]any)["references"].([]any)
	refs[0].(map[string]any)["text"] = "Fabricated cited source text"
	corrupt, _ := json.Marshal(snap)
	if _, err = s.Store.db.Exec(`UPDATE df_learning_extractions SET payload=?,payload_sha=? WHERE id=?`, corrupt, fmt.Sprintf("%x", sha256.Sum256(corrupt)), got.ExtractionID); err != nil {
		t.Fatal(err)
	}
	if got, err = s.Store.Learning.Source(ctx, x.ID, request); err == nil || len(got.References) != 0 {
		t.Fatal("rehashing source payload fabricated source authority", got, err)
	}
}

func TestSkillLearningSourcePatternMembershipWorkspaceConsentAndRevision(t *testing.T) {
	s, x, _, _ := newWorkspaceLearningTest(t, false)
	ctx := context.Background()
	workspaceLearningConsent(t, s, x, true)
	consolidateExtract(t, s, x, "First shared configuration task")
	consolidateRun(t, s)
	first, err := s.Store.Learning.WorkspacePatterns(ctx, x.ID, "", 10)
	if err != nil || len(first.Patterns) != 1 {
		t.Fatal(first, err)
	}
	p := first.Patterns[0]
	oldRequest := harness.SkillLearningSourceRequest{Kind: "pattern", ID: p.ID, ExpectedRevision: p.Revision, Scope: "workspace", ExperienceID: p.RepresentativeID}
	y := workspaceLearningSession(t, s, x.CWD)
	workspaceLearningConsent(t, s, y, true)
	consolidateExtract(t, s, y, "Second shared configuration task")
	consolidateRun(t, s)
	actor := workspaceLearningSession(t, s, x.CWD)
	workspaceLearningConsent(t, s, actor, true)
	page, err := s.Store.Learning.WorkspacePatterns(ctx, actor.ID, "", 10)
	if err != nil || len(page.Patterns) != 1 {
		t.Fatal(page, err)
	}
	p = page.Patterns[0]
	request := oldRequest
	request.ExpectedRevision = p.Revision
	for _, member := range p.ExperienceIDs {
		request.ExperienceID = member
		got, err := s.Store.Learning.Source(ctx, actor.ID, request)
		if err != nil || got.SourceSessionID != x.ID && got.SourceSessionID != y.ID || got.SessionID != actor.ID || got.Source != request || len(got.References) != 2 {
			t.Fatal("shared member source not attributed", got, err)
		}
	}
	if _, err = s.Store.Learning.Source(ctx, actor.ID, oldRequest); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatal("old pattern source silently rebased", err)
	}
	private := workspaceLearningSession(t, s, x.CWD)
	consolidateExtract(t, s, private, "Private configuration task")
	privateExperience := sourceExperience(t, s, private)
	outsider := request
	outsider.ExperienceID = privateExperience.ID
	if _, err = s.Store.Learning.Source(ctx, actor.ID, outsider); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("shared pattern granted unrelated private experience", err)
	}
	for _, session := range []harness.Session{private, workspaceLearningSession(t, s, t.TempDir())} {
		if _, err = s.Store.Learning.Source(ctx, session.ID, request); err == nil {
			t.Fatal("missing consent/workspace granted shared source", session.ID)
		}
	}
	privateScope := request
	privateScope.Scope = "session"
	if _, err = s.Store.Learning.Source(ctx, x.ID, privateScope); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("session scope returned shared member source", err)
	}
	control, _ := s.Store.Learning.Control(ctx, y.ID)
	if _, err = s.SetSkillLearningControl(ctx, "owner", y.ID, harness.SkillLearningControlRequest{ExpectedEpoch: control.Epoch, Contribute: false}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Store.Learning.Source(ctx, actor.ID, request); err == nil || len(got.References) != 0 {
		t.Fatal("revoked transitive pattern source remained readable", got, err)
	}
}

type sourceReadExtractor struct{}

func (*sourceReadExtractor) Identity() string { return "source-reads-extract-fixture-v1" }
func (*sourceReadExtractor) Extract(ctx context.Context, input harness.SkillLearningInput) ([]harness.SkillExperienceCandidate, error) {
	if err := consolidationTestCall(ctx, "experience_extract"); err != nil {
		return nil, err
	}
	refs := make([]string, len(input.References))
	for i, ref := range input.References {
		refs[i] = ref.ID
	}
	return []harness.SkillExperienceCandidate{{Kind: "trigger_mismatch", Goal: "Inspect JSON using the loaded procedure", Applicability: "JSON validation",
		Resolution: "Read complete structured input", Steps: []string{"Read full JSON"}, Verification: "Saved local read observation", Outcome: "observed_execution", References: refs}}, nil
}

func TestSkillLearningSourceActualSkillReadUsesSavedVersionAndInvalidatesChangedRead(t *testing.T) {
	s, x, registry, _, _ := newMatchingTest(t)
	ctx := context.Background()
	if err := s.Store.Learning.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := learning.New(ctx, s.Store.db, learningTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Configure(ctx, &sourceReadExtractor{}, nil); err != nil {
		t.Fatal(err)
	}
	s.Store.Learning = store
	t.Cleanup(func() { _ = store.Close() })
	req := learningTestRun(t, s, x, "Inspect JSON using the loaded skill")
	snapshot, err := registry.Snapshot(ctx, harness.SkillSelection{Workspace: x.CWD})
	if err != nil {
		t.Fatal(err)
	}
	var observation skills.ReadObservation
	if _, err = snapshot.WithReadObserver(func(_ context.Context, read skills.ReadObservation) error { observation = read; return nil }).Get(ctx, "json-check"); err != nil {
		t.Fatal(err)
	}
	tx, err := s.Store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	scope := foregroundBudgetScope(req)
	scope.AttemptID = "source-native-read-fixture"
	if err = recordSkillReadTx(ctx, tx, scope, interaction.SkillRead{RunID: req.RunID, SessionID: x.ID, Read: observation}); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"tool_start", "tool_execute", "tool_end"} {
		event := receiptEvent(req, kind, "source-json", "read_file")
		if kind == "tool_end" {
			event.Content = []harness.Content{{Type: "text", Text: `{"verified":true}`}}
		}
		if _, err = s.Store.Append(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	learningTestFinish(t, s, req, "end_turn")
	learningTestQueue(t, s)
	if worked, err := s.Store.Learning.RunOne(ctx); err != nil || !worked {
		t.Fatal(worked, err)
	}
	e := sourceExperience(t, s, x)
	request := sourceExperienceRequest(e)
	got, err := s.Store.Learning.Source(ctx, x.ID, request)
	if err != nil || len(got.References) != 3 {
		t.Fatal(got, err)
	}
	var found bool
	for _, ref := range got.References {
		if ref.Kind == "skill_read" {
			found = ref.Skill != nil && *ref.Skill == observation.Ref && ref.Path == observation.File.Path && ref.Hash == observation.File.Hash
		}
	}
	if !found {
		t.Fatal("inspection lost actual saved skill read provenance", got, observation)
	}
	if _, err = s.Store.db.Exec(`UPDATE harness_skill_reads SET file_hash=? WHERE run_id=?`, strings.Repeat("a", 64), req.RunID); err != nil {
		t.Fatal(err)
	}
	if got, err = s.Store.Learning.Source(ctx, x.ID, request); err == nil || len(got.References) != 0 {
		t.Fatal("changed actual read returned stale version evidence", got, err)
	}
}
