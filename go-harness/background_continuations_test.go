package deerflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSDKBackgroundParentContinuation(t *testing.T) {
	for _, name := range []string{"BB", "BFB", "FBB"} {
		t.Run(name, func(t *testing.T) { runBackgroundParentContinuation(t, name, false, "", false) })
		t.Run(name+"_automatic", func(t *testing.T) { runBackgroundParentContinuation(t, name, true, "", false) })
	}
}

func TestSDKBackgroundParentContinuationCancellation(t *testing.T) {
	for _, modes := range []string{"BB", "BFB", "FBB"} {
		t.Run(modes+"_subtree", func(t *testing.T) { runBackgroundParentContinuation(t, modes, false, "subtree", false) })
		t.Run(modes+"_automatic_root", func(t *testing.T) { runBackgroundParentContinuation(t, modes, true, "root", false) })
	}
}

func TestSDKBackgroundParentContinuationSiblings(t *testing.T) {
	for _, modes := range []string{"BB", "BFB", "FBB"} {
		t.Run(modes, func(t *testing.T) { runBackgroundParentContinuation(t, modes, false, "", true) })
	}
}

func runBackgroundParentContinuation(t *testing.T, modes string, automatic bool, stopKind string, siblings bool) {
	mixed := len(modes) == 3
	var parentContinuations, rootSummaries, secondContinuations atomic.Int32
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
		hasTool, note, wrote, status := false, false, false, false
		notificationCount, delegated := 0, 0
		var leaf string
		for _, m := range req.Messages {
			all += m.Content + "\n"
			hasTool = hasTool || m.Role == "tool"
			note = note || m.Role == "user" && strings.Contains(m.Content, "Background task notification.")
			if m.Role == "user" && strings.Contains(m.Content, "Background task notification.") {
				notificationCount++
			}
			if m.Role == "tool" {
				wrote = wrote || strings.Contains(m.Content, "background-parent.txt")
				var task struct {
					ID     string `json:"id"`
					Status string `json:"status"`
				}
				if json.Unmarshal([]byte(m.Content), &task) == nil && task.ID != "" {
					delegated++
					leaf = task.ID
					status = status || task.Status == "completed"
				}
			}
		}
		msg := map[string]any{"role": "assistant", "content": "scheduled " + req.Model}
		tool, args := "", ""
		toolSuffix := ""
		switch req.Model {
		case "root":
			if strings.Contains(all, "LEAF_PRIVATE_RESULT") || strings.Contains(all, "PARENT_PRIVATE_TASK") {
				t.Error("private context leaked into root")
			}
			if note {
				if strings.Contains(all, "PARENT_FINAL_SUMMARY") || strings.Contains(all, "HOST_FINAL_SUMMARY") {
					rootSummaries.Add(1)
				}
				msg["content"] = "root accepted summary"
			} else if !hasTool {
				role, instruction := "parent", "PARENT_PRIVATE_TASK"
				if mixed {
					role, instruction = "host", "HOST_PRIVATE_TASK"
				}
				tool, args = "background_agent", fmt.Sprintf(`{"role":%q,"instruction":%q}`, role, instruction)
				if modes[0] == 'F' {
					tool, args = "task", fmt.Sprintf(`{"subagent_type":%q,"prompt":%q,"description":"private host"}`, role, instruction)
				}
			}
		case "host":
			if strings.Contains(all, "ROOT_PRIVATE_TASK") || strings.Contains(all, "LEAF_PRIVATE_RESULT") {
				t.Error("wrong host private history")
			}
			if note {
				if strings.Contains(all, "PARENT_FINAL_SUMMARY") {
					msg["content"] = "HOST_FINAL_SUMMARY"
				}
			} else if !hasTool {
				tool, args = "task", `{"subagent_type":"parent","prompt":"PARENT_PRIVATE_TASK","description":"private parent"}`
				if modes[1] == 'B' {
					tool, args = "background_agent", `{"role":"parent","instruction":"PARENT_PRIVATE_TASK"}`
				}
			}
		case "parent":
			if strings.Contains(all, "ROOT_PRIVATE_TASK") || strings.Contains(all, "HOST_PRIVATE_TASK") || !strings.Contains(all, "PARENT_PRIVATE_TASK") || !strings.Contains(all, "PINNED_PARENT_POLICY") {
				t.Error("parent private history or pinned policy changed")
			}
			if note {
				parentContinuations.Add(1)
				if !strings.Contains(all, "LEAF_PRIVATE_RESULT") {
					t.Error("parent lost leaf result")
				}
				if siblings && notificationCount == 2 {
					if !wrote || !strings.Contains(all, "PARENT_FINAL_SUMMARY") || !strings.Contains(all, "LEAF_PRIVATE_RESULT_1") || !strings.Contains(all, "LEAF_PRIVATE_RESULT_2") {
						t.Error("second continuation did not inherit the last committed private conversation")
					}
					secondContinuations.Add(1)
					msg["content"] = "PARENT_SECOND_SUMMARY"
				} else if !status {
					tool, args = "task_status", fmt.Sprintf(`{"taskId":%q}`, leaf)
				} else if !wrote {
					tool, args = "write_file", `{"path":"background-parent.txt","content":"once"}`
				} else {
					msg["content"] = "PARENT_FINAL_SUMMARY"
				}
			} else if siblings && delegated < 2 {
				tool, args = "background_agent", fmt.Sprintf(`{"role":"leaf","instruction":"LEAF_PRIVATE_TASK_%d"}`, delegated+1)
				toolSuffix = fmt.Sprintf("-%d", delegated+1)
			} else if !hasTool {
				tool, args = "background_agent", `{"role":"leaf","instruction":"LEAF_PRIVATE_TASK"}`
			}
		case "leaf":
			msg["content"] = "LEAF_PRIVATE_RESULT"
			if siblings {
				msg["content"] = "LEAF_PRIVATE_RESULT_1"
				if strings.Contains(all, "LEAF_PRIVATE_TASK_2") {
					msg["content"] = "LEAF_PRIVATE_RESULT_2"
				}
			}
		default:
			t.Errorf("unknown model %s", req.Model)
		}
		finish := "stop"
		if tool != "" {
			msg["content"] = nil
			msg["tool_calls"] = []any{map[string]any{"index": 0, "id": tool + "-" + req.Model + toolSuffix, "type": "function", "function": map[string]string{"name": tool, "arguments": args}}}
			finish = "tool_calls"
		}
		writeTreeModelResponse(w, req.Stream, msg, finish)
	}))
	defer provider.Close()
	// This scenario opens the durable SDK three times. Give setup/recovery its
	// own headroom; each asynchronous task wait still has a shorter hard bound.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	started := time.Now()
	phase := "initial tasks"
	markPhase := func(name string) {
		t.Helper()
		phase = name
		t.Logf("continuation phase=%q elapsed=%s", phase, time.Since(started))
	}
	depth, count, concurrent := 2, 2, 1
	if mixed {
		depth, count = 3, 3
	}
	if siblings {
		count++
	}
	cfg := Config{DataDir: t.TempDir(), Provider: "openai", Model: "root", APIKey: "fixture", BaseURL: provider.URL + "/v1", BackgroundWorkers: 1, PermissionMode: harness.PermissionModeDangerous,
		ModelRoutes: map[string]harness.ModelRoute{}, Subagents: &harness.SubagentsConfig{AutoContinue: &automatic, MaxDepth: &depth, MaxTasksPerRoot: &count, MaxConcurrent: &concurrent, CustomAgents: map[string]harness.AgentConfig{
			"parent": {Description: "parent", SystemPrompt: "PINNED_PARENT_POLICY", Model: "parent", DelegateTo: []string{"leaf"}},
			"leaf":   {Description: "leaf", SystemPrompt: "Leaf", Model: "leaf", Tools: []string{}},
		}}}
	if mixed {
		cfg.Subagents.CustomAgents["host"] = harness.AgentConfig{Description: "host", SystemPrompt: "Host", Model: "host", DelegateTo: []string{"parent"}}
	}
	for _, name := range []string{"parent", "leaf", "host"} {
		cfg.ModelRoutes[name] = harness.ModelRoute{Provider: "openai", Model: name, BaseURL: provider.URL + "/v1", APIKey: "fixture"}
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
	initial, err := c.Run(ctx, sess.ID, []harness.Content{{Type: "text", Text: "ROOT_PRIVATE_TASK"}}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		return harness.AllowOnce, nil
	})
	if err != nil || initial.Execution == nil || initial.Execution.Status != harness.ExecutionCompleted {
		t.Fatalf("initial %+v %v", initial, err)
	}
	waitTask := func(id string, approve bool, want string) harness.BackgroundTask {
		t.Helper()
		wait, stopWait := context.WithTimeout(ctx, 45*time.Second)
		defer stopWait()
		started := time.Now()
		var last []harness.BackgroundTask
		fail := func(err error) {
			t.Helper()
			t.Fatalf("phase=%q waiting for task=%q status=%q approve=%v after %s: %v; last=%+v pool=%+v", phase, id, want, approve, time.Since(started), err, last, c.store.DB().Stats())
		}
		for {
			var tasks []harness.BackgroundTask
			var err error
			if id == "" {
				tasks, err = c.BackgroundTasks(wait, sess.ID, "", 100)
			} else {
				// The original tree already completed. Poll only the continuation
				// instead of repeatedly loading every ancestor and its resources.
				var task harness.BackgroundTask
				task, err = c.BackgroundTask(wait, sess.ID, id)
				tasks = []harness.BackgroundTask{task}
			}
			if err != nil {
				fail(err)
			}
			last = tasks
			allDone := len(tasks) >= 2
			for _, task := range tasks {
				if id != "" && task.ID == id && task.Status == want {
					return task
				}
				if task.Status == "failed" || task.Status == "canceled" {
					t.Fatalf("native task failed: %+v", task)
				}
				allDone = allDone && task.Status == "completed"
				if approve && task.Status == "waiting_input" && task.Interaction != nil {
					_, err = c.ApproveBackgroundTask(wait, sess.ID, task.ID, harness.TaskApproval{ID: task.Interaction.ID, TaskVersion: task.Version, Decision: harness.AllowOnce})
					if err != nil {
						fail(err)
					}
				}
			}
			if id == "" && allDone {
				return harness.BackgroundTask{}
			}
			select {
			case <-wait.Done():
				fail(wait.Err())
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	waitTask("", true, "")
	markPhase("recover completed parent")
	var note string
	for note == "" {
		err = c.store.DB().QueryRowContext(ctx, `SELECT notification_id FROM harness_background_inbox WHERE parent_session_id<>? AND json_extract(CAST(payload AS TEXT),'$.Kind')='completed' ORDER BY seq LIMIT 1`, sess.ID).Scan(&note)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(err)
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c = nil
	cfg.Subagents.CustomAgents = map[string]harness.AgentConfig{}
	reopen := func() {
		c, err = Open(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = c.LoadSession(ctx, sess.ID, workspace, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	reopen()
	markPhase("admit continuation and wait for approval")
	startUpdates := func() (chan harness.RunResult, chan error, context.CancelFunc) {
		updates, done := make(chan harness.RunResult, 16), make(chan error, 1)
		live, stop := context.WithCancel(ctx)
		go func() {
			done <- c.ServeSessionUpdates(live, sess.ID, harness.SessionUpdates{Result: func(ctx context.Context, result harness.RunResult, runErr error) error {
				if runErr != nil {
					return runErr
				}
				select {
				case updates <- result:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}})
		}()
		return updates, done, stop
	}
	var result harness.RunResult
	if automatic {
		updates, done, stop := startUpdates()
		for result.Background == nil {
			select {
			case result = <-updates:
			case err = <-done:
				t.Fatalf("automatic admission %v", err)
			case <-ctx.Done():
				t.Fatal("automatic admission stalled")
			}
		}
		stop()
		if err = <-done; err != nil {
			t.Fatal(err)
		}
	} else {
		result, err = c.ProcessBackgroundNotification(ctx, sess.ID, note, nil, nil)
	}
	if err != nil || result.Background == nil {
		t.Fatalf("private native admission %+v %v", result, err)
	}
	id := result.Background.ID
	firstContinuation := id
	paused := waitTask(id, false, "waiting_input")
	if paused.Interaction == nil || len(paused.Interaction.WaitingInputs) != 1 {
		t.Fatalf("missing new approval %+v", paused)
	}
	if _, err = os.Stat(filepath.Join(workspace, "background-parent.txt")); !os.IsNotExist(err) {
		t.Fatalf("unapproved write: %v", err)
	}
	var roles int
	if err = c.store.DB().QueryRow(`SELECT count(*) FROM harness_agent_invocations`).Scan(&roles); err != nil || roles != count {
		t.Fatalf("continuation spent delegated task quota: %d %v", roles, err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c = nil
	markPhase("recover paused continuation")
	reopen()
	paused, err = c.BackgroundTask(ctx, sess.ID, id)
	if err != nil || paused.Interaction == nil {
		t.Fatalf("lost approval %+v %v", paused, err)
	}
	if stopKind != "" {
		markPhase("cancel " + stopKind)
		before := parentContinuations.Load()
		if stopKind == "subtree" {
			var previous string
			if err = c.store.DB().QueryRow(`SELECT previous_task_id FROM harness_background_continuation_links WHERE task_id=?`, id).Scan(&previous); err != nil {
				t.Fatal(err)
			}
			if _, err = c.CancelBackgroundTask(ctx, sess.ID, previous); err != nil {
				t.Fatal(err)
			}
		} else if err = c.Cancel(sess.ID); err != nil {
			t.Fatal(err)
		}
		waitTask(id, false, "canceled")
		if _, err = c.ApproveBackgroundTask(ctx, sess.ID, id, harness.TaskApproval{ID: paused.Interaction.ID, TaskVersion: paused.Version, Decision: harness.AllowOnce}); err == nil {
			t.Fatal("stopped continuation accepted its old approval")
		}
		if _, err = c.ProcessBackgroundNotification(ctx, sess.ID, note, nil, nil); err == nil {
			t.Fatal("stopped continuation source was admitted again")
		}
		var grants, effects int
		if err = c.store.DB().QueryRow(`SELECT count(*) FROM harness_background_permission_grants WHERE task_id=?`, id).Scan(&grants); err != nil {
			t.Fatal(err)
		}
		if err = c.store.DB().QueryRow(`SELECT count(*) FROM harness_tool_receipts WHERE run_id=? AND json_extract(CAST(receipt AS TEXT),'$.toolName')='write_file' AND state IN ('started','completed','uncertain')`, id).Scan(&effects); err != nil {
			t.Fatal(err)
		}
		if grants != 0 || effects != 0 || parentContinuations.Load() != before {
			t.Fatalf("cancelled continuation ran: grants=%d effects=%d model=%d/%d", grants, effects, parentContinuations.Load(), before)
		}
		if _, err = os.Stat(filepath.Join(workspace, "background-parent.txt")); !os.IsNotExist(err) {
			t.Fatalf("cancelled continuation wrote a file: %v", err)
		}
		return
	}
	markPhase("approve continuation")
	var sourcePayload []byte
	if err = c.store.DB().QueryRow(`SELECT payload FROM harness_background_continuation_sources WHERE task_id=?`, id).Scan(&sourcePayload); err != nil {
		t.Fatal(err)
	}
	if _, err = c.store.DB().Exec(`UPDATE harness_background_continuation_sources SET payload='{}' WHERE task_id=?`, id); err != nil {
		t.Fatal(err)
	}
	beforeApproval := parentContinuations.Load()
	if _, err = c.ApproveBackgroundTask(ctx, sess.ID, id, harness.TaskApproval{ID: paused.Interaction.ID, TaskVersion: paused.Version, Decision: harness.AllowOnce}); err == nil {
		t.Fatal("corrupt saved continuation source was approved")
	}
	var grants int
	if err = c.store.DB().QueryRow(`SELECT count(*) FROM harness_background_permission_grants WHERE task_id=?`, id).Scan(&grants); err != nil || grants != 0 || parentContinuations.Load() != beforeApproval {
		t.Fatalf("corruption consumed an approval or called the model: %d %v", grants, err)
	}
	if _, err = c.store.DB().Exec(`UPDATE harness_background_continuation_sources SET payload=? WHERE task_id=?`, sourcePayload, id); err != nil {
		t.Fatal(err)
	}
	if _, err = c.ApproveBackgroundTask(ctx, sess.ID, id, harness.TaskApproval{ID: paused.Interaction.ID, TaskVersion: paused.Version, Decision: harness.AllowOnce}); err != nil {
		t.Fatal(err)
	}
	waitTask(id, false, "completed")
	markPhase("propagate continuation result")
	if data, err := os.ReadFile(filepath.Join(workspace, "background-parent.txt")); err != nil || string(data) != "once" {
		t.Fatalf("write %q %v", data, err)
	}
	before := parentContinuations.Load()
	replay, err := c.ProcessBackgroundNotification(ctx, sess.ID, note, nil, nil)
	if err != nil || replay.Background == nil || replay.Background.ID != id || parentContinuations.Load() != before {
		t.Fatalf("duplicate private notification replayed: %+v %v", replay, err)
	}
	// The new native task's summary belongs to its original logical parent.
	if automatic {
		updates, done, stop := startUpdates()
		completed := false
		for !completed {
			select {
			case summary := <-updates:
				completed = rootSummaries.Load() > 0 && summary.Execution != nil && summary.Execution.Status == harness.ExecutionCompleted
			case err = <-done:
				t.Fatalf("automatic ancestor summary %v", err)
			case <-ctx.Done():
				t.Fatal("automatic ancestor summary stalled")
			}
		}
		stop()
		if err = <-done; err != nil {
			t.Fatal(err)
		}
		var used, admitted int
		if err = c.store.DB().QueryRow(`SELECT used FROM harness_automatic_continuations WHERE root_id=?`, initial.Execution.RunID).Scan(&used); err != nil {
			t.Fatal(err)
		}
		if err = c.store.DB().QueryRow(`SELECT (SELECT count(*) FROM harness_continuation_sources WHERE root_budget_id=?)+(SELECT count(*) FROM harness_background_continuation_sources WHERE root_id=?)`, initial.Execution.RunID, initial.Execution.RunID).Scan(&admitted); err != nil || used != admitted {
			t.Fatalf("shared automatic quota used=%d admitted=%d %v", used, admitted, err)
		}
	} else {
		var pendingNote string
		for hop := 0; hop < 3; hop++ {
			next := pendingNote
			pendingNote = ""
			for next == "" {
				err = c.store.DB().QueryRow(`SELECT notification_id FROM harness_background_inbox WHERE task_id=? AND json_extract(CAST(payload AS TEXT),'$.Kind')='completed'`, id).Scan(&next)
				if err == nil {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal(err)
				case <-time.After(20 * time.Millisecond):
				}
			}
			result, err = c.ProcessBackgroundNotification(ctx, sess.ID, next, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if result.Background == nil {
				if rootSummaries.Load() == 0 && result.Execution != nil {
					pendingNote = "role-result/" + result.Execution.RunID
					continue
				}
				break
			}
			id = result.Background.ID
			waitTask(id, false, "completed")
		}
	}
	if rootSummaries.Load() != 1 {
		t.Fatalf("summary did not reach root: %d", rootSummaries.Load())
	}
	if siblings {
		var siblingNote string
		if err = c.store.DB().QueryRow(`SELECT notification_id FROM harness_background_inbox WHERE parent_session_id=(SELECT parent_session_id FROM harness_background_inbox WHERE notification_id=?) AND notification_id<>? AND json_extract(CAST(payload AS TEXT),'$.Kind')='completed' ORDER BY seq LIMIT 1`, note, note).Scan(&siblingNote); err != nil {
			t.Fatal(err)
		}
		second, err := c.ProcessBackgroundNotification(ctx, sess.ID, siblingNote, nil, nil)
		if err != nil || second.Background == nil || second.Background.ID == firstContinuation {
			t.Fatalf("second sibling admission: %+v %v", second, err)
		}
		finished := waitTask(second.Background.ID, false, "completed")
		if finished.ContinuationOf != firstContinuation || secondContinuations.Load() != 1 {
			t.Fatalf("sibling reset the native host or private history: %+v calls=%d", finished, secondContinuations.Load())
		}
		if err = c.store.DB().QueryRow(`SELECT count(*) FROM harness_agent_invocations`).Scan(&roles); err != nil || roles != count {
			t.Fatalf("second continuation spent delegated task quota: %d %v", roles, err)
		}
	}
	var leaks int
	if err = c.store.DB().QueryRow(`SELECT count(*) FROM eino_session_events WHERE session_id=? AND CAST(payload AS TEXT) LIKE '%LEAF_PRIVATE_RESULT%'`, sess.ID).Scan(&leaks); err != nil || leaks != 0 {
		t.Fatalf("private native history leaked %d %v", leaks, err)
	}
	if err = c.CloseSession(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	// Completion events can precede the final outbox ACK. Deletion must keep
	// rejecting an unsettled graph, then succeed once native cleanup finishes.
	for {
		_, err = c.service.DeleteSession(ctx, sess.ID)
		if err == nil {
			break
		}
		if !errors.Is(err, harness.ErrBusy) {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			var tasks, leases, notifications int
			_ = c.store.DB().QueryRow(`SELECT count(*) FROM eino_background_tasks WHERE status NOT IN ('completed','failed','canceled')`).Scan(&tasks)
			_ = c.store.DB().QueryRow(`SELECT count(*) FROM harness_background_child_leases`).Scan(&leases)
			_ = c.store.DB().QueryRow(`SELECT count(*) FROM eino_task_notifications`).Scan(&notifications)
			t.Fatalf("graph did not settle for deletion: %v; active=%d leases=%d outbox=%d", err, tasks, leases, notifications)
		case <-time.After(20 * time.Millisecond):
		}
	}
	for _, table := range []string{"harness_background_continuation_sources", "harness_background_continuation_links", "harness_role_contexts", "harness_background_specs"} {
		var count int
		if err = c.store.DB().QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("continuation data survived deletion %s: %d %v", table, count, err)
		}
	}
}
