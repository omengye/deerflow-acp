package runtime

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

type backgroundIntentAnswer struct {
	IntentID string                     `json:"intentId"`
	Version  int64                      `json:"version"`
	Decision harness.PermissionDecision `json:"decision"`
}

func decodeStrictBackgroundJSON(data []byte, out any) error {
	if len(data) == 0 || len(data) > 128*1024 {
		return harness.ErrInvalidInput
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return harness.ErrInvalidInput
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF {
		return harness.ErrInvalidInput
	}
	return nil
}

func backgroundAnswers(request harness.TaskApproval, m backgroundPermissionManifest) (map[string]backgroundIntentAnswer, error) {
	if request.ID != m.ID || request.TaskVersion != m.TaskVersion {
		return nil, harness.ErrExecutionConflict
	}
	result := map[string]backgroundIntentAnswer{}
	if len(request.Evidence) > 0 {
		if request.Decision != "" {
			return nil, harness.ErrInvalidInput
		}
		var evidence struct {
			Decisions []backgroundIntentAnswer `json:"decisions"`
		}
		if err := decodeStrictBackgroundJSON(request.Evidence, &evidence); err != nil {
			return nil, err
		}
		for _, answer := range evidence.Decisions {
			if _, ok := result[answer.IntentID]; ok {
				return nil, harness.ErrInvalidInput
			}
			result[answer.IntentID] = answer
		}
	} else {
		for _, b := range m.Interrupts {
			result[b.IntentID] = backgroundIntentAnswer{IntentID: b.IntentID, Version: b.IntentVersion, Decision: request.Decision}
		}
	}
	if len(result) != len(m.Interrupts) {
		return nil, harness.ErrInvalidInput
	}
	for _, b := range m.Interrupts {
		answer, ok := result[b.IntentID]
		if !ok || answer.Version != b.IntentVersion {
			return nil, harness.ErrExecutionConflict
		}
		switch answer.Decision {
		case harness.AllowOnce, harness.RejectOnce:
		default:
			return nil, harness.ErrInvalidInput
		}
	}
	return result, nil
}

func (s *BackgroundInteractionStore) waitingApprovalTx(ctx context.Context, tx *sql.Tx, actor harness.TaskActor, binding background.Binding, request harness.TaskApproval) (backgroundPermissionManifest, map[string]backgroundIntentAnswer, error) {
	var empty backgroundPermissionManifest
	if err := s.authorize(ctx, actor, binding); err != nil {
		return empty, nil, err
	}
	task, err := readBackgroundNativeTask(ctx, tx, binding.TaskID)
	if err != nil {
		return empty, nil, err
	}
	if task.Status != bt.StatusWaitingInput || task.Version != request.TaskVersion {
		return empty, nil, bt.ErrVersionConflict
	}
	m, state, err := s.readManifest(ctx, tx, binding)
	if err != nil {
		return m, nil, err
	}
	if state != "waiting" || m.Attempt != task.Attempt || m.TaskVersion != task.Version {
		return m, nil, harness.ErrExecutionConflict
	}
	if err = s.validateManifest(ctx, tx, m, task); err != nil {
		return m, nil, err
	}
	answers, err := backgroundAnswers(request, m)
	return m, answers, err
}

// PrepareResume is read-only. It constructs native addresses exclusively from
// the persisted manifest. No grant exists until the native version CAS commits.
func (s *BackgroundInteractionStore) PrepareResume(ctx context.Context, actor harness.TaskActor, binding background.Binding, request harness.TaskApproval) (background.ResumeGrant, error) {
	var grant background.ResumeGrant
	if err := s.authorize(ctx, actor, binding); err != nil {
		return grant, err
	}
	err := withExecutionTransaction(ctx, s.store, func(tx *sql.Tx) error {
		m, _, err := s.waitingApprovalTx(ctx, tx, actor, binding, request)
		if err != nil {
			return err
		}
		targets := make(map[string]interaction.PermissionResume, len(m.Interrupts))
		for _, b := range m.Interrupts {
			targets[b.NativeInterruptID] = interaction.PermissionResume{IntentID: b.IntentID, IntentVersion: b.IntentVersion, GrantID: NewID()}
		}
		grant.Data, err = json.Marshal(targets)
		grant.Approval = request
		return err
	})
	return grant, err
}

// CommitResumeTx creates one-use grants for the next native attempt in the
// same transaction as waiting_input -> pending. It does not consume permission
// to execute a tool; that consumption belongs to the tool receipt transaction.
func (s *BackgroundInteractionStore) CommitResumeTx(ctx context.Context, tx *sql.Tx, actor harness.TaskActor, binding background.Binding, grant background.ResumeGrant) error {
	m, answers, err := s.waitingApprovalTx(ctx, tx, actor, binding, grant.Approval)
	if err != nil {
		return err
	}
	var targets map[string]interaction.PermissionResume
	if err = decodeStrictBackgroundJSON(grant.Data, &targets); err != nil {
		return err
	}
	if len(targets) != len(m.Interrupts) {
		return harness.ErrExecutionConflict
	}
	seen := map[string]bool{}
	for _, b := range m.Interrupts {
		resume, ok := targets[b.NativeInterruptID]
		if !ok || resume.IntentID != b.IntentID || resume.IntentVersion != b.IntentVersion || resume.GrantID == "" || len(resume.GrantID) > 256 || seen[resume.GrantID] {
			return harness.ErrExecutionConflict
		}
		seen[resume.GrantID] = true
		_, err = tx.ExecContext(ctx, `INSERT INTO harness_background_permission_grants(id,task_id,attempt,intent_id,intent_version,decision,state,approved_by,task_version) VALUES(?,?,?,?,?,?,'ready',?,?)`, resume.GrantID, binding.TaskID, m.Attempt+1, b.IntentID, b.IntentVersion, answers[b.IntentID].Decision, actor.OwnerID, m.TaskVersion)
		if err != nil {
			return err
		}
	}
	return executionCAS(ctx, tx, `UPDATE harness_background_permission_manifests SET state='approved' WHERE task_id=? AND id=? AND state='waiting'`, binding.TaskID, m.ID)
}

var _ background.ApprovalBroker = (*BackgroundInteractionStore)(nil)
