package deerflow

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSDKPrivateParentContinuationKeepsContextAndApprovalAcrossRestart(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		name := "explicit"
		if automatic {
			name = "automatic"
		}
		t.Run(name, func(t *testing.T) { runPrivateParentContinuation(t, automatic, false, false) })
		t.Run(name+"_two_private_ancestors", func(t *testing.T) { runPrivateParentContinuation(t, automatic, true, false) })
		t.Run(name+"_ordinary_failure", func(t *testing.T) { runPrivateParentContinuation(t, automatic, false, true) })
	}
}

func runPrivateParentContinuation(t *testing.T, automatic, ancestor, fail bool) {
	var rootCalls, continuations atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string
			Stream   bool
			Messages []struct{ Role, Content string }
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			http.Error(w, "fixture", 400)
			return
		}
		all := ""
		hasTool, continuation, wrote := false, false, false
		for _, message := range req.Messages {
			all += message.Content + "\n"
			hasTool = hasTool || message.Role == "tool"
			continuation = continuation || message.Role == "user" && strings.Contains(message.Content, "Background task notification.")
			wrote = wrote || message.Role == "tool" && strings.Contains(message.Content, "private-parent.txt")
		}
		message := map[string]any{"role": "assistant", "content": "finished " + req.Model}
		toolName, args := "", ""
		switch req.Model {
		case "root":
			rootCalls.Add(1)
			if continuation && strings.Contains(all, "LEAF_FINISHED_RESULT") {
				t.Error("raw grandchild result leaked past private parent")
			}
			if !hasTool {
				toolName, args = "task", `{"subagent_type":"parent","prompt":"PARENT_PRIVATE_TASK","description":"private parent"}`
				if ancestor {
					args = `{"subagent_type":"grandparent","prompt":"GRAND_PRIVATE_TASK","description":"private ancestor"}`
				}
			}
		case "grandparent":
			if strings.Contains(all, "ROOT_PRIVATE_INPUT") || strings.Contains(all, "LEAF_FINISHED_RESULT") {
				t.Error("private ancestor received the wrong history")
			}
			if !strings.Contains(all, "GRAND_PRIVATE_TASK") {
				t.Error("ancestor lost its private task")
			}
			if !hasTool {
				toolName, args = "task", `{"subagent_type":"parent","prompt":"PARENT_PRIVATE_TASK","description":"private parent"}`
			}
		case "parent":
			if strings.Contains(all, "ROOT_PRIVATE_INPUT") {
				t.Error("root conversation leaked to private parent")
			}
			if !strings.Contains(all, "PARENT_PRIVATE_TASK") || !strings.Contains(all, "PARENT_POLICY_V1") {
				t.Error("private task/instruction missing")
			}
			if continuation {
				continuations.Add(1)
				if !strings.Contains(all, "LEAF_FINISHED_RESULT") || !hasTool {
					t.Error("private saved history or descendant result missing")
				}
				if !wrote {
					toolName, args = "write_file", `{"path":"private-parent.txt","content":"once"}`
				} else if fail {
					http.Error(w, "private provider failure detail", http.StatusBadRequest)
					return
				}
			} else if !hasTool {
				toolName, args = "background_agent", `{"role":"leaf","instruction":"LEAF_PRIVATE_TASK"}`
			}
		case "leaf":
			if strings.Contains(all, "ROOT_PRIVATE_INPUT") || strings.Contains(all, "PARENT_PRIVATE_TASK") {
				t.Error("ancestor private input leaked to leaf")
			}
			message["content"] = "LEAF_FINISHED_RESULT"
		default:
			t.Errorf("unknown model %s", req.Model)
		}
		finish := "stop"
		if toolName != "" {
			message["content"], message["tool_calls"] = nil, []any{map[string]any{"index": 0, "id": toolName + "-" + req.Model, "type": "function", "function": map[string]string{"name": toolName, "arguments": args}}}
			finish = "tool_calls"
		}
		writeTreeModelResponse(w, req.Stream, message, finish)
	}))
	defer provider.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	depth, maxTasks, maxAuto := 2, 2, 2
	if ancestor {
		depth, maxTasks, maxAuto = 3, 3, 3
	}
	cfg := Config{DataDir: t.TempDir(), Provider: "openai", Model: "root", APIKey: "fixture", BaseURL: provider.URL + "/v1", BackgroundWorkers: 1, PermissionMode: harness.PermissionModeDangerous,
		ModelRoutes: map[string]harness.ModelRoute{}, Subagents: &harness.SubagentsConfig{MaxDepth: &depth, MaxTasksPerRoot: &maxTasks, AutoContinue: &automatic, MaxAutoContinuations: &maxAuto,
			CustomAgents: map[string]harness.AgentConfig{
				"parent": {Description: "parent", SystemPrompt: "PARENT_POLICY_V1", Model: "parent", DelegateTo: []string{"leaf"}},
				"leaf":   {Description: "leaf", SystemPrompt: "Leaf only", Model: "leaf", Tools: []string{}},
			}}}
	if ancestor {
		cfg.Subagents.CustomAgents["grandparent"] = harness.AgentConfig{Description: "ancestor", SystemPrompt: "GRAND_POLICY_V1", Model: "grandparent", DelegateTo: []string{"parent"}}
	}
	for _, model := range []string{"parent", "leaf", "grandparent"} {
		cfg.ModelRoutes[model] = harness.ModelRoute{Provider: "openai", Model: model, APIKey: "fixture", BaseURL: provider.URL + "/v1"}
	}
	c, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if c != nil {
			_ = c.Close()
		}
	}()
	workspace := t.TempDir()
	sess, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := c.Run(ctx, sess.ID, []harness.Content{{Type: "text", Text: "ROOT_PRIVATE_INPUT"}}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		return harness.AllowOnce, nil
	})
	if err != nil || initial.Execution == nil || initial.Execution.Status != harness.ExecutionCompleted {
		t.Fatalf("initial %+v %v", initial, err)
	}
	var note harness.BackgroundNotification
	for note.ID == "" {
		notes, err := c.BackgroundNotifications(ctx, sess.ID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range notes {
			if n.Kind == "completed" {
				note = n
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("child never completed")
		case <-time.After(10 * time.Millisecond):
		}
	}
	var parentRole string
	var contextCount int
	if err = c.store.DB().QueryRow(`SELECT role_id FROM harness_role_contexts WHERE json_extract(CAST(payload AS TEXT),'$.invocation.spec.name')='parent' ORDER BY revision DESC LIMIT 1`).Scan(&parentRole); err != nil {
		t.Fatal(err)
	}
	if err = c.store.DB().QueryRow(`SELECT count(*) FROM harness_role_contexts`).Scan(&contextCount); err != nil || contextCount != depth {
		t.Fatalf("contexts %d %v", contextCount, err)
	}
	rootCount := rootCalls.Load()
	if automatic {
		// Queue a corrupt derived result before the real role summary. It
		// must be quarantined without starving subsequent notifications.
		bad := bt.Notification{ID: "role-result/corrupt", TaskID: note.TaskID, SessionID: sess.ID, Kind: "role_result", Version: note.TaskVersion, Data: json.RawMessage(`{}`), CreatedAt: time.Now()}
		payload, err := json.Marshal(bad)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = c.store.DB().Exec(`INSERT INTO harness_background_inbox(notification_id,parent_session_id,task_id,payload) VALUES(?,?,?,?)`, bad.ID, sess.ID, note.TaskID, payload); err != nil {
			t.Fatal(err)
		}
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c = nil
	cfg.Subagents.CustomAgents = map[string]harness.AgentConfig{}
	c, err = Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.LoadSession(ctx, sess.ID, workspace, false, nil); err != nil {
		t.Fatal(err)
	}
	var resumed harness.RunResult
	if automatic {
		fixture := &continuationFixture{c: c, ctx: ctx, sess: sess, parent: initial, note: note}
		results, done, stop := startContinuationUpdates(t, fixture, nil)
		select {
		case resumed = <-results:
		case err := <-done:
			t.Fatal(err)
		case <-ctx.Done():
			t.Fatal("private automatic continuation stalled")
		}
		stop()
		if err = <-done; err != nil {
			t.Fatal(err)
		}
	} else {
		resumed, err = c.ProcessBackgroundNotification(ctx, sess.ID, note.ID, nil, nil)
	}
	if err != nil || resumed.Execution == nil || resumed.Execution.Status != harness.ExecutionWaitingInput || len(resumed.Execution.WaitingInputs) != 1 {
		t.Fatalf("private new approval %+v %v", resumed, err)
	}
	if _, err = os.Stat(filepath.Join(workspace, "private-parent.txt")); !os.IsNotExist(err) {
		t.Fatalf("unapproved effect %v", err)
	}
	if rootCalls.Load() != rootCount || continuations.Load() != 1 {
		t.Fatalf("wrong target/spending root=%d continuations=%d", rootCalls.Load(), continuations.Load())
	}
	// The continuation itself must remain resumable after another restart.
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c = nil
	c, err = Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.LoadSession(ctx, sess.ID, workspace, false, nil); err != nil {
		t.Fatal(err)
	}
	state, err := c.Execution(ctx, sess.ID, resumed.Execution.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var originalContext []byte
	if err = c.store.DB().QueryRow(`SELECT payload FROM harness_role_contexts WHERE role_id=? AND revision=1`, parentRole).Scan(&originalContext); err != nil {
		t.Fatal(err)
	}
	if _, err = c.store.DB().Exec(`UPDATE harness_role_contexts SET payload='{}' WHERE role_id=? AND revision=1`, parentRole); err != nil {
		t.Fatal(err)
	}
	blocked, err := c.Execution(ctx, sess.ID, state.RunID)
	if err != nil || blocked.Resumable || blocked.BlockedReason == "" {
		t.Fatalf("corrupt conversation remained resumable: %+v %v", blocked, err)
	}
	_, err = c.ResumeExecution(ctx, sess.ID, harness.ResumeExecutionRequest{RunID: state.RunID, ExpectedVersion: state.Version}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		t.Error("corrupt source reached permission callback")
		return harness.AllowOnce, nil
	})
	if !errors.Is(err, harness.ErrExecutionConflict) || rootCalls.Load() != rootCount || continuations.Load() != 1 {
		t.Fatalf("corrupt source reached execution: %v", err)
	}
	if _, err = os.Stat(filepath.Join(workspace, "private-parent.txt")); !os.IsNotExist(err) {
		t.Fatalf("corrupt source caused an effect: %v", err)
	}
	if _, err = c.store.DB().Exec(`UPDATE harness_role_contexts SET payload=? WHERE role_id=? AND revision=1`, originalContext, parentRole); err != nil {
		t.Fatal(err)
	}
	finished, err := c.ResumeExecution(ctx, sess.ID, harness.ResumeExecutionRequest{RunID: state.RunID, ExpectedVersion: state.Version}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		return harness.AllowOnce, nil
	})
	wantStatus, wantRevisions := harness.ExecutionCompleted, 2
	if fail {
		wantStatus, wantRevisions = harness.ExecutionFailed, 1
	}
	if err != nil || finished.Execution == nil || finished.Execution.Status != wantStatus {
		t.Fatalf("resume %+v %v", finished, err)
	}
	data, err := os.ReadFile(filepath.Join(workspace, "private-parent.txt"))
	if err != nil || string(data) != "once" {
		t.Fatalf("effect %q %v", data, err)
	}
	var revisions, roles, nativeLeaks int
	if err = c.store.DB().QueryRow(`SELECT count(*) FROM harness_role_contexts WHERE role_id=?`, parentRole).Scan(&revisions); err != nil || revisions != wantRevisions {
		t.Fatalf("role context revisions %d %v", revisions, err)
	}
	var summary struct {
		Succeeded bool   `json:"succeeded"`
		Result    string `json:"result"`
	}
	var encodedSummary []byte
	if err = c.store.DB().QueryRow(`SELECT payload FROM harness_role_results WHERE source_run_id=?`, state.RunID).Scan(&encodedSummary); err != nil || json.Unmarshal(encodedSummary, &summary) != nil || summary.Succeeded == fail || strings.Contains(summary.Result, "private provider failure detail") {
		t.Fatalf("invalid or leaking summary: %s %v", encodedSummary, err)
	}
	if err = c.store.DB().QueryRow(`SELECT count(*) FROM harness_agent_invocations`).Scan(&roles); err != nil || roles != depth {
		t.Fatalf("continuation spent another task %d %v", roles, err)
	}
	if err = c.store.DB().QueryRow(`SELECT count(*) FROM eino_session_events WHERE session_id=? AND CAST(payload AS TEXT) LIKE '%LEAF_FINISHED_RESULT%'`, sess.ID).Scan(&nativeLeaks); err != nil || nativeLeaks != 0 {
		t.Fatalf("private continuation polluted root history %d %v", nativeLeaks, err)
	}
	before := continuations.Load()
	duplicate, err := c.ProcessBackgroundNotification(ctx, sess.ID, note.ID, nil, nil)
	if err != nil || duplicate.Execution == nil || duplicate.Execution.RunID != state.RunID || continuations.Load() != before {
		t.Fatalf("duplicate %+v %v", duplicate, err)
	}
	// Each summary is a new durable notification to the immediate ancestor.
	// Both manual and automatic dispatch share original root budgeting.
	if automatic {
		results, done, stop := startContinuationUpdates(t, &continuationFixture{c: c, ctx: ctx, sess: sess, parent: initial}, nil)
		for range depth - 1 {
			select {
			case r := <-results:
				if r.Execution == nil || r.Execution.Status != harness.ExecutionCompleted {
					t.Fatalf("ancestor %+v", r)
				}
			case e := <-done:
				t.Fatal(e)
			case <-ctx.Done():
				t.Fatal("ancestor routing stalled")
			}
		}
		stop()
		if err = <-done; err != nil {
			t.Fatal(err)
		}
		var used int
		if err = c.store.DB().QueryRow(`SELECT used FROM harness_automatic_continuations WHERE root_id=?`, initial.Execution.RunID).Scan(&used); err != nil || used != depth {
			t.Fatalf("ancestor root allowance %d %v", used, err)
		}
		var blockedState string
		if err = c.store.DB().QueryRow(`SELECT state FROM harness_notification_work WHERE notification_id='role-result/corrupt'`).Scan(&blockedState); err != nil || blockedState != "blocked" {
			t.Fatalf("corrupt result not isolated: %q %v", blockedState, err)
		}
	} else {
		from := state.RunID
		for range depth - 1 {
			r, err := c.ProcessBackgroundNotification(ctx, sess.ID, "role-result/"+from, nil, nil)
			if err != nil || r.Execution == nil || r.Execution.Status != harness.ExecutionCompleted {
				t.Fatalf("explicit ancestor %+v %v", r, err)
			}
			from = r.Execution.RunID
		}
	}
	if rootCalls.Load() != rootCount+1 {
		t.Fatalf("root did not receive one summary: %d want %d", rootCalls.Load(), rootCount+1)
	}
	if err = c.store.DB().QueryRow(`SELECT count(*) FROM eino_session_events WHERE session_id=? AND CAST(payload AS TEXT) LIKE '%LEAF_FINISHED_RESULT%'`, sess.ID).Scan(&nativeLeaks); err != nil || nativeLeaks != 0 {
		t.Fatalf("raw leaf result entered root history %d %v", nativeLeaks, err)
	}
	if _, err = c.store.DB().Exec(`INSERT INTO harness_role_context_stages(run_id,attempt_id,role_id,payload) SELECT run_id,'orphan',role_id,payload FROM harness_role_contexts WHERE revision=1`); err != nil {
		t.Fatal(err)
	}
	if err = c.CloseSession(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = c.service.DeleteSession(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"harness_role_context_stages", "harness_role_contexts", "harness_role_results", "harness_continuation_sources", "harness_background_inbox"} {
		var count int
		if err = c.store.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("private data survived deletion in %s: %d %v", table, count, err)
		}
	}
}
