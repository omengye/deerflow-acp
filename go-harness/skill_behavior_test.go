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
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstreamtest"
)

func TestSkillBehaviorSDKManualAndGeneratedCandidates(t *testing.T) {
	for _, mode := range []string{"manual", "generated", "manual-failed"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			var modelCalls, behaviorCalls, userRuns atomic.Int32
			candidateText := "---\nname: research\ndescription: Research a source\n---\nVERIFIED_CANDIDATE. Read input.txt and copy its contents to output.txt.\n"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				modelCalls.Add(1)
				var request struct {
					Model    string
					Messages []struct {
						Role, Content string
						ToolCallID    string `json:"tool_call_id"`
					}
					Tools []any
				}
				if json.NewDecoder(r.Body).Decode(&request) != nil || len(request.Messages) < 2 {
					t.Error("invalid provider request")
					http.Error(w, "invalid", 400)
					return
				}
				for _, m := range request.Messages {
					if strings.Contains(m.Content, "oracle-secret-path") {
						t.Error("host assertion leaked to model")
					}
				}
				message := map[string]any{"role": "assistant"}
				finish := "stop"
				if len(request.Tools) > 0 {
					behaviorCalls.Add(1)
					tool := func(id, name string, args map[string]string) {
						encoded, _ := json.Marshal(args)
						message["tool_calls"] = []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": string(encoded)}}}
						finish = "tool_calls"
					}
					last := request.Messages[len(request.Messages)-1]
					switch {
					case last.Role == "user":
						tool("read-input", "read_file", map[string]string{"path": "input.txt"})
					case last.Role == "tool" && last.ToolCallID == "read-input":
						value := "OLD"
						if mode != "manual-failed" && strings.Contains(request.Messages[0].Content, "VERIFIED_CANDIDATE") {
							value = last.Content
						}
						tool("write-output", "write_file", map[string]string{"path": "output.txt", "content": value})
					case last.Role == "tool" && last.ToolCallID == "write-output":
						message["content"] = "finished"
					default:
						t.Error("unexpected isolated tool context")
						http.Error(w, "invalid", 400)
						return
					}
				} else {
					var input map[string]json.RawMessage
					if json.Unmarshal([]byte(request.Messages[1].Content), &input) != nil {
						t.Error("invalid review input")
						return
					}
					var base, candidate, name string
					_ = json.Unmarshal(input["base_version"], &base)
					_ = json.Unmarshal(input["candidate_version"], &candidate)
					_ = json.Unmarshal(input["skill_name"], &name)
					var answer []byte
					if request.Model == "generator" {
						answer, _ = json.Marshal(map[string]any{"skill_name": name, "base_version": base, "reason": "Copy verified inputs", "files": map[string]string{"SKILL.md": candidateText, "guide.txt": "PINNED_REFERENCE"}})
					} else {
						answer, _ = json.Marshal(map[string]any{"base_version": base, "candidate_version": candidate, "passed": true, "reason": "Content review passed"})
					}
					message["content"] = string(answer)
				}
				_ = modelstreamtest.WriteCompletion(w, map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": finish}}, "usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 25, "total_tokens": 125}})
			}))
			defer server.Close()
			workspace, cfg := skillFixture(t)
			cfg.Engine = engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
				userRuns.Add(1)
				return harness.RunResult{StopReason: "end_turn"}, nil
			})
			cfg.Skills.Install = []harness.SkillInstall{{SourceID: "project", Directory: "research", Enabled: true}}
			route := func(name string) harness.ModelRoute {
				return harness.ModelRoute{Model: name, BaseURL: server.URL, APIKey: "fixture-private-key"}
			}
			var err error
			cfg.SkillValidator, err = NewSkillModelValidator(ctx, harness.SkillModelReviewConfig{Moderation: route("security"), Evaluation: route("quality"), MaxOutputTokens: 512})
			if err != nil {
				t.Fatal(err)
			}
			cfg.SkillGeneration = &harness.SkillGenerationOptions{Model: harness.SkillGenerationModelConfig{Route: route("generator")}, Sources: []string{"project"}, Budget: harness.SkillMaintenanceBudget{MaxDailyModelCalls: 20, MaxDailyTokens: 200000, MaxJobSeconds: 30, Workers: 1}}
			cfg.SkillBehaviorEvaluator, err = NewSkillBehaviorEvaluator(ctx, harness.SkillBehaviorModelConfig{Route: route("behavior"), MaxModelCalls: 3, MaxOutputTokens: 512})
			if err != nil {
				t.Fatal(err)
			}
			cfg.SkillBehavior = &harness.SkillBehaviorOptions{Suites: []harness.SkillBehaviorSuite{{Version: "skill-behavior-v1", ID: "copy-input", SourceID: "project", Name: "research", Cases: []harness.SkillBehaviorCase{{ID: "copy", Kind: "targeted", Prompt: "Copy input.txt to output.txt and then finish.", Files: map[string]string{"input.txt": "PASS"}, Assertions: []harness.SkillBehaviorAssertion{{ID: "output", Kind: "file_equals", Path: "output.txt", Expected: "PASS"}, {ID: "read", Kind: "tool_calls", Tool: "read_file", Count: 1}, {ID: "write", Kind: "tool_calls", Tool: "write_file", Count: 1}, {ID: "oracle", Kind: "file_absent", Path: "oracle-secret-path"}}, Repetitions: 1}}}}}
			c, err := Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if c != nil {
					_ = c.Close()
				}
			}()
			head, err := c.GetSkillHead(ctx, "project", "research")
			if err != nil {
				t.Fatal(err)
			}
			var proposalID string
			if strings.HasPrefix(mode, "manual") {
				p, e := c.SubmitSkillProposal(ctx, harness.SkillProposalRequest{SourceID: "project", Name: "research", RequestKey: "manual-fixture", Action: "edit", Base: head, Reason: "Copy verified inputs", Files: map[string][]byte{"SKILL.md": []byte(candidateText), "guide.txt": []byte("PINNED_REFERENCE")}})
				if e != nil || p.Status != "validating" || p.BehaviorJobID == "" {
					t.Fatalf("manual proposal not queued: %+v %v", p, e)
				}
				proposalID = p.ID
			} else {
				job, e := c.QueueSkillGeneration(ctx, harness.SkillGenerationRequest{SourceID: "project", Name: "research", RequestKey: "generated-fixture", Reason: "Copy verified inputs", Base: head})
				if e != nil {
					t.Fatal(e)
				}
				for proposalID == "" {
					current, e := c.GetSkillGeneration(ctx, job.ID)
					if e != nil {
						t.Fatal(e)
					}
					if current.State == "failed" {
						t.Fatalf("generation failed: %+v", current)
					}
					proposalID = current.ProposalID
					if ctx.Err() != nil {
						t.Fatal("generation timed out")
					}
					if proposalID == "" {
						time.Sleep(20 * time.Millisecond)
					}
				}
			}
			var proposal harness.SkillProposal
			for {
				proposal, err = c.GetSkillProposal(ctx, proposalID)
				if err != nil {
					t.Fatal(err)
				}
				if proposal.Status != "validating" {
					break
				}
				if ctx.Err() != nil {
					t.Fatal("behavior timed out")
				}
				time.Sleep(20 * time.Millisecond)
			}
			wantStatus, wantBehavior, wantImprovements := "pending_review", "passed", 1
			if mode == "manual-failed" {
				wantStatus, wantBehavior, wantImprovements = "failed", "failed", 0
			}
			if proposal.Status != wantStatus || proposal.Evaluation.Behavior != wantBehavior || proposal.Evaluation.BehaviorReport == nil || proposal.Evaluation.BehaviorDigest == "" {
				t.Fatalf("behavior not independently reviewed: %+v", proposal)
			}
			report := proposal.Evaluation.BehaviorReport
			if report.Improvements != wantImprovements || report.Regressions != 0 || len(report.Results) != 1 || report.Results[0].BasePassed || report.Results[0].CandidatePassed != (mode != "manual-failed") || report.ModelCalls != 6 || report.ToolCalls != 4 || report.UsageEstimated {
				t.Fatalf("incorrect comparison: %+v", report)
			}
			evaluation, err := c.SkillBehaviorEvaluation(ctx, proposalID)
			if err != nil || len(evaluation.Results) != 2 || evaluation.Report == nil {
				t.Fatalf("evaluation: %+v %v", evaluation, err)
			}
			receipts, err := c.SkillBehaviorReceipts(ctx, proposalID, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			behaviorCount := 0
			for _, receipt := range receipts {
				if receipt.Stage == "behavior" {
					behaviorCount++
					if receipt.Estimated || receipt.InputTokens != 100 || receipt.OutputTokens != 25 {
						t.Fatalf("unsettled behavior charge: %+v", receipt)
					}
				}
			}
			if behaviorCount != 6 || behaviorCalls.Load() != 6 || userRuns.Load() != 0 {
				t.Fatalf("missing calls or foreground leak: receipts=%d behavior=%d user=%d", behaviorCount, behaviorCalls.Load(), userRuns.Load())
			}
			still, err := c.GetSkillHead(ctx, "project", "research")
			if err != nil || still != head {
				t.Fatal("behavior published without approval")
			}
			host, err := localhost.New(localhost.Config{RuntimeDir: t.TempDir(), Manage: c.ManageLocal, ServeACP: func(context.Context, io.ReadCloser, io.WriteCloser) error { return nil }})
			if err != nil {
				t.Fatal(err)
			}
			ep, err := host.Start(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer host.Close()
			wire := func(fields map[string]any) map[string]json.RawMessage {
				t.Helper()
				conn, e := net.DialTimeout("tcp", net.JoinHostPort(ep.Host, fmt.Sprint(ep.Port)), time.Second)
				if e != nil {
					t.Fatal(e)
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				_, _ = fmt.Fprintf(conn, "DFACP/1 %s MANAGE\n", ep.Token)
				reader := bufio.NewReader(conn)
				line, e := reader.ReadString('\n')
				if e != nil || strings.TrimSpace(line) != "OK" {
					t.Fatal("auth handshake", e)
				}
				if e = json.NewEncoder(conn).Encode(fields); e != nil {
					t.Fatal(e)
				}
				var reply map[string]json.RawMessage
				if e = json.NewDecoder(reader).Decode(&reply); e != nil {
					t.Fatal(e)
				}
				return reply
			}
			for _, op := range []string{"proposal.behavior", "proposal.behavior.receipts"} {
				fields := map[string]any{"operation": op, "proposal_id": proposalID}
				if op == "proposal.behavior.receipts" {
					fields["after_sequence"] = 0
				}
				reply := wire(fields)
				if string(reply["ok"]) != "true" {
					t.Fatalf("management %s: %s", op, reply)
				}
				encoded, _ := json.Marshal(reply)
				if strings.Contains(string(encoded), "oracle-secret-path") || strings.Contains(string(encoded), "input.txt") {
					t.Fatal("fixture inputs leaked in management")
				}
			}
			for _, fields := range []map[string]any{{"operation": "proposal.behavior", "proposal_id": nil}, {"operation": "proposal.behavior", "proposal_id": proposalID, "unknown": true}, {"operation": "proposal.behavior.receipts", "proposal_id": proposalID, "after_sequence": nil}, {"operation": "proposal.behavior.receipts", "proposal_id": proposalID, "limit": 2.5}, {"operation": "proposal.behavior.receipts", "proposal_id": proposalID, "limit": 101}} {
				if reply := wire(fields); string(reply["ok"]) != "false" {
					t.Fatalf("invalid request accepted: %s", reply)
				}
			}
			session, e := c.NewSession(ctx, workspace)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "Continue normal work"}}, nil, nil); e != nil {
				t.Fatal(e)
			}
			if userRuns.Load() != 1 {
				t.Fatal("next user task did not run")
			}
			beforeCalls := modelCalls.Load()
			if err = c.Close(); err != nil {
				t.Fatal(err)
			}
			c = nil
			cfg.SkillBehavior, cfg.SkillBehaviorEvaluator, cfg.SkillGeneration, cfg.SkillValidator = nil, nil, nil, nil
			reopened, e := Open(ctx, cfg)
			if e != nil {
				t.Fatal(e)
			}
			defer reopened.Close()
			prior, e := reopened.SkillBehaviorEvaluation(ctx, proposalID)
			if e != nil || prior.ID != evaluation.ID || modelCalls.Load() != beforeCalls {
				t.Fatalf("restart lost or reran results: %+v %v", prior, e)
			}
		})
	}
}
