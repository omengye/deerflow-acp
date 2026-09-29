package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

type backgroundInteractionAttemptKey struct{}

type BackgroundInteractionAttempt struct {
	store   *BackgroundInteractionStore
	scope   background.TaskScope
	fence   BackgroundInteractionFence
	targets map[string]interaction.PermissionResume
	mu      sync.Mutex
}

func (s *BackgroundInteractionStore) OpenAttempt(ctx context.Context, scope background.TaskScope, check BackgroundInteractionFence) (*BackgroundInteractionAttempt, error) {
	if check == nil || scope.Attempt < 1 {
		return nil, harness.ErrInvalidInput
	}
	a := &BackgroundInteractionAttempt{store: s, scope: scope, fence: check, targets: map[string]interaction.PermissionResume{}}
	err := withExecutionTransaction(ctx, s.store, func(tx *sql.Tx) error {
		if err := a.check(ctx, tx, false); err != nil {
			return err
		}
		task, err := readBackgroundNativeTask(ctx, tx, scope.Binding.TaskID)
		if err != nil {
			return err
		}
		if task.Status != bt.StatusRunning || task.Attempt != scope.Attempt {
			return harness.ErrExecutionConflict
		}
		if drained, err := s.validateReleasedDrain(ctx, tx, scope.Binding, task, scope.Attempt, false); drained || err != nil {
			return err
		}
		m, state, err := s.readManifest(ctx, tx, scope.Binding)
		if errors.Is(err, harness.ErrNotFound) {
			if task.Attempt != 1 || len(task.Checkpoint) != 0 || len(task.PendingResume) != 0 {
				return harness.ErrExecutionUnresumable
			}
			return nil
		}
		if err != nil {
			return err
		}
		if state != "approved" || m.Attempt+1 != scope.Attempt {
			return harness.ErrExecutionConflict
		}
		if err = s.validateManifest(ctx, tx, m, task); err != nil {
			return err
		}
		a.targets, err = s.grantTargetsTx(ctx, tx, m, scope.Attempt)
		if err != nil {
			return err
		}
		return verifyBackgroundResumeData(task.PendingResume, a.targets)
	})
	return a, err
}

func (a *BackgroundInteractionAttempt) check(ctx context.Context, tx *sql.Tx, allowStopping bool) error {
	if err := a.fence(ctx, tx, a.scope, allowStopping); err != nil {
		return err
	}
	_, err := a.store.bindingTx(ctx, tx, a.scope.Binding, !allowStopping)
	return err
}

func (a *BackgroundInteractionAttempt) Hooks() interaction.ExecutionHooks {
	targets := make(map[string]interaction.PermissionResume, len(a.targets))
	for id, p := range a.targets {
		targets[id] = p
	}
	return interaction.ExecutionHooks{Broker: a, Targets: targets, StageCheckpoint: func(context.Context, interaction.StagedExecutionCheckpoint) error {
		return errors.New("native background executor owns checkpoint publication")
	}}
}

func (a *BackgroundInteractionAttempt) PreparePermission(ctx context.Context, p harness.PermissionRequest) (interaction.PermissionIntent, error) {
	var result interaction.PermissionIntent
	if p.SessionID != a.scope.Binding.ChildSessionID || p.RunID != a.scope.Binding.TaskID || p.ConfigVersion != a.scope.Binding.ConfigVersion || p.ToolCallID == "" || p.ToolName == "" || !json.Valid(p.Arguments) || len(p.Arguments) > 128*1024 {
		return result, harness.ErrInvalidInput
	}
	err := withExecutionTransaction(ctx, a.store.store, func(tx *sql.Tx) error {
		if err := a.check(ctx, tx, false); err != nil {
			return err
		}
		receipt, err := readReceipt(ctx, tx, p.SessionID, p.RunID, p.ToolCallID)
		if err != nil {
			return err
		}
		if receipt.State != harness.ReceiptPending || receipt.ToolName != p.ToolName || receipt.ConfigVersion != p.ConfigVersion || receipt.ArgumentsDigest != executionDigest(p.Arguments) {
			return harness.ErrReceiptConflict
		}
		var oldID string
		err = tx.QueryRowContext(ctx, `SELECT id FROM harness_background_permission_intents WHERE task_id=? AND tool_call_id=?`, p.RunID, p.ToolCallID).Scan(&oldID)
		if err == nil {
			old, version, state, err := readBackgroundIntent(ctx, tx, p.RunID, oldID)
			if err != nil {
				return err
			}
			if state != "pending" || old.SessionID != p.SessionID || old.RunID != p.RunID || old.ToolName != p.ToolName || old.ConfigVersion != p.ConfigVersion || executionDigest(old.Arguments) != executionDigest(p.Arguments) || (p.ID != "" && p.ID != old.ID) {
				return harness.ErrExecutionConflict
			}
			result = interaction.PermissionIntent{ID: oldID, Version: version}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if p.ID == "" {
			p.ID = NewID()
		}
		if len(p.ID) > 256 {
			return harness.ErrInvalidInput
		}
		request, err := encodeExecutionPermission(p)
		if err != nil {
			return err
		}
		descriptor := harness.ExecutionInteraction{ID: p.ID, Kind: "permission", Version: 1, ToolCallID: p.ToolCallID, ToolName: p.ToolName, ArgumentsDigest: receipt.ArgumentsDigest, ArgumentsSummary: receipt.ArgumentsSummary, ConfigVersion: p.ConfigVersion}
		data, err := json.Marshal(descriptor)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO harness_background_permission_intents(id,task_id,tool_call_id,version,state,request,descriptor) VALUES(?,?,?,1,'pending',?,?)`, p.ID, p.RunID, p.ToolCallID, request, data)
		result = interaction.PermissionIntent{ID: p.ID, Version: 1}
		return err
	})
	return result, err
}

func (a *BackgroundInteractionAttempt) ResolvePermission(ctx context.Context, p harness.PermissionRequest, intent interaction.PermissionIntent, grantID string) (harness.PermissionDecision, error) {
	var decision harness.PermissionDecision
	err := withExecutionTransaction(ctx, a.store.store, func(tx *sql.Tx) error {
		var err error
		decision, err = a.validateGrantTx(ctx, tx, p, intent, grantID, false)
		return err
	})
	return decision, err
}

func (a *BackgroundInteractionAttempt) validateGrantTx(ctx context.Context, tx *sql.Tx, p harness.PermissionRequest, intent interaction.PermissionIntent, grantID string, allowStopping bool) (harness.PermissionDecision, error) {
	if err := a.check(ctx, tx, allowStopping); err != nil {
		return "", err
	}
	old, version, state, err := readBackgroundIntent(ctx, tx, a.scope.Binding.TaskID, intent.ID)
	if err != nil {
		return "", err
	}
	if state != "pending" || version != intent.Version || old.ID != p.ID || old.SessionID != p.SessionID || old.RunID != p.RunID || old.ToolCallID != p.ToolCallID || old.ToolName != p.ToolName || old.ConfigVersion != p.ConfigVersion || executionDigest(old.Arguments) != executionDigest(p.Arguments) {
		return "", harness.ErrExecutionConflict
	}
	if p.SessionID != a.scope.Binding.ChildSessionID || p.RunID != a.scope.Binding.TaskID {
		return "", harness.ErrExecutionConflict
	}
	var decision harness.PermissionDecision
	var grantState string
	err = tx.QueryRowContext(ctx, `SELECT decision,state FROM harness_background_permission_grants WHERE id=? AND task_id=? AND attempt=? AND intent_id=? AND intent_version=?`, grantID, p.RunID, a.scope.Attempt, intent.ID, intent.Version).Scan(&decision, &grantState)
	if errors.Is(err, sql.ErrNoRows) {
		return "", harness.ErrExecutionConflict
	}
	if err != nil {
		return "", err
	}
	if grantState != "ready" {
		return "", harness.ErrExecutionConflict
	}
	switch decision {
	case harness.AllowOnce, harness.AllowAlways, harness.RejectOnce, harness.RejectAlways:
	default:
		return "", harness.ErrExecutionConflict
	}
	receipt, err := readReceipt(ctx, tx, p.SessionID, p.RunID, p.ToolCallID)
	if err != nil {
		return "", err
	}
	if receipt.State != harness.ReceiptPending || receipt.ToolName != p.ToolName || receipt.ConfigVersion != p.ConfigVersion || receipt.ArgumentsDigest != executionDigest(p.Arguments) {
		return "", harness.ErrReceiptConflict
	}
	return decision, nil
}

// StageInterrupts records trusted native interruption addresses without making
// them resumable. The transition transaction still requires joined resources,
// the real native checkpoint, and a complete effect frontier.
func (a *BackgroundInteractionAttempt) StageInterrupts(ctx context.Context, bindings []interaction.ExecutionInterruptBinding) error {
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return withExecutionTransaction(persist, a.store.store, func(tx *sql.Tx) error {
		if err := a.check(persist, tx, true); err != nil {
			return err
		}
		if err := checkBackgroundBindingItems(persist, tx, a.scope.Binding, bindings, false); err != nil {
			return err
		}
		var old []byte
		err := tx.QueryRowContext(persist, `SELECT bindings FROM harness_background_interrupt_stages WHERE task_id=? AND attempt=?`, a.scope.Binding.TaskID, a.scope.Attempt).Scan(&old)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var previous []interaction.ExecutionInterruptBinding
		if len(old) > 0 {
			if err = json.Unmarshal(old, &previous); err != nil {
				return err
			}
		}
		merged := map[string]interaction.ExecutionInterruptBinding{}
		for _, b := range append(previous, bindings...) {
			if prior, ok := merged[b.NativeInterruptID]; ok && prior != b {
				return harness.ErrExecutionConflict
			}
			merged[b.NativeInterruptID] = b
		}
		all := make([]interaction.ExecutionInterruptBinding, 0, len(merged))
		for _, b := range merged {
			all = append(all, b)
		}
		sort.Slice(all, func(i, j int) bool { return all[i].NativeInterruptID < all[j].NativeInterruptID })
		if err = checkBackgroundBindingItems(persist, tx, a.scope.Binding, all, false); err != nil {
			return err
		}
		data, err := json.Marshal(all)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(persist, `INSERT INTO harness_background_interrupt_stages VALUES(?,?,?) ON CONFLICT(task_id,attempt) DO UPDATE SET bindings=excluded.bindings`, a.scope.Binding.TaskID, a.scope.Attempt, data)
		return err
	})
}

func (a *BackgroundInteractionAttempt) Publish(ctx context.Context, e harness.RunEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e.Kind == "tool_execute" {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	e.SessionID, e.RunID = a.scope.Binding.ChildSessionID, a.scope.Binding.TaskID
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	persist = context.WithValue(persist, backgroundInteractionAttemptKey{}, a)
	_, err := a.store.store.Append(persist, e, a.store.Assets)
	if err != nil {
		return &runPersistenceError{err: err}
	}
	return nil
}

func (a *BackgroundInteractionAttempt) consumeEventTx(ctx context.Context, tx *sql.Tx, e harness.RunEvent, receipt harness.ToolReceipt) error {
	if receipt.State != harness.ReceiptPending || (e.Kind != "tool_execute" && !(e.Kind == "tool_end" && e.Status == "failed")) {
		return nil
	}
	var grantID, intentID string
	var version int64
	var decision harness.PermissionDecision
	err := tx.QueryRowContext(ctx, `SELECT g.id,g.intent_id,g.intent_version,g.decision FROM harness_background_permission_grants g JOIN harness_background_permission_intents i ON i.id=g.intent_id WHERE g.task_id=? AND g.attempt=? AND i.tool_call_id=? AND g.state='ready'`, a.scope.Binding.TaskID, a.scope.Attempt, e.ToolCallID).Scan(&grantID, &intentID, &version, &decision)
	if errors.Is(err, sql.ErrNoRows) {
		if e.Kind == "tool_execute" {
			var pending bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_background_permission_intents WHERE task_id=? AND tool_call_id=? AND state='pending')`, a.scope.Binding.TaskID, e.ToolCallID).Scan(&pending); err != nil {
				return err
			}
			if pending {
				return harness.ErrPermissionDenied
			}
		}
		return nil
	}
	if err != nil {
		return err
	}
	if e.Kind == "tool_end" && (decision == harness.AllowOnce || decision == harness.AllowAlways) {
		return nil
	}
	p, _, _, err := readBackgroundIntent(ctx, tx, a.scope.Binding.TaskID, intentID)
	if err != nil {
		return err
	}
	decision, err = a.validateGrantTx(ctx, tx, p, interaction.PermissionIntent{ID: intentID, Version: version}, grantID, e.Kind != "tool_execute")
	if err != nil {
		return err
	}
	if e.Kind == "tool_execute" && decision != harness.AllowOnce && decision != harness.AllowAlways {
		return harness.ErrPermissionDenied
	}
	if err = executionCAS(ctx, tx, `UPDATE harness_background_permission_grants SET state='consumed' WHERE id=? AND state='ready'`, grantID); err != nil {
		return err
	}
	return executionCAS(ctx, tx, `UPDATE harness_background_permission_intents SET state='resolved',version=version+1 WHERE id=? AND version=? AND state='pending'`, intentID, version)
}
