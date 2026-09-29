package runtime

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
)

const backgroundNotificationSource = "background_notification"

// A source is immutable after admission. Both the notification's historical
// version and the task's version at admission are retained; neither is a grant.
type continuationSource struct {
	Version                                                  int
	RunID, InputID, SessionID, NotificationID, TaskID        string
	OriginRunID, OriginInputID, OriginInputSHA, RootBudgetID string
	Binding                                                  background.Binding
	Spec                                                     BackgroundExecutionSpec
	Notification                                             bt.Notification
	Task                                                     harness.BackgroundTask
}

func migrateContinuations(ctx context.Context, db *sql.DB) error {
	// Older databases predate source markers. A marker is kept outside the
	// source table so deleting a source cannot turn a continuation into a prompt.
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(harness_executions)`)
	if err != nil {
		return err
	}
	columns := map[string]bool{}
	for rows.Next() {
		var ordinal, required, primary int
		var name, kind string
		var fallback any
		if err = rows.Scan(&ordinal, &name, &kind, &required, &fallback, &primary); err != nil {
			rows.Close()
			return err
		}
		columns[name] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, column := range []string{"source_kind", "source_sha"} {
		if !columns[column] {
			if _, err = db.ExecContext(ctx, `ALTER TABLE harness_executions ADD COLUMN `+column+` TEXT NOT NULL DEFAULT ''`); err != nil {
				return err
			}
		}
	}
	_, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS harness_continuation_sources (
run_id TEXT PRIMARY KEY REFERENCES harness_runs(id), input_id TEXT UNIQUE NOT NULL REFERENCES harness_inputs(id),
notification_id TEXT UNIQUE NOT NULL, parent_session_id TEXT NOT NULL REFERENCES harness_sessions(id), task_id TEXT NOT NULL,
origin_run_id TEXT NOT NULL REFERENCES harness_runs(id), origin_input_id TEXT NOT NULL REFERENCES harness_inputs(id),
root_budget_id TEXT NOT NULL, source_sha TEXT NOT NULL, payload BLOB NOT NULL);`)
	return err
}

func continuationConflict(message string) error {
	return fmt.Errorf("%w: continuation %s", harness.ErrExecutionUnresumable, message)
}

func readContinuationNotification(ctx context.Context, tx *sql.Tx, sessionID, id string) (bt.Notification, error) {
	var n bt.Notification
	var data []byte
	var taskID string
	err := tx.QueryRowContext(ctx, `SELECT task_id,payload FROM harness_background_inbox WHERE notification_id=? AND parent_session_id=?`, id, sessionID).Scan(&taskID, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return n, harness.ErrNotFound
	}
	if err != nil {
		return n, err
	}
	if json.Unmarshal(data, &n) != nil || n.ID != id || n.SessionID != sessionID || n.TaskID != taskID || n.Version < 1 || n.Kind == "" {
		return n, continuationConflict("notification identity changed")
	}
	return n, nil
}

// All queries stay in the admission/resume transaction, including both copies
// of the original task binding and the original accepted input and budget root.
func readContinuationOrigin(ctx context.Context, tx *sql.Tx, sessionID, taskID string) (background.Binding, BackgroundExecutionSpec, string, string, error) {
	var b, saved background.Binding
	var spec BackgroundExecutionSpec
	var data, savedBinding, specData, originInput []byte
	var parent, child, origin, call, intent, blocked, specChild, specInput, contract string
	err := tx.QueryRowContext(ctx, `SELECT parent_session_id,child_session_id,origin_run_id,origin_tool_call_id,intent_hash,payload,blocked_reason FROM harness_background_bindings WHERE task_id=?`, taskID).Scan(&parent, &child, &origin, &call, &intent, &data, &blocked)
	if errors.Is(err, sql.ErrNoRows) {
		err = harness.ErrNotFound
	}
	if err != nil {
		return b, spec, "", "", err
	}
	if json.Unmarshal(data, &b) != nil || b.TaskID != taskID || b.ParentSessionID != sessionID || parent != b.ParentSessionID || child != b.ChildSessionID || origin != b.OriginRunID || call != b.OriginToolCallID || intent != b.IntentHash {
		return b, spec, "", "", continuationConflict("task binding changed")
	}
	if blocked != "" {
		return b, spec, "", "", harness.ErrReconciliationRequired
	}
	err = tx.QueryRowContext(ctx, `SELECT child_session_id,input_id,contract,payload,binding FROM harness_background_specs WHERE task_id=?`, taskID).Scan(&specChild, &specInput, &contract, &specData, &savedBinding)
	if err != nil {
		return b, spec, "", "", err
	}
	if json.Unmarshal(specData, &spec) != nil || json.Unmarshal(savedBinding, &saved) != nil || saved != b || specChild != child || specInput != backgroundInputID(b) || contract != b.ExecutionContract {
		return b, spec, "", "", continuationConflict("execution specification changed")
	}
	if _, err = validateBackgroundSpec(b, spec); err != nil {
		return b, spec, "", "", continuationConflict("execution policy changed")
	}
	var originSession, originInputID, root, memberSession, taskRoot, taskSession string
	err = tx.QueryRowContext(ctx, `SELECT r.session_id,r.input_id,i.content,m.root_id,m.session_id FROM harness_runs r JOIN harness_inputs i ON i.id=r.input_id AND i.run_id=r.id JOIN budget_members m ON m.id=r.id WHERE r.id=?`, b.OriginRunID).Scan(&originSession, &originInputID, &originInput, &root, &memberSession)
	if err != nil {
		return b, spec, "", "", err
	}
	err = tx.QueryRowContext(ctx, `SELECT root_id,session_id FROM budget_members WHERE id=?`, taskID).Scan(&taskRoot, &taskSession)
	if err != nil {
		return b, spec, "", "", err
	}
	if root != b.RootBudgetID || taskRoot != root || memberSession != sessionID || originSession != sessionID || taskSession != child {
		return b, spec, "", "", continuationConflict("original budget binding changed")
	}
	return b, spec, originInputID, executionDigest(originInput), nil
}

func continuationInput(source continuationSource) ([]harness.Content, error) {
	readable := func(data []byte) any {
		if len(data) == 0 {
			return nil
		}
		if json.Valid(data) {
			return json.RawMessage(data)
		}
		return string(data)
	}
	// Only notification and result data reach the model. The original user
	// prompt, task instruction, historical owner and policy remain host data.
	data, err := json.Marshal(struct {
		NotificationID      string `json:"notificationId"`
		TaskID              string `json:"taskId"`
		Kind                string `json:"kind"`
		NotificationVersion int64  `json:"notificationVersion"`
		NotificationData    any    `json:"notificationData,omitempty"`
		TaskVersion         int64  `json:"taskVersion"`
		TaskStatus          string `json:"taskStatus"`
		Result              any    `json:"result,omitempty"`
		Error               string `json:"error,omitempty"`
	}{source.NotificationID, source.TaskID, string(source.Notification.Kind), source.Notification.Version, readable(source.Notification.Data), source.Task.Version, source.Task.Status, readable(source.Task.Result), source.Task.Error})
	if err != nil {
		return nil, err
	}
	return []harness.Content{{Type: "text", Text: string(data)}}, nil
}

func loadContinuationSource(ctx context.Context, tx *sql.Tx, row executionRow) (*continuationSource, error) {
	var data []byte
	var inputID, notificationID, parent, taskID, origin, originInput, root, digest string
	err := tx.QueryRowContext(ctx, `SELECT input_id,notification_id,parent_session_id,task_id,origin_run_id,origin_input_id,root_budget_id,source_sha,payload FROM harness_continuation_sources WHERE run_id=?`, row.State.RunID).Scan(&inputID, &notificationID, &parent, &taskID, &origin, &originInput, &root, &digest, &data)
	if row.SourceKind == "" && row.SourceSHA == "" && errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, continuationConflict("source is missing")
	}
	if err != nil {
		return nil, err
	}
	var source continuationSource
	if row.SourceKind != backgroundNotificationSource || digest != row.SourceSHA || executionDigest(data) != digest || json.Unmarshal(data, &source) != nil || source.Version != 1 || source.RunID != row.State.RunID || source.InputID != inputID || inputID != row.State.InputID || source.SessionID != parent || parent != row.State.SessionID || source.NotificationID != notificationID || source.TaskID != taskID || source.OriginRunID != origin || source.OriginInputID != originInput || source.RootBudgetID != root || root != row.RootBudgetID {
		return nil, continuationConflict("source identity or digest changed")
	}
	if source.Task.ID != taskID || source.Task.SessionID != parent || source.Task.ChildSessionID != source.Binding.ChildSessionID || source.Task.Version < source.Notification.Version || source.Notification.ID != notificationID || source.Notification.TaskID != taskID || source.Notification.SessionID != parent || source.OriginRunID != source.Binding.OriginRunID {
		return nil, continuationConflict("snapshot identity changed")
	}
	input, err := continuationInput(source)
	if err != nil {
		return nil, err
	}
	encoded, _ := json.Marshal(input)
	if executionDigest(encoded) != row.InputDigest || executionConfigDigest(source.Spec.Parent) != executionConfigDigest(row.Config) {
		return nil, continuationConflict("input or policy changed")
	}
	b, spec, originalInputID, originalInputSHA, err := readContinuationOrigin(ctx, tx, parent, taskID)
	if err != nil {
		return nil, err
	}
	savedSpec, _ := json.Marshal(source.Spec)
	currentSpec, _ := json.Marshal(spec)
	if b != source.Binding || !bytes.Equal(savedSpec, currentSpec) || originalInputID != originInput || originalInputSHA != source.OriginInputSHA {
		return nil, continuationConflict("original task source changed")
	}
	n, err := readContinuationNotification(ctx, tx, parent, notificationID)
	if err != nil {
		return nil, err
	}
	now, _ := json.Marshal(n)
	saved, _ := json.Marshal(source.Notification)
	if !bytes.Equal(now, saved) {
		return nil, continuationConflict("notification snapshot changed")
	}
	var memberRoot, memberSession, memberOrigin, memberParent, kind string
	err = tx.QueryRowContext(ctx, `SELECT root_id,session_id,origin_id,parent_id,kind FROM budget_members WHERE id=?`, row.State.RunID).Scan(&memberRoot, &memberSession, &memberOrigin, &memberParent, &kind)
	if err != nil {
		return nil, err
	}
	if memberRoot != root || memberSession != parent || memberOrigin != origin || memberParent != origin || kind != "continuation" {
		return nil, continuationConflict("budget member changed")
	}
	return &source, nil
}

func (s *Service) executionInputSourceTx(ctx context.Context, tx *sql.Tx, row executionRow) (*interaction.ExecutionInputSource, error) {
	source, err := loadContinuationSource(ctx, tx, row)
	if err != nil || source == nil {
		return nil, err
	}
	if s.ContinuationHostPolicy == "" || source.Spec.HostPolicy != s.ContinuationHostPolicy {
		return nil, continuationConflict("host policy changed")
	}
	input, err := continuationInput(*source)
	if err != nil {
		return nil, err
	}
	return &interaction.ExecutionInputSource{Kind: backgroundNotificationSource, SHA: row.SourceSHA, Input: input, PinnedExtension: append(json.RawMessage(nil), source.Spec.Extension...)}, nil
}

// beginContinuationTx never creates a root or reuses the original run/input.
// Returning an existing execution is deliberately separate from driving it.
func (s *Service) beginContinuationTx(ctx context.Context, tx *sql.Tx, owner, sessionID, notificationID string) (req harness.RunRequest, lease ExecutionLease, existing string, err error) {
	err = tx.QueryRowContext(ctx, `SELECT run_id FROM harness_continuation_sources WHERE notification_id=? AND parent_session_id=?`, notificationID, sessionID).Scan(&existing)
	if err == nil {
		row, e := readExecution(ctx, tx, sessionID, existing)
		if e != nil {
			return req, lease, existing, e
		}
		if e = validateExecutionConfig(ctx, tx, row); e != nil {
			return req, lease, existing, e
		}
		_, e = s.executionInputSourceTx(ctx, tx, row)
		return req, lease, existing, e
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return
	}
	err = nil
	n, e := readContinuationNotification(ctx, tx, sessionID, notificationID)
	if e != nil {
		err = e
		return
	}
	b, spec, originInputID, originInputSHA, e := readContinuationOrigin(ctx, tx, sessionID, n.TaskID)
	if e != nil {
		err = e
		return
	}
	if s.ContinuationHostPolicy == "" || s.ContinuationHostPolicy != spec.HostPolicy {
		err = continuationConflict("host policy changed")
		return
	}
	if e = (&BackgroundExecutionStore{store: s.Store}).CheckParentPolicyTx(ctx, tx, b, spec); e != nil {
		err = continuationConflict("parent configuration changed")
		return
	}
	var occupied, uncertain bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_runs WHERE session_id=? AND status='running'),EXISTS(SELECT 1 FROM harness_tool_receipts WHERE session_id=? AND state='uncertain')`, sessionID, sessionID).Scan(&occupied, &uncertain)
	if err != nil {
		return
	}
	if occupied {
		err = harness.ErrBusy
		return
	}
	if uncertain {
		err = harness.ErrReconciliationRequired
		return
	}
	if err = s.Store.BudgetLedger.CheckModelAdmissionTx(ctx, tx, b.RootBudgetID); err != nil {
		return
	}
	task, e := readBackgroundNativeTask(ctx, tx, b.TaskID)
	if e != nil {
		err = e
		return
	}
	if task.Spec.SessionID != sessionID || task.Version < n.Version {
		err = continuationConflict("native task identity changed")
		return
	}
	parent, e := readExecutionSession(ctx, tx, sessionID)
	if e != nil {
		err = e
		return
	}
	actual, e := session.NormalizeWorkspace(parent.CWD)
	if e != nil {
		err = e
		return
	}
	if !session.SameWorkspace(actual, parent.CWD) {
		err = continuationConflict("workspace directory was replaced")
		return
	}
	req = harness.RunRequest{Session: parent, RunID: NewID(), InputID: NewID(), RootBudgetID: b.RootBudgetID}
	source := continuationSource{Version: 1, RunID: req.RunID, InputID: req.InputID, SessionID: sessionID, NotificationID: notificationID, TaskID: b.TaskID, OriginRunID: b.OriginRunID, OriginInputID: originInputID, OriginInputSHA: originInputSHA, RootBudgetID: b.RootBudgetID, Binding: b, Spec: spec, Notification: n,
		Task: harness.BackgroundTask{ID: b.TaskID, SessionID: sessionID, ChildSessionID: b.ChildSessionID, Description: task.Spec.Description, Status: string(task.Status), Version: task.Version, Attempt: task.Attempt, Result: append([]byte(nil), task.ResultData...), Error: task.ResultError, CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt}}
	req.Input, err = continuationInput(source)
	if err != nil {
		return
	}
	data, e := json.Marshal(source)
	if e != nil {
		err = e
		return
	}
	input, _ := json.Marshal(req.Input)
	now := timestamp()
	if _, err = tx.ExecContext(ctx, `INSERT INTO harness_runs(id,session_id,input_id,status,created_at,updated_at) VALUES(?,?,?,'running',?,?)`, req.RunID, sessionID, req.InputID, now, now); err != nil {
		return
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO harness_inputs VALUES(?,?,?)`, req.InputID, req.RunID, input); err != nil {
		return
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO harness_continuation_sources VALUES(?,?,?,?,?,?,?,?,?,?)`, req.RunID, req.InputID, notificationID, sessionID, b.TaskID, b.OriginRunID, originInputID, b.RootBudgetID, executionDigest(data), data); err != nil {
		return
	}
	err = s.Store.BudgetLedger.BindMemberTx(ctx, tx, budget.MemberBinding{RootBudgetID: b.RootBudgetID, MemberID: req.RunID, SessionID: sessionID, OriginRunID: b.OriginRunID, ParentMemberID: b.OriginRunID, Kind: "continuation"})
	if err != nil {
		return
	}
	scope := foregroundBudgetScope(req)
	if err = s.Store.BudgetLedger.BeginAttemptTx(ctx, tx, scope); err != nil {
		return
	}
	lease, err = s.Store.BeginExecutionTx(ctx, tx, req, owner, scope)
	if err != nil {
		return
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_executions SET source_kind=?,source_sha=? WHERE run_id=?`, backgroundNotificationSource, executionDigest(data), req.RunID); err != nil {
		return
	}
	if _, err = appendEventTx(ctx, tx, harness.RunEvent{SessionID: sessionID, RunID: req.RunID, Kind: "continuation_started", Content: req.Input}); err != nil {
		return
	}
	_, err = tx.ExecContext(ctx, `UPDATE harness_sessions SET updated_at=? WHERE id=?`, now, sessionID)
	return
}

func (s *Service) ProcessBackgroundNotification(ctx context.Context, owner, sessionID, notificationID string, emit harness.EventHandler, approve harness.PermissionHandler) (result harness.RunResult, runErr error) {
	defer func() {
		if cancellationOnly(runErr) {
			result.StopReason, runErr = "cancelled", nil
		}
	}()
	if notificationID == "" || len(notificationID) > 4096 || strings.ContainsRune(notificationID, 0) {
		return result, harness.ErrInvalidInput
	}
	if s.executionEngine() == nil || s.Store.BudgetLedger == nil {
		return result, executionInvalid("notification processing requires durable execution and budget ledger")
	}
	if r, ok := ctx.Value(reservationKey{}).(*reservation); ok && r.sessionID == sessionID && r.owner == owner {
		if !r.used.CompareAndSwap(false, true) {
			return result, harness.ErrBusy
		}
	} else {
		var release func()
		var err error
		ctx, release, err = s.Admit(ctx, owner, sessionID)
		if err != nil {
			return result, err
		}
		defer release()
		ctx.Value(reservationKey{}).(*reservation).used.Store(true)
	}
	if err := s.Coordinator.Authorize(sessionID, owner); err != nil {
		return result, err
	}
	var req harness.RunRequest
	var lease ExecutionLease
	var existing string
	err := withExecutionTransaction(ctx, s.Store, func(tx *sql.Tx) error {
		var err error
		req, lease, existing, err = s.beginContinuationTx(ctx, tx, owner, sessionID, notificationID)
		return err
	})
	if err != nil {
		return result, err
	}
	if existing != "" {
		state, err := s.Store.Execution(ctx, sessionID, existing)
		if err != nil {
			return result, err
		}
		var reason string
		if err = s.Store.db.QueryRowContext(ctx, `SELECT stop_reason FROM harness_runs WHERE id=?`, existing).Scan(&reason); err != nil {
			return result, err
		}
		if reason == "" || reason == "waiting_input" {
			reason = "end_turn"
		}
		return harness.RunResult{StopReason: reason, Execution: &state}, nil
	}
	if approve == nil {
		approve = func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
			return harness.PermissionCancelled, nil
		}
	}
	return s.driveExecution(ctx, req, lease, nil, emit, approve)
}
