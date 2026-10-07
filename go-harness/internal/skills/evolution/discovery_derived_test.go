package evolution

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

func derivedFixture(t *testing.T, missing, workflow bool) (*collectorFixture, *fixtureGenerator) {
	t.Helper()
	f := newCollectorFixture(t)
	f.service.validator = &fixtureGenerationValidator{id: "derived-review-v1"}
	g := &fixtureGenerator{id: "derived-generator-v1"}
	g.body = func(_ context.Context, in skillmaintenance.GenerationInput) (skillmaintenance.Candidate, error) {
		if len(in.Evidence) == 0 {
			t.Fatal("missing evidence")
		}
		for _, e := range in.Evidence {
			if strings.Contains(e.Summary, "PRIVATE_") {
				t.Fatalf("private content copied %+v", e)
			}
		}
		return skillmaintenance.Candidate{Reason: "Create reusable verification guidance", Files: map[string][]byte{"SKILL.md": []byte("---\nname: " + in.Target.Name + "\ndescription: Reusable task verification\n---\nVerify each stage before continuing.\n")}}, nil
	}
	configureGeneration(t, f.service, g)
	opts := discoveryOptions()
	opts.DisableOperatorCorrections = true
	opts.MissingReportsEnabled = missing
	opts.SuccessfulWorkflowsEnabled = workflow
	opts.NewSkillSourceID = "project"
	if err := f.service.ConfigureDiscovery(opts); err != nil {
		t.Fatal(err)
	}
	return f, g
}

func TestDerivedDiscoveryMissingWorkflowAndQuality(t *testing.T) {
	t.Run("zero-read missing report generates and withdrawal fences approval", func(t *testing.T) {
		f, g := derivedFixture(t, true, false)
		ctx := context.Background()
		run := f.run(t, "", 0, "completed", false)
		f.drain(t, 128)
		request := missingRequest(run, run.RunID, "missing-first")
		mustSubmitMissing(t, f.service, request)
		discoverySteps(t, f.service, 3)
		items := discoveries(t, f.service)
		if len(items) != 1 || items[0].Kind != "missing_skill" || items[0].Base.Exists || items[0].Evidence[0].ReportRevision != 1 {
			t.Fatalf("missing discovery %+v", items)
		}
		generationRun(t, f.service)
		job, err := f.service.GetGeneration(ctx, items[0].GenerationID)
		if err != nil || job.ProposalID == "" || g.calls.Load() != 1 {
			t.Fatalf("job %+v %v", job, err)
		}
		proposal, err := f.service.Get(ctx, job.ProposalID)
		if err != nil || proposal.Status != "pending_review" || proposal.Trigger.Type != "missing_skill" {
			t.Fatalf("proposal %+v %v", proposal, err)
		}
		request.ExpectedReportVersion = 1
		request.RequestKey = "withdraw"
		request.Kind = "withdrawn"
		request.Summary = ""
		mustSubmitMissing(t, f.service, request)
		if _, err = f.service.Approve(ctx, proposal.ID, Review{ExpectedStateVersion: proposal.StateVersion, Actor: "operator"}); err == nil {
			t.Fatal("withdrawn missing evidence published")
		}
		discoverySteps(t, f.service, 3)
		if len(discoveries(t, f.service)) != 1 {
			t.Fatal("same report consumed twice")
		}
	})
	t.Run("distinct completed roots generate safe workflow", func(t *testing.T) {
		f, g := derivedFixture(t, false, true)
		ctx := context.Background()
		for rootIndex := 0; rootIndex < 2; rootIndex++ {
			run := f.run(t, "", 0, "completed", false)
			for i, name := range []string{"web_search", "web_fetch", "write_file"} {
				receipt := harness.ToolReceipt{SessionID: run.Session.ID, RunID: run.RunID, ToolCallID: fmt.Sprintf("step-%d", i), ToolName: name, ArgumentsDigest: strings.Repeat("a", 64), ArgumentsSummary: `{"query":string}`, State: harness.ReceiptCompleted, Version: 3, Attempt: 1, CreatedAt: time.Now(), UpdatedAt: time.Now(), Result: []harness.Content{{Type: "text", Text: "PRIVATE_OUTPUT_NEVER_COPY"}}}
				data, _ := json.Marshal(receipt)
				if _, err := f.db.DB().Exec(`INSERT INTO harness_tool_receipts(run_id,tool_call_id,session_id,state,version,receipt) VALUES(?,?,?,?,?,?)`, receipt.RunID, receipt.ToolCallID, receipt.SessionID, receipt.State, receipt.Version, data); err != nil {
					t.Fatal(err)
				}
			}
			f.drain(t, 128)
			discoverySteps(t, f.service, 3)
			if rootIndex == 0 && len(discoveries(t, f.service)) != 0 {
				t.Fatal("one root met repetition threshold")
			}
		}
		items := discoveries(t, f.service)
		if len(items) != 1 || items[0].Kind != "repeated_workflow" || len(items[0].Evidence) != 2 {
			t.Fatalf("workflow %+v", items)
		}
		generationRun(t, f.service)
		job, err := f.service.GetGeneration(ctx, items[0].GenerationID)
		if err != nil || g.calls.Load() != 1 || job.ProposalID == "" {
			t.Fatalf("workflow generation %+v %v", job, err)
		}
		p, err := f.service.Get(ctx, job.ProposalID)
		if err != nil || p.Status != "pending_review" {
			t.Fatalf("workflow candidate %+v %v", p, err)
		}
		discoverySteps(t, f.service, 4)
		if len(discoveries(t, f.service)) != 1 {
			t.Fatal("workflow duplicated")
		}
	})
	t.Run("quality requires explicit confirmed current read evidence", func(t *testing.T) {
		f := newCollectorFixture(t)
		ctx := context.Background()
		run := f.run(t, "", 1, "completed", false)
		f.drain(t, 128)
		snapshot := feedbackEvidence(t, f, run)
		request := feedbackRequest(snapshot, "correction")
		for i, kind := range []string{"correction", "quality_failure", "network", "helpful", "withdrawn"} {
			request.Kind = kind
			request.RequestKey = kind
			request.ExpectedFeedbackVersion = int64(i)
			if kind == "withdrawn" {
				request.Summary = ""
				request.ReadIDs = nil
			}
			mustSubmitFeedback(t, f.service, request)
			current, err := f.service.Feedback(ctx, request.SignalID)
			if err != nil {
				t.Fatal(err)
			}
			q := current.Signal.Quality
			want := kind == "quality_failure" || kind == "helpful"
			if q.Eligible != want || want && (q.Confidence != "high" || q.Source != "operator_confirmed") {
				t.Fatalf("kind=%s quality=%+v", kind, q)
			}
		}
		for i, code := range []string{"rate_limit", "remote"} {
			receipt := harness.ToolReceipt{SessionID: run.Session.ID, RunID: run.RunID, ToolCallID: "environment", ToolName: "web_fetch", State: harness.ReceiptFailed, FailureCode: code}
			data, _ := json.Marshal(receipt)
			if _, err := f.db.DB().Exec(`INSERT INTO harness_tool_receipts(run_id,tool_call_id,session_id,state,version,receipt) VALUES(?,?,?,?,1,?) ON CONFLICT(run_id,tool_call_id) DO UPDATE SET receipt=excluded.receipt`, run.RunID, receipt.ToolCallID, run.Session.ID, receipt.State, data); err != nil {
				t.Fatal(err)
			}
			request = feedbackRequest(snapshot, "quality_failure")
			request.RequestKey = "environment-" + code
			request.ExpectedFeedbackVersion = int64(5 + i)
			mustSubmitFeedback(t, f.service, request)
			current, err := f.service.Feedback(ctx, request.SignalID)
			if err != nil || current.Signal.Quality.Eligible || current.Signal.Quality.Kind != code {
				t.Fatalf("environment=%s quality=%+v error=%v", code, current.Signal.Quality, err)
			}
		}
	})
}
