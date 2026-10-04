package deerflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type discoveryProvider struct {
	userCalls        atomic.Int32
	maintenanceCalls atomic.Int32
	generated        chan []harness.SkillGenerationEvidence
}

// Real foreground Eino loops use the skill tool. Independent tool-free model
// routes exercise the production generation, moderation and evaluation budget.
func skillDiscoveryFixture(t *testing.T) (string, Config, *discoveryProvider) {
	t.Helper()
	workspace, cfg := skillFixture(t)
	provider := &discoveryProvider{generated: make(chan []harness.SkillGenerationEvidence, 8)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model    string `json:"model"`
			Stream   bool   `json:"stream"`
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
			Tools []json.RawMessage `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "invalid fixture request", http.StatusBadRequest)
			return
		}
		message := map[string]any{"role": "assistant", "content": "The research skill was read."}
		finish := "stop"
		if request.Model == "user" {
			call := provider.userCalls.Add(1)
			if len(request.Messages) == 0 {
				t.Error("user route received no conversation")
				http.Error(w, "missing messages", http.StatusBadRequest)
				return
			}
			if request.Messages[len(request.Messages)-1].Role != "tool" {
				message["content"] = nil
				message["tool_calls"] = []any{map[string]any{"index": 0, "id": fmt.Sprintf("discovery-read-%d", call), "type": "function", "function": map[string]string{"name": "skill", "arguments": `{"skill":"research"}`}}}
				finish = "tool_calls"
			}
		} else {
			provider.maintenanceCalls.Add(1)
			if len(request.Tools) != 0 || len(request.Messages) != 2 || request.Stream {
				t.Error("automatic maintenance received user tools/history or streaming")
				http.Error(w, "maintenance isolation violation", http.StatusBadRequest)
				return
			}
			var content string
			var input struct {
				SkillName        string                            `json:"skill_name"`
				BaseVersion      string                            `json:"base_version"`
				CandidateVersion string                            `json:"candidate_version"`
				Evidence         []harness.SkillGenerationEvidence `json:"evidence"`
			}
			if err := json.Unmarshal(request.Messages[1].Content, &content); err != nil {
				t.Error(err)
				return
			}
			if err := json.Unmarshal([]byte(content), &input); err != nil {
				t.Error(err)
				return
			}
			var answer []byte
			if request.Model == "generator" {
				select {
				case provider.generated <- input.Evidence:
				default:
					t.Error("automatic discovery made excessive generation calls")
				}
				answer, _ = json.Marshal(map[string]any{"skill_name": input.SkillName, "base_version": input.BaseVersion, "reason": "Check dates and source claims before summarizing.", "files": map[string]string{"SKILL.md": "---\nname: research\ndescription: Research a source\n---\nVerify source dates and claims. Read [guide](guide.txt).\n", "guide.txt": "PINNED_REFERENCE"}})
			} else if request.Model == "moderator" || request.Model == "evaluator" {
				answer, _ = json.Marshal(map[string]any{"base_version": input.BaseVersion, "candidate_version": input.CandidateVersion, "passed": true, "reason": "Fixture content review passed."})
			} else {
				t.Errorf("unexpected maintenance route: %q", request.Model)
				http.Error(w, "unknown route", http.StatusBadRequest)
				return
			}
			message["content"] = string(answer)
		}
		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			body, _ := json.Marshal(map[string]any{"id": "discovery-fixture", "choices": []any{map[string]any{"index": 0, "delta": message, "finish_reason": finish}}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", body)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "discovery-fixture", "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}, "usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 25, "total_tokens": 125}})
	}))
	t.Cleanup(server.Close)
	cfg.Provider, cfg.Model, cfg.BaseURL, cfg.APIKey = "openai", "user", server.URL+"/v1", "user-fixture-key"
	cfg.Skills.Install = []harness.SkillInstall{{SourceID: "project", Directory: "research", Enabled: true}}
	route := func(name string) harness.ModelRoute {
		return harness.ModelRoute{Model: name, BaseURL: server.URL + "/v1", APIKey: "discovery-private-key"}
	}
	var err error
	cfg.SkillValidator, err = NewSkillModelValidator(context.Background(), harness.SkillModelReviewConfig{Moderation: route("moderator"), Evaluation: route("evaluator"), MaxOutputTokens: 512})
	if err != nil {
		t.Fatal(err)
	}
	cfg.SkillGeneration = &harness.SkillGenerationOptions{Model: harness.SkillGenerationModelConfig{Route: route("generator")}, Budget: harness.SkillMaintenanceBudget{MaxDailyModelCalls: 12, MaxDailyTokens: 100000, MaxJobSeconds: 60, Workers: 1}, Sources: []string{"project"}}
	return workspace, cfg, provider
}

func discoveryOptions() *harness.SkillDiscoveryOptions {
	return &harness.SkillDiscoveryOptions{RepeatThreshold: 2, RepeatWindowDays: 7, CooldownHours: 24, MaxDailyProposals: 2, MaxPendingProposals: 2}
}

func waitDiscoverySignal(t *testing.T, ctx context.Context, c *Client, runID string) harness.SkillSignalEvidence {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		page, err := c.ListSkillSignals(ctx, harness.SkillQuery{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		for _, signal := range page.Signals {
			if signal.RootID != runID || signal.CollectionPending || signal.ReadCount != 1 {
				continue
			}
			evidence, err := c.GetSkillSignalEvidence(ctx, signal.ID, "", 10)
			if err != nil || len(evidence.Reads) != 1 || evidence.Reads[0].RunID != runID || signal.CompletedRuns != 1 || signal.UncertainRuns != 0 || signal.Attribution != "unassessed" {
				t.Fatalf("real settled skill evidence: %+v %v", evidence, err)
			}
			return evidence
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-timer.C:
			t.Fatalf("real root did not settle: run=%s page=%+v", runID, page)
		case <-ticker.C:
		}
	}
}

func discoveryFeedback(t *testing.T, ctx context.Context, c *Client, evidence harness.SkillSignalEvidence, version int64, summary string) harness.SkillFeedbackResult {
	t.Helper()
	result, err := c.SubmitSkillFeedback(ctx, harness.SkillFeedbackRequest{SignalID: evidence.Signal.ID, ExpectedSignalStateVersion: evidence.Signal.StateVersion, ExpectedFeedbackVersion: version, RequestKey: fmt.Sprintf("correction-%s-%d", evidence.Signal.RootID, version), Kind: "correction", Summary: summary, ReadIDs: []int64{evidence.Reads[0].ID}})
	if err != nil || result.Replayed || result.CurrentRevision != version+1 {
		t.Fatalf("operator correction: %+v %v", result, err)
	}
	return result
}

func waitDiscoveryReview(t *testing.T, ctx context.Context, c *Client) harness.SkillDiscovery {
	t.Helper()
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		page, err := c.ListSkillDiscoveries(ctx, harness.SkillQuery{Limit: 100})
		if err != nil || len(page.Discoveries) > 1 {
			t.Fatalf("automatic selection duplicated task roots: %+v %v", page, err)
		}
		if len(page.Discoveries) == 1 {
			discovery := page.Discoveries[0]
			job, err := c.GetSkillGeneration(ctx, discovery.GenerationID)
			if err != nil {
				t.Fatal(err)
			}
			if job.State == "failed" || job.State == "cancelled" {
				t.Fatalf("discovery generation failed: %+v", job)
			}
			if job.State == "completed" && discovery.State == "completed" && discovery.ProposalID != "" {
				return discovery
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-timer.C:
			status, statusErr := c.SkillDiscoveryStatus(ctx)
			t.Fatalf("automatic correction selector did not finish: page=%+v status=%+v error=%v", page, status, statusErr)
		case <-ticker.C:
		}
	}
}

// Observe the durable worker clock as a read-only barrier. This avoids treating
// a sleep (or an empty list before the worker ran) as proof of non-generation.
// No test calls the selector directly or manufactures a signal/table row.
func waitDiscoveryConsideredAfter(t *testing.T, ctx context.Context, c *Client, after time.Time) {
	t.Helper()
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		var observed int64
		if err := c.store.DB().QueryRowContext(ctx, "SELECT last_ns FROM df_skill_discovery_scan WHERE id=1").Scan(&observed); err != nil {
			t.Fatal(err)
		}
		if observed >= after.UnixNano() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-timer.C:
			t.Fatal("automatic selector did not consider the persisted skill signal")
		case <-ticker.C:
		}
	}
}

func TestSkillDiscoveryRepeatedRevisionsDoNotReplaceDistinctRoots(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	workspace, cfg, provider := skillDiscoveryFixture(t)
	cfg.SkillDiscovery = discoveryOptions()
	c, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	session, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.SetConfigOption(ctx, session.ID, "approval", harness.ApprovalReadOnly); err != nil {
		t.Fatal(err)
	}
	first, err := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "Read research for the first independent task."}}, nil, nil)
	if err != nil || first.Execution == nil {
		t.Fatalf("first task: %+v %v", first, err)
	}
	evidence := waitDiscoverySignal(t, ctx, c, first.Execution.RunID)
	discoveryFeedback(t, ctx, c, evidence, 0, "Initial correction for one root.")
	discoveryFeedback(t, ctx, c, evidence, 1, "Refined correction for the same root.")
	latest := discoveryFeedback(t, ctx, c, evidence, 2, "Final correction still represents only one root.")
	waitDiscoveryConsideredAfter(t, ctx, c, time.Now())
	page, err := c.ListSkillDiscoveries(ctx, harness.SkillQuery{Limit: 10})
	status, statusErr := c.SkillDiscoveryStatus(ctx)
	if err != nil || statusErr != nil || len(page.Discoveries) != 0 || status.DailyReservations != 0 || provider.maintenanceCalls.Load() != 0 {
		t.Fatalf("one root's revisions crossed distinct-task threshold: %+v %+v calls=%d errors=%v/%v", page, status, provider.maintenanceCalls.Load(), err, statusErr)
	}
	second, err := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "Read research for a second independent task."}}, nil, nil)
	if err != nil || second.Execution == nil {
		t.Fatalf("second task: %+v %v", second, err)
	}
	secondEvidence := waitDiscoverySignal(t, ctx, c, second.Execution.RunID)
	discoveryFeedback(t, ctx, c, secondEvidence, 0, "Independent second root requires the same improvement.")
	discovery := waitDiscoveryReview(t, ctx, c)
	if len(discovery.Evidence) != 2 || provider.maintenanceCalls.Load() != 3 {
		t.Fatalf("distinct roots failed to trigger one candidate: %+v calls=%d", discovery, provider.maintenanceCalls.Load())
	}
	roots := map[string]int64{}
	for _, ref := range discovery.Evidence {
		roots[ref.RootID] = ref.FeedbackRevision
	}
	if len(roots) != 2 || roots[first.Execution.RunID] != latest.Feedback.Revision || roots[second.Execution.RunID] != 1 {
		t.Fatalf("automatic selection used duplicate or stale revisions: %+v", discovery.Evidence)
	}
}

func TestSkillDiscoverySDKAndAuthenticatedManagement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	workspace, cfg, provider := skillDiscoveryFixture(t)
	c, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if c != nil {
			_ = c.Close()
		}
	}()
	session, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.SetConfigOption(ctx, session.ID, "approval", harness.ApprovalReadOnly); err != nil {
		t.Fatal(err)
	}
	var roots []harness.SkillSignalEvidence
	var executions []harness.ExecutionState
	for n := range 2 {
		result, err := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: fmt.Sprintf("Task %d: read the research skill.", n)}}, nil, nil)
		if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted {
			t.Fatalf("foreground Eino task: %+v %v", result, err)
		}
		roots = append(roots, waitDiscoverySignal(t, ctx, c, result.Execution.RunID))
	}
	// Execution.EventCursor is the session's durable event cursor; record it
	// after both legitimate foreground runs, before enabling discovery.
	for _, root := range roots {
		execution, err := c.Execution(ctx, session.ID, root.Signal.RootID)
		if err != nil {
			t.Fatal(err)
		}
		executions = append(executions, execution)
	}
	discoveryFeedback(t, ctx, c, roots[0], 0, "Obsolete correction; replaced below.")
	first := discoveryFeedback(t, ctx, c, roots[0], 1, "Check the source date before summarizing.")
	second := discoveryFeedback(t, ctx, c, roots[1], 0, "Verify source claims before summarizing.")
	if roots[0].Signal.RootID == roots[1].Signal.RootID || provider.userCalls.Load() != 4 {
		t.Fatal("fixture did not produce two independent real roots")
	}
	before, err := c.HistoryPage(ctx, session.ID, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	head, err := c.GetSkillHead(ctx, "project", "research")
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := c.SkillDiscoveryStatus(ctx)
	if err != nil || disabled.Enabled || disabled.Running || disabled.Options != nil || provider.maintenanceCalls.Load() != 0 {
		t.Fatalf("disabled discovery performed or advertised automatic work: %+v calls=%d %v", disabled, provider.maintenanceCalls.Load(), err)
	}
	empty, err := c.ListSkillDiscoveries(ctx, harness.SkillQuery{Limit: 10})
	if err != nil || len(empty.Discoveries) != 0 {
		t.Fatalf("disabled discovery generated work: %+v %v", empty, err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c = nil
	cfg.SkillDiscovery = discoveryOptions()
	c, err = Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.LoadSession(ctx, session.ID, workspace, false, nil); err != nil {
		t.Fatal(err)
	}
	discovery := waitDiscoveryReview(t, ctx, c)
	if discovery.ID == "" || discovery.Target != roots[0].Signal.Target || discovery.Base != head || discovery.Attribution != "unassessed" || discovery.Kind != "operator_corrections" || discovery.CreatedAt.IsZero() || len(discovery.Evidence) != 2 {
		t.Fatalf("discovery changed evidence identity/attribution: %+v", discovery)
	}
	expected := map[string]harness.SkillFeedback{first.Feedback.RootID: first.Feedback, second.Feedback.RootID: second.Feedback}
	for _, observed := range discovery.Evidence {
		feedback, ok := expected[observed.RootID]
		if !ok || observed.SignalID != feedback.SignalID || observed.SignalStateVersion != feedback.SignalStateVersion || observed.FeedbackRevision != feedback.Revision || !reflect.DeepEqual(observed.ReadIDs, feedback.ReadIDs) {
			t.Fatalf("duplicate root, stale revision or fabricated evidence: %+v expected=%+v", observed, expected)
		}
		delete(expected, observed.RootID)
	}
	if len(expected) != 0 {
		t.Fatal("revisions from one task replaced evidence from another root")
	}
	select {
	case evidence := <-provider.generated:
		if len(evidence) != 2 {
			t.Fatalf("generation did not receive exactly two current corrections: %+v", evidence)
		}
		text, _ := json.Marshal(evidence)
		if strings.Contains(string(text), "Obsolete correction") || !strings.Contains(string(text), first.Feedback.Summary) || !strings.Contains(string(text), second.Feedback.Summary) {
			t.Fatalf("generation used stale or missing feedback: %s", text)
		}
	default:
		t.Fatal("no independently budgeted generator call")
	}
	proposal, err := c.GetSkillProposal(ctx, discovery.ProposalID)
	if err != nil || proposal.Status != "pending_review" || proposal.PublishedRevision != 0 || proposal.Evaluation.Behavior != "not_run" {
		t.Fatalf("automatic generation skipped human review: %+v %v", proposal, err)
	}
	job, err := c.GetSkillGeneration(ctx, discovery.GenerationID)
	if err != nil || job.State != "completed" || job.DiscoveryID != discovery.ID || job.ProposalID != proposal.ID {
		t.Fatalf("discovery/generation association: %+v %v", job, err)
	}
	receipts, err := c.SkillGenerationReceipts(ctx, discovery.GenerationID, 0, 100)
	if err != nil || len(receipts) != 3 || provider.maintenanceCalls.Load() != 3 {
		t.Fatalf("independent maintenance charges: %+v calls=%d %v", receipts, provider.maintenanceCalls.Load(), err)
	}
	for _, receipt := range receipts {
		if receipt.Estimated || receipt.ChargedTokens != 125 {
			t.Fatalf("maintenance charge was not accounted: %+v", receipt)
		}
	}
	current, err := c.GetSkillHead(ctx, "project", "research")
	if err != nil || current != head {
		t.Fatalf("discovery published a skill: %+v %v", current, err)
	}
	after, err := c.HistoryPage(ctx, session.ID, "", 100)
	if err != nil || !reflect.DeepEqual(before, after) || provider.userCalls.Load() != 4 {
		t.Fatalf("maintenance modified/replayed user conversation: %v calls=%d", err, provider.userCalls.Load())
	}
	for _, original := range executions {
		current, err := c.Execution(ctx, session.ID, original.RunID)
		if err != nil || !reflect.DeepEqual(current, original) {
			t.Fatalf("maintenance changed user execution: %+v %+v %v", original, current, err)
		}
	}

	// Exercise the real token-authenticated control framing, including queries
	// which cannot inject identities or start a paid discovery cycle manually.
	wire := newMissingManagement(t, ctx, c, nil)
	conn, _, line := wire.connect("wrong-discovery-token")
	_ = conn.Close()
	if line == "OK" {
		t.Fatal("unauthenticated discovery management accepted")
	}
	var status harness.SkillDiscoveryStatus
	wire.wire(map[string]any{"operation": "skill.discovery.status"}, "", &status)
	if !status.Enabled || !status.Running || status.Options == nil || *status.Options != *cfg.SkillDiscovery || len(status.Sources) != 1 || status.Sources[0].SourceIdentity != discovery.Target.SourceIdentity || status.DailyReservations != 1 || status.Pending != 1 || status.FailedSignals != 0 || status.LastErrorCode != "" {
		t.Fatalf("discovery status: %+v", status)
	}
	var managed harness.SkillDiscoveryPage
	wire.wire(map[string]any{"operation": "skill.discovery.list", "source_id": "project", "name": "research", "limit": 1}, "", &managed)
	if len(managed.Discoveries) != 1 || managed.Discoveries[0].ID != discovery.ID || managed.Next != "" {
		t.Fatalf("authenticated discovery history: %+v", managed)
	}
	for _, fields := range []map[string]any{
		{"operation": "skill.discovery.status", "run": true},
		{"operation": "skill.discovery.list", "target": discovery.Target},
		{"operation": "skill.discovery.list", "actor": "forged"},
		{"operation": "skill.discovery.list", "status": "pending_review"},
		{"operation": "skill.discovery.list", "limit": 0},
		{"operation": "skill.discovery.list", "limit": 101},
		{"operation": "skill.discovery.list", "limit": nil},
		{"operation": "skill.discovery.list", "limit": 1.5},
		{"operation": "skill.discovery.list", "source_id": nil},
		{"operation": "skill.discovery.list", "name": nil},
		{"operation": "skill.discovery.list", "cursor": nil},
		{"operation": "skill.discovery.list", "cursor": "not-a-real-cursor"},
	} {
		wire.wire(fields, "invalid_request", nil)
	}
	wire.wire(map[string]any{"operation": "skill.discovery.run"}, "unsupported_operation", nil)
	var capability struct {
		Capabilities map[string]bool `json:"capabilities"`
	}
	wire.wire(map[string]any{"operation": "proposal.history"}, "", &capability)
	if !capability.Capabilities["discovery"] || !capability.Capabilities["generation"] || capability.Capabilities["probation"] {
		t.Fatalf("discovery capability is missing or overclaimed: %+v", capability.Capabilities)
	}
	wire.wire(map[string]any{"operation": "skill.discovery.list", "source_id": "unconfigured"}, "permission_denied", nil)
	cancelled, stop := context.WithCancel(ctx)
	stop()
	if _, err = c.SkillDiscoveryStatus(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled discovery status: %v", err)
	}
	if _, err = c.ListSkillDiscoveries(cancelled, harness.SkillQuery{Limit: 10}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled discovery list: %v", err)
	}
	for _, limit := range []int{0, -1, 101} {
		if _, err = c.ListSkillDiscoveries(ctx, harness.SkillQuery{Limit: limit}); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatalf("SDK invalid discovery limit %d: %v", limit, err)
		}
	}
	if err = wire.host.Close(); err != nil {
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c = nil

	// The selector remains opted in across restart. Durable selection/cooldown
	// must not charge or generate again for the same consumed corrections.
	c, err = Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	waitDiscoveryConsideredAfter(t, ctx, c, time.Now())
	restarted, err := c.ListSkillDiscoveries(ctx, harness.SkillQuery{Limit: 10})
	if err != nil || len(restarted.Discoveries) != 1 || !reflect.DeepEqual(restarted.Discoveries[0], discovery) {
		t.Fatalf("restart changed discovery receipt: original=%+v restarted=%+v error=%v", discovery, restarted, err)
	}
	if _, err = c.LoadSession(ctx, session.ID, workspace, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "Continue after automatic maintenance."}}, nil, nil); err != nil {
		t.Fatalf("discovery poisoned next prompt: %v", err)
	}
	if provider.maintenanceCalls.Load() != 3 || provider.userCalls.Load() != 6 {
		t.Fatalf("restart replayed maintenance: user=%d maintenance=%d", provider.userCalls.Load(), provider.maintenanceCalls.Load())
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c = nil

	// Reusing the store with the same source ID but a different configured root
	// does not authorize the previous source identity or expose its history.
	_, other := skillFixture(t)
	foreign := cfg
	foreign.SkillDiscovery, foreign.SkillGeneration, foreign.SkillValidator = nil, nil, nil
	foreign.Skills = other.Skills
	foreign.Skills.Install = []harness.SkillInstall{{SourceID: "project", Directory: "research", Enabled: true}}
	c, err = Open(ctx, foreign)
	if err != nil {
		t.Fatal(err)
	}
	foreignPage, err := c.ListSkillDiscoveries(ctx, harness.SkillQuery{SourceID: "project", Limit: 10})
	if err != nil || len(foreignPage.Discoveries) != 0 {
		t.Fatalf("source identity leak: %+v %v", foreignPage, err)
	}
	foreignWire := newMissingManagement(t, ctx, c, nil)
	foreignWire.wire(map[string]any{"operation": "skill.discovery.list", "source_id": "project", "limit": 10}, "", &foreignPage)
	if len(foreignPage.Discoveries) != 0 {
		t.Fatal("MANAGE leaked prior source identity")
	}
	foreignWire.wire(map[string]any{"operation": "skill.discovery.status"}, "", &disabled)
	if disabled.Enabled || disabled.Running || disabled.Options != nil || provider.maintenanceCalls.Load() != 3 {
		t.Fatalf("disabled source context charged work: %+v", disabled)
	}
	if err = foreignWire.host.Close(); err != nil {
		t.Fatal(err)
	}
}
