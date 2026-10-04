package desktopconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/launch"
)

func generationDocument(t *testing.T) (Options, map[string]any) {
	t.Helper()
	options, doc := reviewDocument(t)
	review := readObject(doc, "skill_evolution")
	review["generation_model_name"] = "second"
	review["generation"] = map[string]any{"enabled": true, "timeout_seconds": 1, "max_input_bytes": 4096, "max_output_tokens": 1024}
	review["budget"] = map[string]any{"max_daily_model_calls": 3, "max_daily_tokens": 100000, "max_job_seconds": 30, "workers": 1}
	return options, doc
}

// Exercise the actual settings save -> launch loader -> Eino maintenance worker
// path, including named routes with identical provider model IDs.
func TestGenerationSettingsSaveLaunchAndProviderConsumption(t *testing.T) {
	for _, mode := range []string{"review", "budget", "timeout", "input-limit", "token-budget", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			options, doc := generationDocument(t)
			review := readObject(doc, "skill_evolution")
			third := cloneMap(modelByName(t, doc, "second"))
			third["name"], third["original_name"] = "generator", ""
			doc["models"] = append(list(doc, "models"), third)
			review["generation_model_name"] = "generator"
			if mode == "budget" {
				readObject(review, "budget")["max_daily_model_calls"] = 2
			} else if mode == "timeout" {
				readObject(review, "budget")["max_daily_model_calls"] = 1
			} else if mode == "input-limit" {
				readObject(review, "generation")["max_input_bytes"] = 1024
			} else if mode == "token-budget" {
				readObject(review, "budget")["max_daily_tokens"] = 1
			} else if mode == "deadline" {
				readObject(review, "generation")["timeout_seconds"] = 0
				readObject(review, "budget")["max_job_seconds"] = 1
			}
			var calls [3]atomic.Int32
			for index, name := range []string{"first", "second", "generator"} {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls[index].Add(1)
					var request struct {
						Model     string
						Messages  []struct{ Content string }
						MaxTokens int `json:"max_completion_tokens"`
						Tools     []any
					}
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
						http.Error(w, "invalid fixture request", 400)
						return
					}
					_, _ = io.Copy(io.Discard, r.Body)
					wantTokens := 512
					if index == 2 {
						wantTokens = 1024
					}
					if request.Model != "shared-id" || request.MaxTokens != wantTokens || len(request.Messages) != 2 || len(request.Tools) != 0 || r.Header.Get("Authorization") != "Bearer "+name+"-private-key" {
						t.Errorf("saved %s model route or request bounds differ", name)
						http.Error(w, "invalid route", 400)
						return
					}
					if mode == "timeout" || mode == "deadline" {
						select {
						case <-r.Context().Done():
						case <-time.After(5 * time.Second):
							t.Error("saved generation timeout did not cancel provider request")
						}
						return
					}
					var input map[string]any
					if err := json.Unmarshal([]byte(request.Messages[1].Content), &input); err != nil {
						t.Error(err)
					}
					answer := map[string]any{"base_version": input["base_version"], "candidate_version": input["candidate_version"], "passed": true, "reason": "Fixture content review"}
					if index == 2 {
						answer = map[string]any{"skill_name": input["skill_name"], "base_version": input["base_version"], "reason": "Check source evidence", "files": map[string]string{"SKILL.md": "---\nname: evidence\ndescription: Check source evidence\n---\nCite the source and verify claims.\n"}}
					}
					body, _ := json.Marshal(answer)
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": string(body)}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 25, "total_tokens": 125}})
				}))
				defer server.Close()
				model := modelByName(t, doc, name)
				model["provider"], model["model"], model["base_url"], model["api_key"] = "openai", "shared-id", server.URL+"/v1", name+"-private-key"
			}
			doc = run(t, options, "save", doc)
			if strings.Contains(string(mustJSON(t, doc)), "generator-private-key") {
				t.Fatal("snapshot leaked generation credential")
			}
			var cfg deerflow.Config
			connections := 0
			if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil {
				t.Fatal(err)
			}
			wantTimeout, wantBytes, wantDeadline := time.Second, 4096, int64(30)
			if mode == "deadline" {
				wantTimeout, wantDeadline = 0, 1
			}
			if mode == "input-limit" {
				wantBytes = 1024
			}
			if cfg.SkillGeneration == nil || cfg.SkillGeneration.Model.Timeout != wantTimeout || cfg.SkillGeneration.Model.MaxInputBytes != wantBytes || cfg.SkillGeneration.Model.MaxOutputTokens != 1024 || cfg.SkillGeneration.Budget.MaxJobSeconds != wantDeadline || cfg.SkillGeneration.Budget.Workers != 1 || !reflect.DeepEqual(cfg.SkillGeneration.Sources, []string{"python-custom"}) || cfg.SkillGeneration.AllowBuiltinOverlays {
				t.Fatal("generation settings were not consumed")
			}
			cfg.DataDir = filepath.Join(t.TempDir(), "runtime")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			client, err := deerflow.Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			head, err := client.GetSkillHead(ctx, "python-custom", "evidence")
			if err != nil {
				t.Fatal(err)
			}
			reason := "Produce an evidence checklist"
			if mode == "input-limit" {
				reason = strings.Repeat("Evidence. ", 200)
			}
			job, err := client.QueueSkillGeneration(ctx, harness.SkillGenerationRequest{SourceID: "python-custom", Name: "evidence", RequestKey: "saved-request", Reason: reason, Base: head})
			if err != nil {
				t.Fatal(err)
			}
			for job.State != "completed" && job.State != "failed" {
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(20 * time.Millisecond):
				}
				job, err = client.GetSkillGeneration(ctx, job.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			wantCalls := [3]int32{1, 1, 1}
			if mode == "budget" {
				wantCalls = [3]int32{1, 0, 1}
			}
			if mode == "timeout" || mode == "deadline" {
				wantCalls = [3]int32{0, 0, 1}
			}
			if mode == "input-limit" || mode == "token-budget" {
				wantCalls = [3]int32{}
			}
			for index, want := range wantCalls {
				if calls[index].Load() != want {
					t.Fatalf("route %d requests=%d want=%d", index, calls[index].Load(), want)
				}
			}
			if mode == "review" {
				proposal, err := client.GetSkillProposal(ctx, job.ProposalID)
				if err != nil || job.State != "completed" || proposal.Status != "pending_review" || proposal.Evaluation.Behavior != "not_run" {
					t.Fatal("generation bypassed review", err, job)
				}
			} else {
				wantCode := "budget_exhausted"
				if mode == "input-limit" {
					wantCode = "model_failed"
				}
				if mode == "deadline" {
					wantCode = "job_deadline"
				}
				if job.State != "failed" || job.ErrorCode != wantCode {
					t.Fatalf("saved bound not enforced: %+v", job)
				}
			}
			current, err := client.GetSkillHead(ctx, "python-custom", "evidence")
			if err != nil || current != head {
				t.Fatal("generation published without approval", err)
			}
			receipts, err := client.SkillGenerationReceipts(ctx, job.ID, 0, 100)
			if err != nil || len(receipts) != int(wantCalls[0]+wantCalls[1]+wantCalls[2]) {
				t.Fatal("missing provider receipts", err)
			}
			if (mode == "timeout" || mode == "deadline") && (!receipts[0].Estimated || receipts[0].ChargedTokens < 1024) {
				t.Fatal("interrupted request lost conservative charge")
			}
			if err := client.Close(); err != nil {
				t.Fatal(err)
			}
			readObject(readObject(doc, "skill_evolution"), "generation")["enabled"] = false
			run(t, options, "save", doc)
			if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil || cfg.SkillGeneration != nil {
				t.Fatal("disable retained generation worker", err)
			}
			reopened, err := deerflow.Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if loaded, err := reopened.GetSkillGeneration(ctx, job.ID); err != nil || loaded.State != job.State {
				t.Fatal("disabled config lost generation history", err)
			}
			if _, err := reopened.QueueSkillGeneration(ctx, harness.SkillGenerationRequest{SourceID: "python-custom", Name: "evidence", RequestKey: "disabled", Reason: "Must not execute", Base: head}); err == nil {
				t.Fatal("disabled config admitted model work")
			}
		})
	}
}

func TestGenerationInvalidEditsFailBeforeSaveAndAtLaunch(t *testing.T) {
	for _, edit := range []struct {
		group, key string
		value      any
	}{
		{"", "generation_model_name", "unknown"}, {"generation", "enabled", "yes"},
		{"generation", "max_input_bytes", 1023}, {"generation", "timeout_seconds", .5},
		{"generation", "max_output_tokens", 32769}, {"generation", "unknown", true},
		{"budget", "max_daily_model_calls", 0}, {"budget", "max_daily_tokens", 0},
		{"budget", "max_daily_tokens", 1000000001}, {"budget", "max_job_seconds", 3601},
		{"budget", "workers", 1.5}, {"budget", "workers", 17}, {"budget", "unknown", true},
	} {
		t.Run(fmt.Sprintf("%s-%s-%v", edit.group, edit.key, edit.value), func(t *testing.T) {
			options, doc := generationDocument(t)
			doc = run(t, options, "save", doc)
			raw, err := readYAML(options.Config)
			if err != nil {
				t.Fatal(err)
			}
			for _, document := range []map[string]any{doc, raw} {
				target := readObject(document, "skill_evolution")
				if edit.group != "" {
					target = readObject(target, edit.group)
				}
				target[edit.key] = edit.value
			}
			before, _ := os.ReadFile(options.Config)
			for _, enabled := range []bool{true, false} {
				readObject(doc, "skill_evolution")["enabled"] = enabled
				for _, op := range []string{"validate", "save"} {
					if _, err := Run(options, op, mustJSON(t, doc)); err == nil || !strings.Contains(err.Error(), "skill_evolution") {
						t.Fatal("invalid generation edit accepted", op, err)
					}
				}
			}
			after, _ := os.ReadFile(options.Config)
			if !bytes.Equal(before, after) {
				t.Fatal("invalid edit wrote config")
			}
			if err := writeYAMLAtomic(options.Config, raw); err != nil {
				t.Fatal(err)
			}
			var cfg deerflow.Config
			connections := 0
			if _, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil); err == nil || !strings.Contains(err.Error(), "skill_evolution") {
				t.Fatal("launch accepted invalid generation config", err)
			}
		})
	}
}
