package evolution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

func feedbackEvidence(t *testing.T, f *collectorFixture, run harness.RunRequest) harness.SkillSignalEvidence {
	t.Helper()
	id := signalID(run.RootBudgetID, f.read.Ref.SourceIdentity, f.read.Ref.Name, f.read.Ref.Version)
	evidence, err := f.service.SignalEvidence(context.Background(), id, "", 100)
	if err != nil || len(evidence.Reads) == 0 {
		t.Fatalf("feedback evidence: %+v %v", evidence, err)
	}
	return evidence
}

func feedbackRequest(evidence harness.SkillSignalEvidence, key string) harness.SkillFeedbackRequest {
	return harness.SkillFeedbackRequest{
		SignalID: evidence.Signal.ID, ExpectedSignalStateVersion: evidence.Signal.StateVersion,
		ExpectedFeedbackVersion: evidence.Signal.FeedbackRevision, RequestKey: key,
		Kind: "correction", Summary: "The instructions need a verification step.",
		ReadIDs: []int64{evidence.Reads[0].ID},
	}
}

func mustSubmitFeedback(t *testing.T, s *Service, request harness.SkillFeedbackRequest) harness.SkillFeedbackResult {
	t.Helper()
	result, err := s.SubmitFeedback(context.Background(), request)
	if err != nil {
		t.Fatalf("submit feedback: %+v %v", result, err)
	}
	return result
}

func TestFeedbackBindsObservedVersionAndDoesNotAssessOrPublish(t *testing.T) {
	f := newCollectorFixture(t)
	ctx := context.Background()
	run := f.run(t, "", 2, "completed", false)
	f.run(t, run.RootBudgetID, 1, "failed", true)
	f.drain(t, 128)
	evidence := feedbackEvidence(t, f, run)
	beforeHead := f.head(t)
	before, err := f.service.Feedback(ctx, evidence.Signal.ID)
	if err != nil || before.Feedback != nil || before.Signal.FeedbackRevision != 0 {
		t.Fatalf("initial feedback: %+v %v", before, err)
	}
	request := feedbackRequest(evidence, "observed-version")
	request.ReadIDs = []int64{evidence.Reads[0].ID, evidence.Reads[2].ID}
	result := mustSubmitFeedback(t, f.service, request)
	got := result.Feedback
	if result.Replayed || result.CurrentRevision != 1 || got.Revision != 1 || got.SignalID != evidence.Signal.ID ||
		got.Target != evidence.Signal.Target || got.SkillVersion != evidence.Signal.Version || got.RootID != run.RootBudgetID ||
		got.RootSessionID != run.Session.ID || got.SignalStateVersion != evidence.Signal.StateVersion || got.Origin != "operator" ||
		got.Kind != request.Kind || got.Summary != request.Summary || got.SummaryRedacted || got.CreatedAt == "" || !reflect.DeepEqual(got.ReadIDs, request.ReadIDs) {
		t.Fatalf("feedback lost its observed identity: %+v", result)
	}
	current, err := f.service.Feedback(ctx, request.SignalID)
	if err != nil || current.Feedback == nil || !reflect.DeepEqual(*current.Feedback, got) || !current.EvidenceCurrent ||
		current.Signal.FeedbackRevision != 1 || current.Signal.StateVersion != evidence.Signal.StateVersion || current.Signal.Attribution != "unassessed" {
		t.Fatalf("feedback changed evidence/attribution: %+v %v", current, err)
	}
	if f.head(t) != beforeHead || f.calls.Load() != 0 {
		t.Fatal("feedback invoked validation or changed a published skill")
	}
	var proposals, jobs int
	if err = f.db.DB().QueryRow(`SELECT count(*) FROM df_skill_proposals`).Scan(&proposals); err != nil || proposals != 0 {
		t.Fatalf("feedback created proposals: %d %v", proposals, err)
	}
	if err = f.db.DB().QueryRow(`SELECT count(*) FROM df_skill_generation_inputs`).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("feedback enqueued generation: %d %v", jobs, err)
	}
}

func TestFeedbackRejectsUnobservedAndCrossSignalReads(t *testing.T) {
	f := newCollectorFixture(t)
	ctx := context.Background()
	first := f.run(t, "", 2, "completed", false)
	other := f.run(t, "", 1, "completed", false)
	f.drain(t, 128)
	evidence, foreign := feedbackEvidence(t, f, first), feedbackEvidence(t, f, other)
	cases := []struct {
		name string
		edit func(*harness.SkillFeedbackRequest)
	}{
		{"foreign_read", func(r *harness.SkillFeedbackRequest) { r.ReadIDs = []int64{foreign.Reads[0].ID} }},
		{"mixed_reads", func(r *harness.SkillFeedbackRequest) { r.ReadIDs = append(r.ReadIDs, foreign.Reads[0].ID) }},
		{"missing_read", func(r *harness.SkillFeedbackRequest) { r.ReadIDs = []int64{1 << 50} }},
		{"duplicate_read", func(r *harness.SkillFeedbackRequest) { r.ReadIDs = append(r.ReadIDs, r.ReadIDs[0]) }},
		{"zero_read", func(r *harness.SkillFeedbackRequest) { r.ReadIDs = []int64{0} }},
		{"empty_reads", func(r *harness.SkillFeedbackRequest) { r.ReadIDs = nil }},
		{"too_many_reads", func(r *harness.SkillFeedbackRequest) {
			r.ReadIDs = make([]int64, 17)
			for i := range r.ReadIDs {
				r.ReadIDs[i] = int64(i + 1)
			}
		}},
		{"empty_summary", func(r *harness.SkillFeedbackRequest) { r.Summary = " \t\n" }},
		{"long_summary", func(r *harness.SkillFeedbackRequest) { r.Summary = strings.Repeat("中", 667) }},
		{"oversize_before_trim", func(r *harness.SkillFeedbackRequest) { r.Summary = strings.Repeat(" ", 2000) + "valid" }},
		{"redaction_expands_beyond_limit", func(r *harness.SkillFeedbackRequest) { r.Summary = strings.TrimSpace(strings.Repeat("token=x ", 240)) }},
		{"summary_nul", func(r *harness.SkillFeedbackRequest) { r.Summary += "\x00" }},
		{"summary_format_control", func(r *harness.SkillFeedbackRequest) { r.Summary += "\u200b" }},
		{"summary_invalid_utf8", func(r *harness.SkillFeedbackRequest) { r.Summary += string([]byte{0xff}) }},
		{"unknown_kind", func(r *harness.SkillFeedbackRequest) { r.Kind = "skill_quality_failure" }},
		{"missing_key", func(r *harness.SkillFeedbackRequest) { r.RequestKey = "" }},
		{"key_whitespace", func(r *harness.SkillFeedbackRequest) { r.RequestKey = "feedback key" }},
		{"key_control", func(r *harness.SkillFeedbackRequest) { r.RequestKey = "feedback\nkey" }},
		{"key_non_ascii", func(r *harness.SkillFeedbackRequest) { r.RequestKey = "反馈键" }},
		{"negative_feedback_version", func(r *harness.SkillFeedbackRequest) { r.ExpectedFeedbackVersion = -1 }},
		{"zero_evidence_version", func(r *harness.SkillFeedbackRequest) { r.ExpectedSignalStateVersion = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := feedbackRequest(evidence, tc.name)
			tc.edit(&request)
			if _, err := f.service.SubmitFeedback(ctx, request); err == nil {
				t.Fatal("invalid evidence or feedback intent was persisted")
			}
		})
	}
	current, err := f.service.Feedback(ctx, evidence.Signal.ID)
	if err != nil || current.Feedback != nil || current.Signal.FeedbackRevision != 0 {
		t.Fatalf("rejected feedback left state: %+v %v", current, err)
	}
}

func TestFeedbackIdempotencySurvivesCollectionAndLaterJudgments(t *testing.T) {
	f := newCollectorFixture(t)
	ctx := context.Background()
	run := f.run(t, "", 1, "completed", false)
	f.drain(t, 128)
	evidence := feedbackEvidence(t, f, run)
	request := feedbackRequest(evidence, "stable-request")
	first := mustSubmitFeedback(t, f.service, request)
	changed := request
	changed.Summary = "A different correction under the original request key."
	if _, err := f.service.SubmitFeedback(ctx, changed); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatal("same key changed intent", err)
	}
	f.run(t, run.RootBudgetID, 1, "completed", false)
	f.drain(t, 128)
	current, err := f.service.Feedback(ctx, request.SignalID)
	if err != nil || current.EvidenceCurrent || current.Signal.StateVersion == evidence.Signal.StateVersion {
		t.Fatalf("new evidence did not invalidate assessment snapshot: %+v %v", current, err)
	}
	stale := request
	stale.RequestKey = "stale-evidence"
	stale.ExpectedFeedbackVersion = 1
	if _, err := f.service.SubmitFeedback(ctx, stale); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatal("new feedback ignored evidence CAS", err)
	}
	changed = request
	changed.RequestKey = "updated-judgment"
	changed.ExpectedFeedbackVersion = 1
	changed.ExpectedSignalStateVersion = current.Signal.StateVersion
	changed.Kind = "helpful"
	changed.Summary = "The corrected workflow helped."
	second := mustSubmitFeedback(t, f.service, changed)
	if second.Feedback.Revision != 2 || second.CurrentRevision != 2 {
		t.Fatalf("replacement judgment: %+v", second)
	}
	again := mustSubmitFeedback(t, f.service, request)
	if !again.Replayed || again.CurrentRevision != 2 || !reflect.DeepEqual(again.Feedback, first.Feedback) {
		t.Fatalf("lost response replay rewrote an earlier revision: %+v", again)
	}
	current, err = f.service.Feedback(ctx, request.SignalID)
	if err != nil || current.Feedback == nil || !reflect.DeepEqual(*current.Feedback, second.Feedback) || !current.EvidenceCurrent {
		t.Fatalf("replay became current feedback: %+v %v", current, err)
	}
}

func TestFeedbackConcurrentWritersUseFeedbackCAS(t *testing.T) {
	f := newCollectorFixture(t)
	ctx := context.Background()
	run := f.run(t, "", 1, "completed", false)
	f.drain(t, 128)
	evidence := feedbackEvidence(t, f, run)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			request := feedbackRequest(evidence, fmt.Sprintf("writer-%d", i))
			<-start
			_, err := f.service.SubmitFeedback(ctx, request)
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	wins, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, harness.ErrSkillConflict):
			conflicts++
		default:
			t.Fatal("feedback race returned unexpected error", err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("concurrent feedback wins=%d conflicts=%d", wins, conflicts)
	}
	page, err := f.service.QueryFeedback(ctx, evidence.Signal.ID, "", 100)
	if err != nil || len(page.Revisions) != 1 || page.CurrentRevision != 1 {
		t.Fatalf("concurrent writes left duplicate votes: %+v %v", page, err)
	}
}

func TestFeedbackWithdrawalRequiresExistingJudgmentAndPreservesHistory(t *testing.T) {
	f := newCollectorFixture(t)
	ctx := context.Background()
	run := f.run(t, "", 1, "completed", false)
	f.drain(t, 128)
	evidence := feedbackEvidence(t, f, run)
	withdraw := feedbackRequest(evidence, "withdraw-empty")
	withdraw.Kind, withdraw.Summary, withdraw.ReadIDs = "withdrawn", "", nil
	if _, err := f.service.SubmitFeedback(ctx, withdraw); err == nil {
		t.Fatal("withdrew a judgment that does not exist")
	}
	first := mustSubmitFeedback(t, f.service, feedbackRequest(evidence, "before-withdraw"))
	withdraw.ExpectedFeedbackVersion = first.CurrentRevision
	withdraw.RequestKey = "withdraw"
	for _, fault := range []string{"text", "read"} {
		bad := withdraw
		bad.RequestKey = fault
		if fault == "text" {
			bad.Summary = "Not permitted in a tombstone."
		} else {
			bad.ReadIDs = []int64{evidence.Reads[0].ID}
		}
		if _, err := f.service.SubmitFeedback(ctx, bad); err == nil {
			t.Fatalf("withdrawal retained %s", fault)
		}
	}
	result := mustSubmitFeedback(t, f.service, withdraw)
	if result.Feedback.Revision != 2 || result.Feedback.Kind != "withdrawn" || result.Feedback.Summary != "" || len(result.Feedback.ReadIDs) != 0 {
		t.Fatalf("withdrawal: %+v", result)
	}
	page, err := f.service.QueryFeedback(ctx, evidence.Signal.ID, "", 100)
	if err != nil || page.CurrentRevision != 2 || len(page.Revisions) != 2 || !reflect.DeepEqual(page.Revisions[0], first.Feedback) || page.Revisions[1].Kind != "withdrawn" {
		t.Fatalf("withdrawal erased immutable history: %+v %v", page, err)
	}
	current, err := f.service.Feedback(ctx, evidence.Signal.ID)
	if err != nil || current.Signal.FeedbackRevision != 2 || current.Signal.Attribution != "unassessed" {
		t.Fatalf("withdrawal reset the feedback clock: %+v %v", current, err)
	}
	stale := feedbackRequest(evidence, "stale-after-withdraw")
	if _, err := f.service.SubmitFeedback(ctx, stale); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatal("withdrawal permitted a stale first-write CAS", err)
	}
	again := mustSubmitFeedback(t, f.service, withdraw)
	if !again.Replayed || !reflect.DeepEqual(again.Feedback, result.Feedback) {
		t.Fatalf("withdrawal replay: %+v", again)
	}
}

func TestFeedbackHistoryCursorsBindSignalAndFeedbackRevision(t *testing.T) {
	f := newCollectorFixture(t)
	ctx := context.Background()
	run := f.run(t, "", 2, "completed", false)
	other := f.run(t, "", 1, "completed", false)
	f.drain(t, 128)
	evidence, foreign := feedbackEvidence(t, f, run), feedbackEvidence(t, f, other)
	reads, err := f.service.SignalEvidence(ctx, evidence.Signal.ID, "", 1)
	if err != nil || reads.Next == "" {
		t.Fatalf("read pagination fixture: %+v %v", reads, err)
	}
	signals, err := f.service.QuerySignals(ctx, harness.SkillQuery{Limit: 1})
	if err != nil || signals.Next == "" {
		t.Fatalf("signal page fixture: %+v %v", signals, err)
	}
	for i := int64(0); i < 3; i++ {
		request := feedbackRequest(evidence, fmt.Sprintf("revision-%d", i))
		request.ExpectedFeedbackVersion = i
		mustSubmitFeedback(t, f.service, request)
	}
	if _, err := f.service.QuerySignals(ctx, harness.SkillQuery{Limit: 1, Cursor: signals.Next}); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatal("feedback changed a list projection without invalidating its cursor", err)
	}
	if _, err := f.service.SignalEvidence(ctx, evidence.Signal.ID, reads.Next, 1); err != nil {
		t.Fatal("feedback invalidated unchanged read evidence cursor", err)
	}
	page, err := f.service.QueryFeedback(ctx, evidence.Signal.ID, "", 1)
	if err != nil || len(page.Revisions) != 1 || page.Revisions[0].Revision != 1 || page.CurrentRevision != 3 || page.Next == "" {
		t.Fatalf("first history page: %+v %v", page, err)
	}
	if _, err := f.service.QueryFeedback(ctx, foreign.Signal.ID, page.Next, 1); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatal("cross-signal cursor accepted", err)
	}
	second, err := f.service.QueryFeedback(ctx, evidence.Signal.ID, page.Next, 2)
	if err != nil || len(second.Revisions) != 2 || second.Revisions[0].Revision != 2 || second.Revisions[1].Revision != 3 || second.Next != "" {
		t.Fatalf("history order/boundaries: %+v %v", second, err)
	}
	request := feedbackRequest(evidence, "after-pagination")
	request.ExpectedFeedbackVersion = 3
	mustSubmitFeedback(t, f.service, request)
	if _, err := f.service.QueryFeedback(ctx, evidence.Signal.ID, page.Next, 1); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatal("stale feedback revision cursor accepted", err)
	}
	for _, limit := range []int{-1, 0, 101} {
		if _, err := f.service.QueryFeedback(ctx, evidence.Signal.ID, "", limit); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatalf("invalid page limit %d accepted: %v", limit, err)
		}
	}
}

func TestFeedbackRebindingRevokesQueryAndIdempotentReplay(t *testing.T) {
	f := newCollectorFixture(t)
	ctx := context.Background()
	run := f.run(t, "", 1, "completed", false)
	f.drain(t, 128)
	evidence := feedbackEvidence(t, f, run)
	request := feedbackRequest(evidence, "before-source-rebind")
	mustSubmitFeedback(t, f.service, request)
	for _, mode := range []string{"removed", "root", "workspace"} {
		t.Run(mode, func(t *testing.T) {
			cfg := f.cfg
			cfg.Sources = append([]harness.SkillSource(nil), cfg.Sources...)
			switch mode {
			case "removed":
				cfg.Sources = nil
			case "root":
				cfg.Sources[0].Root = filepath.Join(cfg.Sources[0].Workspace, "rebound-skills")
				if err := os.MkdirAll(cfg.Sources[0].Root, 0700); err != nil {
					t.Fatal(err)
				}
			case "workspace":
				cfg.Sources[0].Workspace = filepath.Dir(cfg.Sources[0].Workspace)
			}
			registry, err := skills.NewRegistry(ctx, cfg, f.db.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer registry.Close()
			service, err := New(ctx, registry, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer service.Close()
			if _, err := service.Feedback(ctx, request.SignalID); !errors.Is(err, harness.ErrPermissionDenied) {
				t.Fatal("rebound source read feedback", err)
			}
			if _, err := service.QueryFeedback(ctx, request.SignalID, "", 10); !errors.Is(err, harness.ErrPermissionDenied) {
				t.Fatal("rebound source read history", err)
			}
			if _, err := service.SubmitFeedback(ctx, request); !errors.Is(err, harness.ErrPermissionDenied) {
				t.Fatal("idempotent replay bypassed revoked source authorization", err)
			}
		})
	}
}

func TestFeedbackAllJudgmentsRemainUnassessedAndRestartIsDurable(t *testing.T) {
	f := newCollectorFixture(t)
	ctx := context.Background()
	run := f.run(t, "", 1, "cancelled", false)
	f.drain(t, 128)
	evidence := feedbackEvidence(t, f, run)
	var last harness.SkillFeedback
	for i, kind := range []string{"helpful", "correction", "network", "quota", "permission", "cancelled", "other"} {
		request := feedbackRequest(evidence, kind)
		request.ExpectedFeedbackVersion = int64(i)
		request.Kind = kind
		last = mustSubmitFeedback(t, f.service, request).Feedback
		current, err := f.service.Feedback(ctx, evidence.Signal.ID)
		if err != nil || current.Signal.Attribution != "unassessed" || current.Signal.CancelledRuns != 1 || current.Signal.FailedRuns != 0 {
			t.Fatalf("%s judged skill causality from user feedback: %+v %v", kind, current, err)
		}
	}
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(ctx, f.registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	current, err := reopened.Feedback(ctx, evidence.Signal.ID)
	if err != nil || current.Feedback == nil || !reflect.DeepEqual(*current.Feedback, last) || current.Signal.FeedbackRevision != 7 {
		t.Fatalf("reopened feedback: %+v %v", current, err)
	}
	page, err := reopened.QueryFeedback(ctx, evidence.Signal.ID, "", 100)
	if err != nil || len(page.Revisions) != 7 || page.CurrentRevision != 7 {
		t.Fatalf("reopened history: %+v %v", page, err)
	}
	if f.calls.Load() != 0 {
		t.Fatal("judgment history called a model")
	}
}

func TestFeedbackRedactsSummaryWithoutCollapsingDistinctRequests(t *testing.T) {
	f := newCollectorFixture(t)
	ctx := context.Background()
	run := f.run(t, "", 1, "completed", false)
	f.drain(t, 128)
	evidence := feedbackEvidence(t, f, run)
	request := feedbackRequest(evidence, "sensitive-summary")
	request.Summary = "The fix used api_key=feedback-secret-123 and Authorization: Bearer feedback-bearer-456 at https://example.invalid/private?token=feedback-url-789"
	result := mustSubmitFeedback(t, f.service, request)
	if !result.Feedback.SummaryRedacted || result.Feedback.Summary == "" {
		t.Fatalf("summary was not visibly redacted: %+v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"feedback-secret-123", "feedback-bearer-456", "feedback-url-789", "https://example.invalid/private"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("feedback leaked %s", secret)
		}
	}
	var requestKey, requestSHA, payloadSHA string
	var payload []byte
	if err := f.db.DB().QueryRow(`SELECT request_key,request_sha,payload,payload_sha FROM df_skill_feedback WHERE signal_id=? AND revision=1`, request.SignalID).Scan(&requestKey, &requestSHA, &payload, &payloadSHA); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"feedback-secret-123", "feedback-bearer-456", "feedback-url-789", "https://example.invalid/private"} {
		if strings.Contains(requestKey+requestSHA+string(payload)+payloadSHA, secret) {
			t.Fatalf("feedback retained raw summary secret in storage: %s", secret)
		}
	}
	changed := request
	changed.Summary = strings.ReplaceAll(changed.Summary, "feedback-secret-123", "different-secret-123")
	if _, err := f.service.SubmitFeedback(ctx, changed); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatal("redaction collapsed different raw intents under one request key", err)
	}
	again := mustSubmitFeedback(t, f.service, request)
	if !again.Replayed || !reflect.DeepEqual(again.Feedback, result.Feedback) {
		t.Fatalf("redacted request did not replay: %+v", again)
	}
}

func TestFeedbackCanonicalizesSummaryAndReadOrderWithoutMutatingCaller(t *testing.T) {
	f := newCollectorFixture(t)
	run := f.run(t, "", 2, "completed", false)
	f.drain(t, 128)
	evidence := feedbackEvidence(t, f, run)
	request := feedbackRequest(evidence, "canonical-request")
	request.Summary = " \t" + request.Summary + "\n"
	request.ReadIDs = []int64{evidence.Reads[1].ID, evidence.Reads[0].ID}
	first := mustSubmitFeedback(t, f.service, request)
	if first.Feedback.Summary != strings.TrimSpace(request.Summary) || !reflect.DeepEqual(first.Feedback.ReadIDs, []int64{evidence.Reads[0].ID, evidence.Reads[1].ID}) {
		t.Fatalf("canonical feedback: %+v", first)
	}
	if request.ReadIDs[0] != evidence.Reads[1].ID {
		t.Fatal("feedback sorting mutated the caller's read slice")
	}
	request.Summary = strings.TrimSpace(request.Summary)
	request.ReadIDs = []int64{evidence.Reads[0].ID, evidence.Reads[1].ID}
	again := mustSubmitFeedback(t, f.service, request)
	if !again.Replayed || !reflect.DeepEqual(again.Feedback, first.Feedback) {
		t.Fatalf("equivalent canonical request conflicted: %+v", again)
	}
}

func TestFeedbackTransactionFailureLeavesTerminalRunAndRequestRetryable(t *testing.T) {
	f := newCollectorFixture(t)
	ctx := context.Background()
	run := f.run(t, "", 1, "completed", false)
	f.drain(t, 128)
	evidence := feedbackEvidence(t, f, run)
	request := feedbackRequest(evidence, "atomic-request")
	var beforeRevision int64
	if err := f.db.DB().QueryRow(`SELECT revision FROM df_skill_signal_catalog WHERE id=1`).Scan(&beforeRevision); err != nil {
		t.Fatal(err)
	}
	// This fault fires from the feedback INSERT's catalog trigger, after the
	// feedback row itself has been written. Both effects must roll back together.
	if _, err := f.db.DB().Exec(`CREATE TRIGGER feedback_catalog_fault BEFORE UPDATE ON df_skill_signal_catalog BEGIN SELECT RAISE(ABORT,'fixture-feedback-storage-failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.SubmitFeedback(ctx, request); err == nil {
		t.Fatal("injected feedback transaction failure was ignored")
	}
	current, err := f.service.Feedback(ctx, request.SignalID)
	if err != nil || current.Feedback != nil || current.Signal.FeedbackRevision != 0 || current.Signal.StateVersion != evidence.Signal.StateVersion {
		t.Fatalf("failed transaction left current feedback: %+v %v", current, err)
	}
	var rows int
	var afterRevision int64
	if err = f.db.DB().QueryRow(`SELECT count(*) FROM df_skill_feedback`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("failed transaction retained request/history: %d %v", rows, err)
	}
	if err = f.db.DB().QueryRow(`SELECT revision FROM df_skill_signal_catalog WHERE id=1`).Scan(&afterRevision); err != nil || afterRevision != beforeRevision {
		t.Fatalf("failed transaction changed catalog: %d -> %d %v", beforeRevision, afterRevision, err)
	}
	var runStatus, outcomeStatus string
	var processed int
	if err = f.db.DB().QueryRow(`SELECT r.status,o.status,o.processed FROM harness_runs r JOIN harness_skill_outcomes o ON o.run_id=r.id WHERE r.id=?`, run.RunID).Scan(&runStatus, &outcomeStatus, &processed); err != nil || runStatus != "completed" || outcomeStatus != "completed" || processed != 1 {
		t.Fatalf("feedback storage fault changed terminal run: %s %s %d %v", runStatus, outcomeStatus, processed, err)
	}
	if _, err = f.db.DB().Exec(`DROP TRIGGER feedback_catalog_fault`); err != nil {
		t.Fatal(err)
	}
	retry := mustSubmitFeedback(t, f.service, request)
	if retry.Replayed || retry.CurrentRevision != 1 {
		t.Fatalf("rolled-back request was treated as committed: %+v", retry)
	}
	// A subsequent task may still finish and be collected normally.
	f.run(t, "", 1, "completed", false)
	f.drain(t, 128)
	if f.calls.Load() != 0 {
		t.Fatal("feedback recovery invoked a model")
	}
}

func TestFeedbackSessionDeletionCascadesOnlyItsPrivateHistory(t *testing.T) {
	f := newCollectorFixture(t)
	ctx := context.Background()
	first := f.run(t, "", 1, "completed", false)
	second := f.run(t, "", 1, "completed", false)
	f.drain(t, 128)
	a, b := feedbackEvidence(t, f, first), feedbackEvidence(t, f, second)
	aRequest, bRequest := feedbackRequest(a, "delete-me"), feedbackRequest(b, "keep-me")
	mustSubmitFeedback(t, f.service, aRequest)
	bSaved := mustSubmitFeedback(t, f.service, bRequest)
	// Exercise the production session purge, including its source-signal delete
	// and the feedback foreign-key cascade, instead of manually deleting feedback.
	runtime := hr.NewService(f.runtime, nil, "fixture")
	if already, err := runtime.DeleteSession(ctx, first.Session.ID); err != nil || already {
		t.Fatalf("session deletion: already=%v %v", already, err)
	}
	if _, err := f.service.Feedback(ctx, a.Signal.ID); !errors.Is(err, harness.ErrNotFound) {
		t.Fatal("deleted session retained feedback detail", err)
	}
	if _, err := f.service.QueryFeedback(ctx, a.Signal.ID, "", 100); !errors.Is(err, harness.ErrNotFound) {
		t.Fatal("deleted session retained feedback history", err)
	}
	if _, err := f.service.SubmitFeedback(ctx, aRequest); !errors.Is(err, harness.ErrNotFound) {
		t.Fatal("deleted session request replay revived feedback", err)
	}
	var total, deleted int
	if err := f.db.DB().QueryRow(`SELECT count(*),count(CASE WHEN signal_id=? THEN 1 END) FROM df_skill_feedback`, a.Signal.ID).Scan(&total, &deleted); err != nil || total != 1 || deleted != 0 {
		t.Fatalf("session feedback cascade total=%d deleted=%d %v", total, deleted, err)
	}
	bCurrent, err := f.service.Feedback(ctx, b.Signal.ID)
	if err != nil || bCurrent.Feedback == nil || !reflect.DeepEqual(*bCurrent.Feedback, bSaved.Feedback) {
		t.Fatalf("session deletion changed unrelated feedback: %+v %v", bCurrent, err)
	}
}

func TestFeedbackRevisionLimitReservesWithdrawalAndAllowsOldReplay(t *testing.T) {
	f := newCollectorFixture(t)
	ctx := context.Background()
	run := f.run(t, "", 1, "completed", false)
	f.drain(t, 128)
	evidence := feedbackEvidence(t, f, run)
	firstRequest := feedbackRequest(evidence, "limit-0")
	first := mustSubmitFeedback(t, f.service, firstRequest)
	for i := int64(1); i < 999; i++ {
		request := feedbackRequest(evidence, fmt.Sprintf("limit-%d", i))
		request.ExpectedFeedbackVersion = i
		result := mustSubmitFeedback(t, f.service, request)
		if result.CurrentRevision != i+1 {
			t.Fatalf("revision limit fixture lost a write: %+v", result)
		}
	}
	blocked := feedbackRequest(evidence, "limit-blocked")
	blocked.ExpectedFeedbackVersion = 999
	if _, err := f.service.SubmitFeedback(ctx, blocked); !errors.Is(err, harness.ErrSkillFeedbackLimit) {
		t.Fatal("normal feedback consumed the withdrawal reserve", err)
	}
	withdraw := blocked
	withdraw.RequestKey, withdraw.Kind, withdraw.Summary, withdraw.ReadIDs = "limit-withdraw", "withdrawn", "", nil
	last := mustSubmitFeedback(t, f.service, withdraw)
	if last.CurrentRevision != 1000 || last.Feedback.Kind != "withdrawn" {
		t.Fatalf("feedback at limit cannot be withdrawn: %+v", last)
	}
	blocked.ExpectedFeedbackVersion = 1000
	if _, err := f.service.SubmitFeedback(ctx, blocked); !errors.Is(err, harness.ErrSkillFeedbackLimit) {
		t.Fatal("feedback exceeded the durable history bound", err)
	}
	again := mustSubmitFeedback(t, f.service, firstRequest)
	if !again.Replayed || again.CurrentRevision != 1000 || !reflect.DeepEqual(again.Feedback, first.Feedback) {
		t.Fatalf("history limit broke idempotent request replay: %+v", again)
	}
}
