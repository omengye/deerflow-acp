package deerflow

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
)

func TestSkillGenerationSDKWorkerAndAuthenticatedManagement(t *testing.T) {
	for _, mode := range []string{"review", "quality-rejected", "budget"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			var calls, runs atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var req struct {
					Model    string
					Messages []struct{ Role, Content string }
					Tools    []any
				}
				if e := json.NewDecoder(r.Body).Decode(&req); e != nil {
					t.Error(e)
					http.Error(w, "bad", 400)
					return
				}
				if len(req.Tools) != 0 || len(req.Messages) != 2 {
					t.Error("generation/review received tools or history")
					http.Error(w, "bad", 400)
					return
				}
				var input map[string]json.RawMessage
				if e := json.Unmarshal([]byte(req.Messages[1].Content), &input); e != nil {
					t.Error(e)
				}
				var base, candidate, name string
				_ = json.Unmarshal(input["base_version"], &base)
				_ = json.Unmarshal(input["candidate_version"], &candidate)
				_ = json.Unmarshal(input["skill_name"], &name)
				var answer []byte
				if req.Model == "generator" {
					answer, _ = json.Marshal(map[string]any{"skill_name": name, "base_version": base, "reason": "Verify sources before summarizing", "files": map[string]string{"SKILL.md": "---\nname: research\ndescription: Research a source\n---\nVerify sources. Read [guide](guide.txt).\n", "guide.txt": "PINNED_REFERENCE"}})
				} else {
					answer, _ = json.Marshal(map[string]any{"base_version": base, "candidate_version": candidate, "passed": mode != "quality-rejected" || req.Model != "evaluator", "reason": "Fixture content assessment"})
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": string(answer)}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 25, "total_tokens": 125}})
			}))
			defer server.Close()
			workspace, cfg := skillFixture(t)
			cfg.Engine = engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
				runs.Add(1)
				return harness.RunResult{StopReason: "end_turn"}, nil
			})
			cfg.Skills.Install = []harness.SkillInstall{{SourceID: "project", Directory: "research", Enabled: true}}
			route := func(model string) harness.ModelRoute {
				return harness.ModelRoute{Model: model, BaseURL: server.URL, APIKey: "fixture-private-key"}
			}
			var e error
			cfg.SkillValidator, e = NewSkillModelValidator(ctx, harness.SkillModelReviewConfig{Moderation: route("moderator"), Evaluation: route("evaluator"), MaxOutputTokens: 512})
			if e != nil {
				t.Fatal(e)
			}
			budget := int64(10)
			if mode == "budget" {
				budget = 2
			}
			cfg.SkillGeneration = &harness.SkillGenerationOptions{Model: harness.SkillGenerationModelConfig{Route: route("generator")}, Budget: harness.SkillMaintenanceBudget{MaxDailyModelCalls: budget, MaxDailyTokens: 100000, MaxJobSeconds: 60, Workers: 1}, Sources: []string{"project"}}
			c, e := Open(ctx, cfg)
			if e != nil {
				t.Fatal(e)
			}
			defer c.Close()
			host, e := localhost.New(localhost.Config{RuntimeDir: t.TempDir(), Manage: c.ManageLocal, ServeACP: func(context.Context, io.ReadCloser, io.WriteCloser) error { return nil }})
			if e != nil {
				t.Fatal(e)
			}
			ep, e := host.Start(ctx)
			if e != nil {
				t.Fatal(e)
			}
			defer host.Close()
			wire := func(fields map[string]any) map[string]json.RawMessage {
				t.Helper()
				conn, e := net.DialTimeout("tcp", net.JoinHostPort(ep.Host, fmt.Sprint(ep.Port)), time.Second)
				if e != nil {
					t.Fatal(e)
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				if _, e = fmt.Fprintf(conn, "DFACP/1 %s MANAGE\n", ep.Token); e != nil {
					t.Fatal(e)
				}
				reader := bufio.NewReader(conn)
				if line, e := reader.ReadString('\n'); e != nil || strings.TrimSpace(line) != "OK" {
					t.Fatalf("handshake %q %v", line, e)
				}
				if e = json.NewEncoder(conn).Encode(fields); e != nil {
					t.Fatal(e)
				}
				var result map[string]json.RawMessage
				if e = json.NewDecoder(reader).Decode(&result); e != nil {
					t.Fatal(e)
				}
				return result
			}
			history := wire(map[string]any{"operation": "proposal.history"})
			var advertised struct {
				Capabilities map[string]bool `json:"capabilities"`
			}
			if string(history["ok"]) != "true" || json.Unmarshal(history["data"], &advertised) != nil || !advertised.Capabilities["generation"] || advertised.Capabilities["discovery"] || advertised.Capabilities["probation"] {
				t.Fatalf("configured generation capabilities: %s", history)
			}
			target, head, _, e := c.evolution.Head(ctx, "project", "research")
			if e != nil {
				t.Fatal(e)
			}
			statusResponse := wire(map[string]any{"operation": "skill.generation.status"})
			var generationStatus harness.SkillGenerationStatus
			if string(statusResponse["ok"]) != "true" || json.Unmarshal(statusResponse["data"], &generationStatus) != nil || !generationStatus.Enabled || len(generationStatus.Sources) != 1 || generationStatus.Sources[0].SourceIdentity != target.SourceIdentity || generationStatus.Budget == nil || generationStatus.Budget.MaxDailyModelCalls != budget {
				t.Fatalf("status: %s", statusResponse)
			}
			prepareResponse := wire(map[string]any{"operation": "skill.generation.prepare", "target": target})
			var preparation harness.SkillGenerationPreparation
			if string(prepareResponse["ok"]) != "true" || json.Unmarshal(prepareResponse["data"], &preparation) != nil || preparation.Target != target || preparation.Base != head || calls.Load() != 0 {
				t.Fatalf("prepare: %s", prepareResponse)
			}
			for _, invalid := range []map[string]any{
				{"operation": "skill.generation.status", "unknown": true},
				{"operation": "skill.generation.prepare"},
				{"operation": "skill.generation.prepare", "target": nil},
				{"operation": "skill.generation.prepare", "target": target, "reason": "Not accepted by a read-only check"},
			} {
				if result := wire(invalid); string(result["ok"]) != "false" {
					t.Fatalf("invalid preparation accepted: %s", result)
				}
			}
			response := wire(map[string]any{"operation": "skill.generation.enqueue", "target": target, "expected_head": head, "request_key": "operator-request", "reason": "Please verify source claims"})
			var queued harness.SkillGenerationJob
			if string(response["ok"]) != "true" || json.Unmarshal(response["data"], &queued) != nil || queued.ID == "" {
				t.Fatalf("enqueue %s", response)
			}
			var finished harness.SkillGenerationJob
			deadline := time.Now().Add(20 * time.Second)
			for {
				finished, e = c.GetSkillGeneration(ctx, queued.ID)
				if e != nil {
					t.Fatal(e)
				}
				if finished.State == "completed" || finished.State == "failed" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("worker did not finish: %+v", finished)
				}
				time.Sleep(20 * time.Millisecond)
			}
			p, e := c.GetSkillProposal(ctx, finished.ProposalID)
			if e != nil {
				t.Fatal(e)
			}
			if mode == "review" {
				if finished.State != "completed" || p.Status != "pending_review" || p.Evaluation.Behavior != "not_run" {
					t.Fatalf("review %+v %+v", finished, p)
				}
			} else if finished.State != "failed" || p.Status != "failed" {
				t.Fatalf("failed checks became reviewable %+v %+v", finished, p)
			}
			if mode == "budget" && finished.ErrorCode != "budget_exhausted" {
				t.Fatalf("budget reason %+v", finished)
			}
			want := int32(3)
			if mode == "budget" {
				want = 2
			}
			if calls.Load() != want || runs.Load() != 0 {
				t.Fatalf("calls=%d user-runs=%d", calls.Load(), runs.Load())
			}
			current, e := c.GetSkillHead(ctx, "project", "research")
			if e != nil || current != head {
				t.Fatalf("worker published %+v %v", current, e)
			}
			response = wire(map[string]any{"operation": "skill.generation.receipts", "job_id": queued.ID, "limit": 1})
			if string(response["ok"]) != "true" || strings.Contains(string(response["data"]), "fixture-private-key") || strings.Contains(string(response["data"]), server.URL) {
				t.Fatalf("receipts %s", response)
			}
			receipts, e := c.SkillGenerationReceipts(ctx, queued.ID, 0, 100)
			if e != nil || len(receipts) != int(want) {
				t.Fatalf("receipts %+v %v", receipts, e)
			}
			for _, receipt := range receipts {
				if receipt.Estimated || receipt.ChargedTokens != 125 {
					t.Fatal(receipt)
				}
			}
			// A failed maintenance job cannot put the conversation into failure or
			// reconciliation, or debit/replay its user task.
			session, e := c.NewSession(ctx, workspace)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "Continue the conversation"}}, nil, nil); e != nil || runs.Load() != 1 {
				t.Fatalf("maintenance blocked chat %v runs=%d", e, runs.Load())
			}
			if mode == "review" {
				if _, e = c.ApproveSkillProposal(ctx, p.ID, harness.SkillReview{ExpectedStateVersion: p.StateVersion, Actor: "operator"}); e != nil {
					t.Fatal(e)
				}
			}
			if e = host.Close(); e != nil {
				t.Fatal(e)
			}
			if e = c.Close(); e != nil {
				t.Fatal(e)
			}
			cfg.SkillGeneration = nil
			cfg.SkillValidator = nil
			reopened, e := Open(ctx, cfg)
			if e != nil {
				t.Fatal(e)
			}
			defer reopened.Close()
			disabled, e := reopened.SkillGenerationStatus(ctx)
			if e != nil || disabled.Enabled || len(disabled.Sources) != 0 || disabled.Budget != nil {
				t.Fatal("disabled status advertises grants", e)
			}
			if _, e := reopened.PrepareSkillGeneration(ctx, target); e == nil {
				t.Fatal("disabled generation prepared a target")
			}
			disabledHistory, e := skillManagementCall(t, reopened, map[string]any{"operation": "proposal.history"})
			if e != nil {
				t.Fatal(e)
			}
			disabledJSON, e := json.Marshal(disabledHistory)
			if e != nil || json.Unmarshal(disabledJSON, &advertised) != nil || advertised.Capabilities["generation"] || advertised.Capabilities["discovery"] || advertised.Capabilities["probation"] {
				t.Fatalf("disabled generation capabilities: %s %v", disabledJSON, e)
			}
			loaded, e := reopened.GetSkillGeneration(ctx, queued.ID)
			if e != nil || loaded.State != finished.State || loaded.ProposalID != p.ID || calls.Load() != want {
				t.Fatalf("restart replayed %+v %v calls=%d", loaded, e, calls.Load())
			}
			if _, e = reopened.QueueSkillGeneration(ctx, harness.SkillGenerationRequest{SourceID: "project", Name: "research", RequestKey: "disabled", Reason: "Should not run", Base: head}); e == nil {
				t.Fatal("disabled generation accepted paid work")
			}
		})
	}
}

func TestSkillGenerationSDKCloseJoinsModelAndPreservesCharge(t *testing.T) {
	ctx := context.Background()
	started, joined := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		close(started)
		<-r.Context().Done()
		close(joined)
	}))
	defer server.Close()
	_, cfg := skillFixture(t)
	cfg.Engine = engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		return harness.RunResult{StopReason: "end_turn"}, nil
	})
	cfg.Skills.Install = []harness.SkillInstall{{SourceID: "project", Directory: "research", Enabled: true}}
	route := harness.ModelRoute{Model: "fixture", APIKey: "fixture", BaseURL: server.URL}
	var e error
	cfg.SkillValidator, e = NewSkillModelValidator(ctx, harness.SkillModelReviewConfig{Moderation: route, Evaluation: route})
	if e != nil {
		t.Fatal(e)
	}
	cfg.SkillGeneration = &harness.SkillGenerationOptions{Model: harness.SkillGenerationModelConfig{Route: route}, Budget: harness.SkillMaintenanceBudget{MaxDailyModelCalls: 10, MaxDailyTokens: 100000, MaxJobSeconds: 60, Workers: 1}, Sources: []string{"project"}}
	c, e := Open(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	head, e := c.GetSkillHead(ctx, "project", "research")
	if e != nil {
		t.Fatal(e)
	}
	job, e := c.QueueSkillGeneration(ctx, harness.SkillGenerationRequest{SourceID: "project", Name: "research", RequestKey: "shutdown", Reason: "Verify source claims", Base: head})
	if e != nil {
		t.Fatal(e)
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("generation did not start")
	}
	if e = c.Close(); e != nil {
		t.Fatal(e)
	}
	select {
	case <-joined:
	case <-time.After(2 * time.Second):
		t.Fatal("provider request not cancelled")
	}
	cfg.SkillGeneration = nil
	cfg.SkillValidator = nil
	reopened, e := Open(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	saved, e := reopened.GetSkillGeneration(ctx, job.ID)
	if e != nil || saved.State != "retry" || saved.Attempts != 1 || saved.CandidateVersion != "" {
		t.Fatalf("shutdown job %+v %v", saved, e)
	}
	r, e := reopened.SkillGenerationReceipts(ctx, job.ID, 0, 10)
	if e != nil || len(r) != 1 || !r[0].Estimated || r[0].ChargedTokens != r[0].ReservedTokens || r[0].ChargedTokens == 0 {
		t.Fatalf("shutdown refunded uncertain call %+v %v", r, e)
	}
}
