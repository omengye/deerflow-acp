package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/assets"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

type BackgroundInteractionFence func(context.Context, *sql.Tx, background.TaskScope, bool) error

// BackgroundInteractionStore owns native task permission data, independently
// of foreground executions. Authorizer must check process-local ownership and
// must not open this database's connection pool from CommitResumeTx.
type BackgroundInteractionStore struct {
	store      *Store
	authorizer background.Authorizer
	Assets     *assets.Store // configure before sharing with workers
}

func NewBackgroundInteractionStore(ctx context.Context, store *Store, authorizer background.Authorizer) (*BackgroundInteractionStore, error) {
	if store == nil || authorizer == nil {
		return nil, harness.ErrBackgroundUnavailable
	}
	_, err := store.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS harness_background_permission_intents (
 id TEXT PRIMARY KEY,task_id TEXT NOT NULL REFERENCES harness_background_specs(task_id),tool_call_id TEXT NOT NULL,version INTEGER NOT NULL,state TEXT NOT NULL,request BLOB NOT NULL,descriptor BLOB NOT NULL,
 UNIQUE(task_id,tool_call_id),FOREIGN KEY(task_id,tool_call_id) REFERENCES harness_tool_receipts(run_id,tool_call_id));
CREATE TABLE IF NOT EXISTS harness_background_interrupt_stages (
 task_id TEXT NOT NULL REFERENCES harness_background_specs(task_id),attempt INTEGER NOT NULL,bindings BLOB NOT NULL,PRIMARY KEY(task_id,attempt));
CREATE TABLE IF NOT EXISTS harness_background_permission_manifests (
 task_id TEXT PRIMARY KEY REFERENCES harness_background_specs(task_id),id TEXT NOT NULL UNIQUE,state TEXT NOT NULL,payload BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS harness_background_permission_grants (
 id TEXT PRIMARY KEY,task_id TEXT NOT NULL REFERENCES harness_background_specs(task_id),attempt INTEGER NOT NULL,intent_id TEXT NOT NULL REFERENCES harness_background_permission_intents(id),intent_version INTEGER NOT NULL,
 decision TEXT NOT NULL,state TEXT NOT NULL,approved_by TEXT NOT NULL,task_version INTEGER NOT NULL,UNIQUE(task_id,attempt,intent_id));
CREATE TABLE IF NOT EXISTS harness_background_policy_grants (
 grant_id TEXT PRIMARY KEY REFERENCES harness_background_permission_grants(id),policy_sha TEXT NOT NULL,config_version INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS harness_background_drain_manifests (
 task_id TEXT PRIMARY KEY REFERENCES harness_background_specs(task_id),state TEXT NOT NULL,payload BLOB NOT NULL);
`)
	if err != nil {
		return nil, err
	}
	return &BackgroundInteractionStore{store: store, authorizer: authorizer}, nil
}

type backgroundPermissionManifest struct {
	Version                                                            int
	ID                                                                 string
	Binding                                                            background.Binding
	Attempt, TaskVersion                                               int64
	CheckpointSHA, NativeTaskSHA, ConfigSHA, InputSHA, ReceiptFrontier string
	NativeHead                                                         ExecutionNativeHead
	EventCursor                                                        int64
	Interrupts                                                         []interaction.ExecutionInterruptBinding
}

func readBackgroundNativeTask(ctx context.Context, tx *sql.Tx, id string) (*bt.Task, error) {
	var task bt.Task
	var data []byte
	var status string
	var version int64
	err := tx.QueryRowContext(ctx, `SELECT status,version,payload FROM eino_background_tasks WHERE id=?`, id).Scan(&status, &version, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, harness.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(data, &task); err != nil {
		return nil, err
	}
	if task.Spec.ID != id || task.Version != version || string(task.Status) != status {
		return nil, harness.ErrTaskOriginConflict
	}
	return &task, nil
}

func (s *BackgroundInteractionStore) authorize(ctx context.Context, actor harness.TaskActor, binding background.Binding) error {
	if actor.SessionID != binding.ParentSessionID || actor.OwnerID == "" {
		return harness.ErrPermissionDenied
	}
	return s.authorizer.AuthorizeTaskAccess(ctx, actor)
}

func (s *BackgroundInteractionStore) bindingTx(ctx context.Context, tx *sql.Tx, binding background.Binding, policy bool) (harness.RunRequest, error) {
	host := &BackgroundExecutionStore{store: s.store}
	if _, err := host.backgroundRunTx(ctx, tx, background.TaskScope{Binding: binding, Attempt: 1}); err != nil {
		return harness.RunRequest{}, err
	}
	var encoded, input []byte
	if err := tx.QueryRowContext(ctx, `SELECT payload FROM harness_background_specs WHERE task_id=?`, binding.TaskID).Scan(&encoded); err != nil {
		return harness.RunRequest{}, err
	}
	var spec BackgroundExecutionSpec
	if err := json.Unmarshal(encoded, &spec); err != nil {
		return harness.RunRequest{}, err
	}
	if _, err := validateBackgroundSpec(binding, spec); err != nil {
		return harness.RunRequest{}, err
	}
	if policy {
		if err := host.CheckParentPolicyTx(ctx, tx, binding, spec); err != nil {
			return harness.RunRequest{}, err
		}
	}
	child, err := readExecutionSession(ctx, tx, binding.ChildSessionID)
	if err != nil {
		return harness.RunRequest{}, err
	}
	if child.CWD != spec.Parent.CWD || child.Model != spec.Parent.Model || child.Mode != spec.Parent.Mode || child.ApprovalMode != spec.Parent.ApprovalMode || child.ConfigVersion != spec.Parent.ConfigVersion || child.Subagents {
		return harness.RunRequest{}, harness.ErrTaskOriginConflict
	}
	if err = tx.QueryRowContext(ctx, `SELECT content FROM harness_inputs WHERE id=? AND run_id=?`, backgroundInputID(binding), binding.TaskID).Scan(&input); err != nil {
		return harness.RunRequest{}, err
	}
	wanted, err := json.Marshal(spec.Input)
	if err != nil {
		return harness.RunRequest{}, err
	}
	if executionDigest(input) != executionDigest(wanted) {
		return harness.RunRequest{}, harness.ErrTaskOriginConflict
	}
	return harness.RunRequest{Session: child, RunID: binding.TaskID, InputID: backgroundInputID(binding), RootBudgetID: binding.RootBudgetID, Input: spec.Input}, nil
}

func (s *BackgroundInteractionStore) readManifest(ctx context.Context, tx *sql.Tx, binding background.Binding) (backgroundPermissionManifest, string, error) {
	var m backgroundPermissionManifest
	var data []byte
	var id, state string
	err := tx.QueryRowContext(ctx, `SELECT id,state,payload FROM harness_background_permission_manifests WHERE task_id=?`, binding.TaskID).Scan(&id, &state, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return m, state, harness.ErrNotFound
	}
	if err != nil {
		return m, state, err
	}
	if err = json.Unmarshal(data, &m); err != nil {
		return m, state, err
	}
	if m.Version != 1 || m.ID != id || m.Binding != binding || m.Attempt < 1 || m.TaskVersion < 1 {
		return m, state, harness.ErrTaskOriginConflict
	}
	return m, state, nil
}

func readBackgroundIntent(ctx context.Context, tx *sql.Tx, taskID, intentID string) (harness.PermissionRequest, int64, string, error) {
	var p harness.PermissionRequest
	var version int64
	var state string
	var encoded []byte
	err := tx.QueryRowContext(ctx, `SELECT request,version,state FROM harness_background_permission_intents WHERE task_id=? AND id=?`, taskID, intentID).Scan(&encoded, &version, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return p, 0, "", harness.ErrExecutionConflict
	}
	if err != nil {
		return p, 0, "", err
	}
	err = decodeExecutionPermission(encoded, &p)
	return p, version, state, err
}

func checkBackgroundBindings(ctx context.Context, tx *sql.Tx, binding background.Binding, items []interaction.ExecutionInterruptBinding) error {
	return checkBackgroundBindingItems(ctx, tx, binding, items, true)
}
func checkBackgroundBindingItems(ctx context.Context, tx *sql.Tx, binding background.Binding, items []interaction.ExecutionInterruptBinding, complete bool) error {
	if len(items) < 1 || len(items) > 128 {
		return executionInvalid("background wait requires 1 to 128 native targets")
	}
	intents, targets := map[string]bool{}, map[string]bool{}
	for _, b := range items {
		if b.IntentID == "" || b.IntentVersion < 1 || b.NativeInterruptID == "" || len(b.NativeInterruptID) > 4096 || intents[b.IntentID] || targets[b.NativeInterruptID] {
			return executionInvalid("invalid background interrupt binding")
		}
		intents[b.IntentID], targets[b.NativeInterruptID] = true, true
		p, version, state, err := readBackgroundIntent(ctx, tx, binding.TaskID, b.IntentID)
		if err != nil {
			return err
		}
		if version != b.IntentVersion || state != "pending" || p.SessionID != binding.ChildSessionID || p.RunID != binding.TaskID {
			return harness.ErrExecutionConflict
		}
		receipt, err := readReceipt(ctx, tx, p.SessionID, p.RunID, p.ToolCallID)
		if err != nil {
			return err
		}
		if receipt.State != harness.ReceiptPending || receipt.ArgumentsDigest != executionDigest(p.Arguments) || receipt.ToolName != p.ToolName || receipt.ConfigVersion != p.ConfigVersion {
			return harness.ErrReceiptConflict
		}
	}
	if !complete {
		return nil
	}
	var pending, receipts int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM harness_background_permission_intents WHERE task_id=? AND state='pending'`, binding.TaskID).Scan(&pending); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM harness_tool_receipts WHERE run_id=? AND state='pending'`, binding.TaskID).Scan(&receipts); err != nil {
		return err
	}
	if pending != len(items) || receipts != len(items) {
		return fmt.Errorf("%w: background checkpoint omits pending tool calls", harness.ErrExecutionConflict)
	}
	return nil
}

func (s *BackgroundInteractionStore) validateManifest(ctx context.Context, tx *sql.Tx, m backgroundPermissionManifest, task *bt.Task) error {
	if err := s.validateFrontier(ctx, tx, m, task); err != nil {
		return err
	}
	return checkBackgroundBindings(ctx, tx, m.Binding, m.Interrupts)
}

func (s *BackgroundInteractionStore) validateFrontier(ctx context.Context, tx *sql.Tx, m backgroundPermissionManifest, task *bt.Task) error {
	req, err := s.bindingTx(ctx, tx, m.Binding, true)
	if err != nil {
		return err
	}
	input, _ := json.Marshal(req.Input)
	if m.ConfigSHA != executionConfigDigest(req.Session) || m.InputSHA != executionDigest(input) || executionDigest(task.Checkpoint) != m.NativeTaskSHA {
		return harness.ErrExecutionUnresumable
	}
	var checkpoint []byte
	if err = tx.QueryRowContext(ctx, `SELECT payload FROM eino_checkpoints WHERE id=?`, m.Binding.TaskID+"/checkpoint").Scan(&checkpoint); errors.Is(err, sql.ErrNoRows) {
		return harness.ErrExecutionUnresumable
	} else if err != nil {
		return err
	}
	if len(checkpoint) == 0 || executionDigest(checkpoint) != m.CheckpointSHA {
		return harness.ErrExecutionUnresumable
	}
	head, err := executionNativeHead(ctx, tx, m.Binding.ChildSessionID)
	if err != nil {
		return err
	}
	if head != m.NativeHead {
		return harness.ErrExecutionUnresumable
	}
	cursor, err := executionEventCursor(ctx, tx, m.Binding.ChildSessionID)
	if err != nil {
		return err
	}
	if cursor != m.EventCursor {
		return harness.ErrExecutionUnresumable
	}
	frontier, err := executionReceiptFrontier(ctx, tx, m.Binding.ChildSessionID, m.Binding.TaskID)
	if err != nil {
		return err
	}
	if frontier != m.ReceiptFrontier {
		return harness.ErrExecutionUnresumable
	}
	return nil
}

func (s *BackgroundInteractionStore) Interaction(ctx context.Context, actor harness.TaskActor, binding background.Binding) (*harness.BackgroundInteraction, error) {
	if err := s.authorize(ctx, actor, binding); err != nil {
		return nil, err
	}
	var result *harness.BackgroundInteraction
	err := withExecutionTransaction(ctx, s.store, func(tx *sql.Tx) error {
		task, err := readBackgroundNativeTask(ctx, tx, binding.TaskID)
		if err != nil {
			return err
		}
		if task.Status != bt.StatusWaitingInput {
			return nil
		}
		m, state, err := s.readManifest(ctx, tx, binding)
		if err != nil {
			return err
		}
		if state != "waiting" || m.TaskVersion != task.Version {
			return harness.ErrExecutionConflict
		}
		result = &harness.BackgroundInteraction{ID: m.ID, TaskVersion: task.Version, WaitingInputs: []harness.ExecutionInteraction{}}
		for _, b := range m.Interrupts {
			var data []byte
			if err = tx.QueryRowContext(ctx, `SELECT descriptor FROM harness_background_permission_intents WHERE id=? AND task_id=?`, b.IntentID, binding.TaskID).Scan(&data); err != nil {
				return err
			}
			var descriptor harness.ExecutionInteraction
			if err = json.Unmarshal(data, &descriptor); err != nil {
				return err
			}
			result.WaitingInputs = append(result.WaitingInputs, descriptor)
		}
		if err = s.validateManifest(ctx, tx, m, task); err != nil {
			if errors.Is(err, harness.ErrExecutionUnresumable) || errors.Is(err, harness.ErrExecutionConflict) || errors.Is(err, harness.ErrReconciliationRequired) || errors.Is(err, harness.ErrPermissionDenied) || errors.Is(err, harness.ErrReceiptConflict) {
				result.BlockedReason = err.Error()
				return nil
			}
			return err
		}
		result.Resumable = true
		return nil
	})
	return result, err
}
