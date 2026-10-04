package runtime

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
)

type taskTreeCursor struct {
	Version int    `json:"v"`
	Owner   string `json:"owner"`
	Session string `json:"session"`
	Filter  string `json:"filter"`
	Root    string `json:"root"`
	Node    string `json:"node"`
}

func treeIdentity(s string) bool {
	return s != "" && len(s) <= 4096 && utf8.ValidString(s) && !strings.ContainsAny(s, "\x00\r\n")
}
func treeOwner(owner string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(owner))) }
func treeTables(ctx context.Context, tx *sql.Tx) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name IN ('budget_roots','budget_operation_owners','harness_agent_invocations','harness_agent_states','harness_background_bindings','harness_background_specs','harness_background_resources','harness_background_intent_roles','harness_task_tree_stops','harness_artifacts','harness_assets','harness_artifact_owners')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]bool{}
	for rows.Next() {
		var n string
		if err = rows.Scan(&n); err != nil {
			return nil, err
		}
		m[n] = true
	}
	return m, rows.Err()
}

func (s *Service) TaskTree(ctx context.Context, actor harness.TaskActor, q harness.TaskTreeQuery) (harness.TaskTreePage, error) {
	page := harness.TaskTreePage{SessionID: actor.SessionID, Nodes: []harness.TaskTreeNode{}, Consistency: "live_keyset"}
	if q.Limit < 0 || q.Limit > 100 || q.RootID != "" && !treeIdentity(q.RootID) || len(q.Cursor) > 16384 {
		return page, harness.ErrInvalidInput
	}
	if q.Limit == 0 {
		q.Limit = 100
	}
	if err := s.AuthorizeTaskAccess(ctx, actor); err != nil {
		return page, err
	}
	cursor := taskTreeCursor{Version: 1, Owner: treeOwner(actor.OwnerID), Session: actor.SessionID, Filter: q.RootID}
	if q.Cursor != "" {
		data, err := base64.RawURLEncoding.DecodeString(q.Cursor)
		if err != nil || json.Unmarshal(data, &cursor) != nil || cursor.Version != 1 || cursor.Owner != treeOwner(actor.OwnerID) || cursor.Session != actor.SessionID || cursor.Filter != q.RootID || !treeIdentity(cursor.Root) || !treeIdentity(cursor.Node) {
			return page, harness.ErrInvalidInput
		}
	}
	tx, err := s.Store.db.BeginTx(ctx, nil)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	tables, err := treeTables(ctx, tx)
	if err != nil {
		return page, err
	}
	if !tables["budget_roots"] {
		return page, nil
	}
	if err = treeRejectChild(ctx, tx, tables, actor.SessionID); err != nil {
		return page, err
	}
	if q.RootID != "" {
		if err = treeAuthorizeRoot(ctx, tx, actor.SessionID, q.RootID); err != nil {
			return page, err
		}
	}
	query := `SELECT r.id AS root_id,'root/'||r.id AS node_id FROM budget_roots r WHERE r.session_id=?`
	args := []any{actor.SessionID}
	if tables["harness_agent_invocations"] {
		query += ` UNION ALL SELECT i.root_id,i.id FROM harness_agent_invocations i JOIN budget_roots r ON r.id=i.root_id WHERE r.session_id=?`
		args = append(args, actor.SessionID)
	}
	if tables["harness_background_bindings"] {
		query += ` UNION SELECT json_extract(CAST(b.payload AS TEXT),'$.RootBudgetID'),COALESCE(NULLIF(json_extract(CAST(b.payload AS TEXT),'$.InvocationID'),''),'background/'||b.task_id) FROM harness_background_bindings b JOIN budget_roots r ON r.id=json_extract(CAST(b.payload AS TEXT),'$.RootBudgetID') WHERE r.session_id=?`
		args = append(args, actor.SessionID)
	}
	query = `SELECT root_id,node_id FROM (` + query + `) WHERE (?='' OR root_id=?) AND (root_id>? OR (root_id=? AND node_id>?)) ORDER BY root_id,node_id LIMIT ?`
	args = append(args, q.RootID, q.RootID, cursor.Root, cursor.Root, cursor.Node, q.Limit+1)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return page, err
	}
	type pair struct{ root, node string }
	var ids []pair
	for rows.Next() {
		var p pair
		if err = rows.Scan(&p.root, &p.node); err != nil {
			break
		}
		ids = append(ids, p)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return page, err
	}
	page.HasMore = len(ids) > q.Limit
	if page.HasMore {
		ids = ids[:q.Limit]
	}
	for _, id := range ids {
		node, err := s.Store.taskTreeNodeTx(ctx, tx, actor.SessionID, id.root, id.node, tables)
		if err != nil {
			return page, err
		}
		page.Nodes = append(page.Nodes, node)
	}
	if err = s.AuthorizeTaskAccess(ctx, actor); err != nil {
		return page, err
	}
	if page.HasMore {
		last := ids[len(ids)-1]
		cursor.Root, cursor.Node = last.root, last.node
		data, _ := json.Marshal(cursor)
		page.NextCursor = base64.RawURLEncoding.EncodeToString(data)
	}
	if err = tx.Commit(); err != nil {
		return page, err
	}
	for i := range page.Nodes {
		if err = s.taskTreeResources(ctx, actor, &page.Nodes[i]); err != nil {
			return page, err
		}
	}
	return page, s.AuthorizeTaskAccess(ctx, actor)
}

func (s *Service) TaskTreeNode(ctx context.Context, actor harness.TaskActor, root, id string) (harness.TaskTreeNode, error) {
	if !treeIdentity(root) || !treeIdentity(id) {
		return harness.TaskTreeNode{}, harness.ErrInvalidInput
	}
	if err := s.AuthorizeTaskAccess(ctx, actor); err != nil {
		return harness.TaskTreeNode{}, err
	}
	tx, err := s.Store.db.BeginTx(ctx, nil)
	if err != nil {
		return harness.TaskTreeNode{}, err
	}
	defer tx.Rollback()
	n, err := s.Store.TaskTreeNodeTx(ctx, tx, actor.SessionID, root, id)
	if err != nil {
		return n, err
	}
	if err = s.AuthorizeTaskAccess(ctx, actor); err != nil {
		return harness.TaskTreeNode{}, err
	}
	if err = tx.Commit(); err != nil {
		return n, err
	}
	err = s.taskTreeResources(ctx, actor, &n)
	return n, err
}

// Resource connectivity is live host state, projected through the already
// authorized public background controller after releasing the SQLite snapshot.
func (s *Service) taskTreeResources(ctx context.Context, actor harness.TaskActor, n *harness.TaskTreeNode) error {
	if n.Mode != "background" && n.BackgroundExecution == nil || s.Background == nil {
		return nil
	}
	taskID := n.TaskID
	if n.BackgroundExecution != nil {
		taskID = n.BackgroundExecution.TaskID
	}
	task, err := s.Background.BackgroundTask(ctx, actor, taskID)
	if err != nil {
		return err
	}
	n.Resources = task.Resources
	if n.Resources != nil && n.Resources.Status != "ready" {
		if n.Waiting != nil {
			n.Waiting.Resumable = false
		}
		if n.BackgroundExecution != nil {
			n.BackgroundExecution.Resumable = false
		}
		actions := make([]string, 0, len(n.AllowedActions))
		for _, action := range n.AllowedActions {
			if action != "resume" {
				actions = append(actions, action)
			}
		}
		n.AllowedActions = actions
	}
	return s.AuthorizeTaskAccess(ctx, actor)
}

// TaskTreeNodeTx composes inside the caller's transaction. The caller must hold
// current connection authority; this method additionally verifies root scope.
func (s *Store) TaskTreeNodeTx(ctx context.Context, tx *sql.Tx, session, root, id string) (harness.TaskTreeNode, error) {
	tables, err := treeTables(ctx, tx)
	if err != nil {
		return harness.TaskTreeNode{}, err
	}
	if !tables["budget_roots"] {
		return harness.TaskTreeNode{}, harness.ErrNotFound
	}
	if err = treeRejectChild(ctx, tx, tables, session); err != nil {
		return harness.TaskTreeNode{}, err
	}
	return s.taskTreeNodeTx(ctx, tx, session, root, id, tables)
}
func treeRejectChild(ctx context.Context, tx *sql.Tx, tables map[string]bool, session string) error {
	if !tables["harness_background_bindings"] {
		return nil
	}
	var private bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_background_bindings WHERE child_session_id=?)`, session).Scan(&private); err != nil {
		return err
	}
	if private {
		return harness.ErrPermissionDenied
	}
	return nil
}
func treeAuthorizeRoot(ctx context.Context, tx *sql.Tx, session, root string) error {
	var owner string
	err := tx.QueryRowContext(ctx, `SELECT session_id FROM budget_roots WHERE id=?`, root).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) || err == nil && owner != session {
		return harness.ErrNotFound
	}
	return err
}
func treeTime(value string) *time.Time {
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return &t
	}
	return nil
}
func treeText(text string) *harness.TaskTreeResult {
	r := &harness.TaskTreeResult{Text: text}
	if len(r.Text) > 8192 {
		r.Text = r.Text[:8192]
		for !utf8.ValidString(r.Text) {
			r.Text = r.Text[:len(r.Text)-1]
		}
		r.Truncated = true
	}
	return r
}

func (s *Store) taskTreeNodeTx(ctx context.Context, tx *sql.Tx, session, root, id string, tables map[string]bool) (harness.TaskTreeNode, error) {
	n := harness.TaskTreeNode{ID: id, RootID: root, RootSessionID: session, Status: "unavailable", AllowedActions: []string{}}
	if err := treeAuthorizeRoot(ctx, tx, session, root); err != nil {
		return n, err
	}
	if id == "root/"+root {
		return s.taskTreeRootTx(ctx, tx, n, tables)
	}
	n.InvocationID, n.Mode, n.ParentID = id, "foreground", "root/"+root
	found := false
	if tables["harness_agent_invocations"] {
		in, err := agents.ReadInvocation(ctx, tx, id, root)
		if err == nil {
			found = true
			n.Role, n.Model, n.Description, n.Depth = in.Spec.Name, in.Spec.Model, in.Spec.Description, in.Depth
			if in.ParentID != "" {
				n.ParentID = in.ParentID
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return n, err
		}
	}
	if tables["harness_agent_states"] {
		var created, updated, result string
		var available, truncated bool
		var attempt string
		err := tx.QueryRowContext(ctx, `SELECT state,version,attempt_id,result_text,result_available,result_truncated,created_at,updated_at FROM harness_agent_states WHERE invocation_id=? AND root_id=?`, id, root).Scan(&n.Status, &n.Version, &attempt, &result, &available, &truncated, &created, &updated)
		if err == nil {
			n.CreatedAt, n.UpdatedAt = treeTime(created), treeTime(updated)
			if available {
				n.Result = &harness.TaskTreeResult{Text: result, Truncated: truncated}
			}
			if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM harness_agent_attempts WHERE invocation_id=?`, id).Scan(&n.Attempt); err != nil {
				return n, err
			}
			if n.Status == "waiting_input" {
				n.Waiting = &harness.TaskTreeWaiting{Kind: "unavailable"}
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return n, err
		}
	}
	if tables["harness_background_bindings"] {
		var bindingData, taskData []byte
		var blocked string
		err := tx.QueryRowContext(ctx, `SELECT b.payload,t.payload,b.blocked_reason FROM harness_background_bindings b JOIN eino_background_tasks t ON t.id=b.task_id WHERE json_extract(CAST(b.payload AS TEXT),'$.RootBudgetID')=? AND COALESCE(NULLIF(json_extract(CAST(b.payload AS TEXT),'$.InvocationID'),''),'background/'||b.task_id)=? ORDER BY b.rowid DESC LIMIT 1`, root, id).Scan(&bindingData, &taskData, &blocked)
		if err == nil {
			var binding background.Binding
			var task bt.Task
			if json.Unmarshal(bindingData, &binding) != nil || json.Unmarshal(taskData, &task) != nil || binding.RoleID() != id || binding.RootBudgetID != root || binding.RootSession() != session || task.Spec.ID != binding.TaskID {
				return n, harness.ErrExecutionConflict
			}
			found = true
			n.Mode = "background"
			n.TaskID, n.ContinuationOf = binding.TaskID, binding.ContinuationOf
			n.Status, n.Version, n.Attempt = string(task.Status), task.Version, task.Attempt
			n.BlockedReason = blocked
			n.Description = task.Spec.Description
			n.Depth = binding.TaskDepth()
			n.CreatedAt, n.UpdatedAt = &task.CreatedAt, &task.UpdatedAt
			if binding.ParentInvocationID != "" {
				n.ParentID = binding.ParentInvocationID
			} else if binding.ParentTaskID != "" {
				n.ParentID = "background/" + binding.ParentTaskID
			}
			if n.Role == "" {
				n.Role = "unavailable"
			}
			if len(task.ResultData) > 0 && utf8.Valid(task.ResultData) {
				n.Result = treeText(string(task.ResultData))
			}
			if task.Status == bt.StatusWaitingInput {
				n.Waiting = &harness.TaskTreeWaiting{Kind: "unavailable"}
				if err := s.treeBackgroundWait(ctx, tx, binding, &task, &n); err != nil {
					return n, err
				}
			}
			if task.Status == bt.StatusSuspended {
				n.Waiting = &harness.TaskTreeWaiting{Kind: "suspended", Resumable: blocked == ""}
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return n, err
		}
	}
	if !found {
		return n, harness.ErrNotFound
	}
	if err := treeArtifactSummary(ctx, tx, tables, &n); err != nil {
		return n, err
	}
	if tables["budget_operation_owners"] {
		if err := treeInvocationUsage(ctx, tx, &n); err != nil {
			return n, err
		}
	}
	if n.Mode == "foreground" {
		if err := treeExecutionReference(ctx, tx, &n); err != nil {
			return n, err
		}
		if tables["harness_background_intent_roles"] {
			if err := s.treeBackgroundExecutionReference(ctx, tx, &n); err != nil {
				return n, err
			}
		}
	}
	if tables["harness_task_tree_stops"] {
		if err := agents.CheckStopped(ctx, tx, root, id); errors.Is(err, agents.ErrStopped) {
			if n.Status == "running" || n.Status == "pending" || n.Status == "waiting_input" || n.Status == "suspended" {
				n.Status = "cancelling"
			}
			if n.BackgroundExecution != nil && n.BackgroundExecution.Resumable {
				n.AllowedActions = append(n.AllowedActions, "resume")
			}
			return n, nil
		} else if err != nil {
			return n, err
		}
	}
	if n.Version > 0 {
		n.AllowedActions = append(n.AllowedActions, "cancel_subtree")
	}
	if n.Mode == "background" && n.Waiting != nil {
		if n.Waiting.Kind == "permission" || n.Waiting.Kind == "mixed" {
			n.AllowedActions = append(n.AllowedActions, "approve")
		}
		if (n.Status == "suspended" || n.Waiting.Kind == "cancelled_roles") && n.Waiting.Resumable {
			n.AllowedActions = append(n.AllowedActions, "resume")
		}
	}
	return n, nil
}

func (s *Store) taskTreeRootTx(ctx context.Context, tx *sql.Tx, n harness.TaskTreeNode, tables map[string]bool) (harness.TaskTreeNode, error) {
	n.Mode, n.Role = "root", "lead"
	var run, created, updated string
	u := &harness.TaskTreeUsage{Scope: "root_total"}
	err := tx.QueryRowContext(ctx, `SELECT r.root_run_id,r.revision,r.model_calls,r.tool_calls,r.spent_tokens,r.held_tokens,r.blocked_reason,h.status,h.created_at,h.updated_at,s.model FROM budget_roots r JOIN harness_runs h ON h.id=r.root_run_id JOIN harness_sessions s ON s.id=r.session_id WHERE r.id=? AND r.session_id=?`, n.RootID, n.RootSessionID).Scan(&run, &n.Version, &u.ModelCalls, &u.ToolCalls, &u.TotalTokens, &u.HeldTokens, &n.BlockedReason, &n.Status, &created, &updated, &n.Model)
	if errors.Is(err, sql.ErrNoRows) {
		return n, harness.ErrNotFound
	}
	if err != nil {
		return n, err
	}
	n.CreatedAt, n.UpdatedAt = treeTime(created), treeTime(updated)
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(CASE WHEN usage_json IS NOT NULL THEN json_extract(CAST(usage_json AS TEXT),'$.inputTokens') ELSE 0 END),0),COALESCE(SUM(CASE WHEN usage_json IS NOT NULL THEN json_extract(CAST(usage_json AS TEXT),'$.outputTokens') ELSE 0 END),0),COALESCE(MAX(CASE WHEN state<>'settled' OR COALESCE(json_extract(CAST(usage_json AS TEXT),'$.estimated'),0)=1 THEN 1 ELSE 0 END),0) FROM budget_reservations WHERE root_id=? AND kind='model'`, n.RootID).Scan(&u.InputTokens, &u.OutputTokens, &u.Estimated)
	if err != nil {
		return n, err
	}
	n.Usage = u
	if err := treeArtifactSummary(ctx, tx, tables, &n); err != nil {
		return n, err
	}
	if row, err := readExecution(ctx, tx, n.RootSessionID, run); err == nil {
		n.Status, n.Attempt = string(row.State.Status), row.State.Attempt
		n.Model = row.Config.Model
	} else if !errors.Is(err, harness.ErrNotFound) {
		return n, err
	}
	var executionVersions int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(version),0) FROM harness_executions WHERE root_budget_id=? AND session_id=?`, n.RootID, n.RootSessionID).Scan(&executionVersions); err != nil {
		return n, err
	}
	n.Version += executionVersions
	if err = treeExecutionReference(ctx, tx, &n); err != nil {
		return n, err
	}
	if tables["harness_task_tree_stops"] {
		if err := agents.CheckStopped(ctx, tx, n.RootID, ""); errors.Is(err, agents.ErrStopped) {
			if n.Status == "running" || n.Status == "waiting_input" || n.Status == "resuming" {
				n.Status = "cancelling"
			}
			return n, nil
		} else if err != nil {
			return n, err
		}
	}
	n.AllowedActions = []string{"cancel_subtree"}
	return n, nil
}

func treeExecutionReference(ctx context.Context, tx *sql.Tx, n *harness.TaskTreeNode) error {
	rows, err := tx.QueryContext(ctx, `SELECT run_id FROM harness_executions WHERE root_budget_id=? AND session_id=? AND status IN ('running','suspending','waiting_input','resuming','needs_reconciliation') ORDER BY rowid DESC`, n.RootID, n.RootSessionID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		row, err := readExecution(ctx, tx, n.RootSessionID, id)
		if err != nil {
			return err
		}
		matched := n.Mode == "root"
		if !matched && row.Manifest != nil {
			for _, binding := range row.Manifest.Interrupts {
				if binding.RoleID == n.InvocationID && binding.RoleID != "" {
					p, err := readIntentRole(ctx, tx, n.RootID, binding.IntentID)
					if err != nil {
						return err
					}
					matched = p.RoleID == binding.RoleID
					if matched {
						break
					}
				}
			}
			for _, binding := range row.Manifest.TaskWaits {
				if binding.Wait.RoleID == n.InvocationID {
					matched = true
				}
			}
		}
		if !matched {
			continue
		}
		ref := &harness.TaskTreeExecution{RunID: id, Version: row.State.Version, Status: row.State.Status}
		if row.State.Status == harness.ExecutionWaitingInput {
			ref.Resumable = validateExecutionManifest(ctx, tx, row) == nil && executionResumeReadiness(ctx, tx, row) == nil
		}
		n.Execution = ref
		if row.State.Status == harness.ExecutionWaitingInput && row.Manifest != nil {
			w := &harness.TaskTreeWaiting{Kind: "permission", Resumable: ref.Resumable}
			for _, b := range row.Manifest.Interrupts {
				if n.Mode == "root" || b.RoleID == n.InvocationID {
					w.IntentCount++
				}
			}
			for _, b := range row.Manifest.TaskWaits {
				if n.Mode == "root" || b.Wait.RoleID == n.InvocationID {
					w.TaskIDs = append(w.TaskIDs, b.Wait.TaskID)
				}
			}
			if len(w.TaskIDs) > 0 {
				w.Kind = "task_wait"
				if w.IntentCount > 0 {
					w.Kind = "mixed"
				}
			}
			n.Waiting = w
		}
		if n.Mode == "root" {
			n.Status = string(row.State.Status)
			n.Model = row.Config.Model
			n.Attempt = row.State.Attempt
		}
		return nil
	}
	return nil
}

func (s *Store) treeBackgroundWait(ctx context.Context, tx *sql.Tx, b background.Binding, t *bt.Task, n *harness.TaskTreeNode) error {
	// Read only public descriptors from already validated host manifests. Never
	// project native checkpoint addresses or permission argument values.
	store := &BackgroundInteractionStore{store: s}
	if state, ok, err := store.taskWaitInteraction(ctx, tx, b, t); err != nil {
		return err
	} else if ok {
		n.Waiting = &harness.TaskTreeWaiting{Kind: state.Kind, TaskIDs: state.WaitingTasks, IntentCount: len(state.WaitingInputs), Resumable: state.Resumable}
		return nil
	}
	manifest, _, err := store.readManifest(ctx, tx, b)
	if errors.Is(err, harness.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = store.validateManifest(ctx, tx, manifest, t); err != nil {
		return err
	}
	visible, cancelled, err := backgroundActiveManifest(ctx, tx, manifest)
	if err != nil {
		return err
	}
	kind := "permission"
	if len(cancelled) > 0 && len(visible.Interrupts) == 0 {
		kind = "cancelled_roles"
	}
	n.Waiting = &harness.TaskTreeWaiting{Kind: kind, IntentCount: len(visible.Interrupts), Resumable: kind == "cancelled_roles"}
	return nil
}

func (s *Store) treeBackgroundExecutionReference(ctx context.Context, tx *sql.Tx, n *harness.TaskTreeNode) error {
	var bindingData, taskData []byte
	err := tx.QueryRowContext(ctx, `SELECT b.payload,t.payload FROM harness_background_intent_roles r JOIN harness_background_permission_intents i ON i.id=r.intent_id JOIN harness_background_bindings b ON b.task_id=i.task_id JOIN eino_background_tasks t ON t.id=b.task_id WHERE r.root_id=? AND r.invocation_id=? AND i.state='pending' AND t.status='waiting_input' ORDER BY b.rowid DESC LIMIT 1`, n.RootID, n.InvocationID).Scan(&bindingData, &taskData)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var b background.Binding
	var task bt.Task
	if json.Unmarshal(bindingData, &b) != nil || json.Unmarshal(taskData, &task) != nil || b.RootBudgetID != n.RootID || b.RootSession() != n.RootSessionID || b.TaskID != task.Spec.ID {
		return harness.ErrExecutionConflict
	}
	store := &BackgroundInteractionStore{store: s}
	m, state, err := store.readManifest(ctx, tx, b)
	if err != nil {
		return err
	}
	if state != "waiting" || m.TaskVersion != task.Version {
		return harness.ErrExecutionConflict
	}
	matched := false
	for _, item := range m.Interrupts {
		if item.RoleID == n.InvocationID {
			matched = true
			break
		}
	}
	if !matched {
		return nil
	}
	visible, stopped, err := backgroundActiveManifest(ctx, tx, m)
	if err != nil {
		return err
	}
	ref := &harness.TaskTreeBackgroundExecution{TaskID: b.TaskID, Version: task.Version, Status: string(task.Status), InteractionID: visible.ID}
	ref.Resumable = len(visible.Interrupts) == 0 && len(stopped) > 0 && store.validateManifest(ctx, tx, m, &task) == nil
	n.BackgroundExecution = ref
	return nil
}
