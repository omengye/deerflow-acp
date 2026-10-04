package deerflow

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSDKSkillReadsAttributeForegroundAndBackgroundToOneRoot(t *testing.T) {
	workspace, cfg := skillFixture(t)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model    string                  `json:"model"`
			Stream   bool                    `json:"stream"`
			Messages []struct{ Role string } `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "fixture request", 400)
			return
		}
		toolResults := 0
		for _, m := range request.Messages {
			if m.Role == "tool" {
				toolResults++
			}
		}
		message := map[string]any{"role": "assistant", "content": "done"}
		finish := "stop"
		call := func(index int, id, name, args string) any {
			return map[string]any{"index": index, "id": id, "type": "function", "function": map[string]string{"name": name, "arguments": args}}
		}
		var calls []any
		if request.Model == "parent" && toolResults == 0 {
			calls = []any{
				call(0, "front", "task", `{"subagent_type":"reader","prompt":"Read research and its reference","description":"foreground"}`),
				call(1, "back", "background_agent", `{"role":"reader","instruction":"Read research and its reference","description":"background"}`),
			}
		} else if request.Model == "child" {
			switch toolResults {
			case 0:
				calls = []any{call(0, "load", "skill", `{"skill":"research"}`)}
			case 1:
				calls = []any{call(0, "read", "read_skill_file", `{"skill":"research","path":"guide.txt"}`)}
			}
		}
		if len(calls) > 0 {
			message["content"], message["tool_calls"], finish = nil, calls, "tool_calls"
		}
		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			body, _ := json.Marshal(map[string]any{"id": "fixture", "choices": []any{map[string]any{"index": 0, "delta": message, "finish_reason": finish}}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", body)
		} else {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "fixture", "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}})
		}
	}))
	defer provider.Close()
	cfg.Provider, cfg.Model, cfg.APIKey, cfg.BaseURL = "openai", "parent", "fixture", provider.URL+"/v1"
	cfg.DisableSubagents, cfg.BackgroundWorkers, cfg.PermissionMode = false, 2, harness.PermissionModeOff
	cfg.ModelRoutes = map[string]harness.ModelRoute{"child": {Provider: "openai", Model: "child", APIKey: "fixture", BaseURL: provider.URL + "/v1"}}
	cfg.Subagents = &harness.SubagentsConfig{CustomAgents: map[string]harness.AgentConfig{"reader": {Description: "Read skills", SystemPrompt: "Read the assigned skill and support file", Model: "child", Tools: []string{"skill", "read_skill_file"}, Skills: []string{"research"}}}}
	cfg.Budget = &harness.BudgetLimits{MaxModelCalls: 12, MaxToolCalls: 8, MaxTokens: 100000, MaxOutputTokens: 128}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	installed, err := c.InstallSkill(ctx, "project", "research")
	if err != nil {
		t.Fatal(err)
	}
	if err = c.SetSkillEnabled(ctx, "project", "research", true); err != nil {
		t.Fatal(err)
	}
	session, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "Delegate two reads"}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := c.BackgroundTasks(ctx, session.ID, "", 10)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks=%+v %v", tasks, err)
	}
	task := tasks[0]
	for task.Status == "pending" || task.Status == "running" {
		task, err = c.WaitBackgroundTask(ctx, session.ID, task.ID, task.Version)
		if err != nil {
			t.Fatal(err)
		}
	}
	if task.Status != "completed" {
		t.Fatalf("task=%+v", task)
	}
	var reads, roles, runs, roots, outcomes int
	err = c.store.DB().QueryRowContext(ctx, `SELECT count(*),count(DISTINCT role_id),count(DISTINCT run_id),count(DISTINCT root_id)
 FROM harness_skill_reads WHERE root_id=? AND version=? AND role_id<>''`, result.Execution.RunID, installed.Ref.Version).Scan(&reads, &roles, &runs, &roots)
	if err != nil || reads != 4 || roles != 2 || runs != 2 || roots != 1 {
		t.Fatalf("reads=%d roles=%d runs=%d roots=%d error=%v", reads, roles, runs, roots, err)
	}
	err = c.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM harness_skill_outcomes WHERE status='completed' AND effects_uncertain=0 AND run_id IN (?,?)`, result.Execution.RunID, task.ID).Scan(&outcomes)
	if err != nil || outcomes != 2 {
		t.Fatalf("terminal evidence=%d %v", outcomes, err)
	}
	signal := waitSkillSignal(t, ctx, c, 4)
	if signal.RootID != result.Execution.RunID || signal.RootSessionID != session.ID || signal.RunCount != 2 || signal.RoleCount != 2 || signal.CompletedRuns != 2 || signal.Attribution != "unassessed" {
		t.Fatalf("root aggregation=%+v", signal)
	}
	listed, err := c.ManageLocal(ctx, localManagementFields("skill.signals.list", map[string]string{"source_id": "project"}))
	if err != nil || len(listed.(harness.SkillSignalPage).Signals) != 1 {
		t.Fatalf("MANAGE signals=%+v %v", listed, err)
	}
	request := localManagementFields("skill.signals.get", map[string]string{"signal_id": signal.ID})
	request.Fields["limit"] = json.RawMessage(`1`)
	detail, err := c.ManageLocal(ctx, request)
	if err != nil || len(detail.(harness.SkillSignalEvidence).Reads) != 1 || detail.(harness.SkillSignalEvidence).Next == "" {
		t.Fatalf("MANAGE signal evidence=%+v %v", detail, err)
	}
	request.Fields["root_id"] = json.RawMessage(`"foreign"`)
	if _, err = c.ManageLocal(ctx, request); err == nil {
		t.Fatal("management accepted a supplied root identity")
	}
	// Deleting the owned terminal tree removes private evidence together with
	// the source sessions. Published skill content remains installed.
	if err = c.CloseSession(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = c.service.DeleteSession(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	if err = c.store.DB().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM harness_skill_reads)+(SELECT count(*) FROM harness_skill_outcomes)+(SELECT count(*) FROM df_skill_signals)+(SELECT count(*) FROM df_skill_signal_reads)+(SELECT count(*) FROM df_skill_collection_jobs)`).Scan(&reads); err != nil || reads != 0 {
		t.Fatalf("session deletion retained private evidence: %d %v", reads, err)
	}
	remaining, err := c.ListSkills(ctx, harness.SkillSelection{Workspace: workspace})
	if err != nil || len(remaining) != 1 || remaining[0].Ref != installed.Ref {
		t.Fatalf("session deletion changed installation: %+v %v", remaining, err)
	}
}

func waitSkillSignal(t *testing.T, ctx context.Context, c *Client, reads int64) harness.SkillSignal {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		page, err := c.ListSkillSignals(ctx, harness.SkillQuery{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Signals) == 1 && page.Signals[0].ReadCount == reads && !page.Signals[0].CollectionPending {
			return page.Signals[0]
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-deadline.C:
			t.Fatalf("collector did not settle: %+v", page)
		case <-ticker.C:
		}
	}
}
