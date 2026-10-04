package background

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/cloudwego/eino/adk"
	bt "github.com/cloudwego/eino/adk/backgroundtask"
	ds "github.com/cloudwego/eino/adk/backgroundtask/subagent"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type continuationSubmissionKey struct{}
type continuationSubmission struct {
	actor harness.TaskActor
	check func(context.Context, *sql.Tx, Binding) error
}

// ContinuationTaskID is a deterministic host identity, not permission to submit.
func ContinuationTaskID(b Binding) string { return originTaskID(b) }

// SubmitContinuation requires a current root attachment and an admission
// validator which runs in the native creation transaction. The host validator
// proves the saved notification, role, parent terminal state and quota. No
// completed worker lease or model-supplied binding can stand in for that proof.
func (s *Service) SubmitContinuation(ctx context.Context, actor harness.TaskActor, b Binding, input *adk.AgentInput, description string, check func(context.Context, *sql.Tx, Binding) error) (harness.BackgroundTask, error) {
	if b.ContinuationOf == "" || b.InvocationID == "" || b.RootSession() != actor.SessionID || check == nil || s.config.Attempts == nil {
		return harness.BackgroundTask{}, harness.ErrPermissionDenied
	}
	if _, worker := ScopeFromContext(ctx); worker {
		return harness.BackgroundTask{}, harness.ErrPermissionDenied
	}
	if err := s.authorize(ctx, actor); err != nil {
		return harness.BackgroundTask{}, err
	}
	finish, err := s.beginSubmission()
	if err != nil {
		return harness.BackgroundTask{}, err
	}
	defer finish()
	if s.config.Budgets == nil {
		return harness.BackgroundTask{}, harness.ErrBackgroundUnavailable
	}
	b.TaskID, b.ExecutorKey = originTaskID(b), ds.ExecutorKey
	if b.ChildSessionID == "" {
		b.ChildSessionID = b.TaskID + "/session"
	}
	if b.ChildSessionID != b.TaskID+"/session" {
		return harness.BackgroundTask{}, harness.ErrPermissionDenied
	}
	data, err := json.Marshal(struct {
		Input       *adk.AgentInput
		Description string
	}{input, description})
	if err != nil {
		return harness.BackgroundTask{}, err
	}
	b.RequestHash = fmt.Sprintf("%x", sha256.Sum256(data))
	// The root is the authenticated caller; the saved parent session is used
	// only for native routing and hashing after the transactional validator.
	nativeActor := harness.TaskActor{OwnerID: actor.OwnerID, SessionID: b.ParentSessionID}
	if old, _, err := loadBinding(ctx, s.store.DB(), b.TaskID); err == nil {
		if old.RequestHash != b.RequestHash || old.ContinuationOf != b.ContinuationOf || old.InvocationID != b.InvocationID || old.ExecutionContract != b.ExecutionContract || old.RootSession() != actor.SessionID {
			return harness.BackgroundTask{}, harness.ErrTaskOriginConflict
		}
		return s.Get(ctx, actor, b.TaskID)
	} else if !errors.Is(err, harness.ErrNotFound) {
		return harness.BackgroundTask{}, err
	}
	ctx = context.WithValue(ctx, continuationSubmissionKey{}, &continuationSubmission{actor: actor, check: check})
	ctx = context.WithValue(ctx, nativeContextKey{}, &nativeSubmission{actor: nativeActor, binding: b})
	task, err := ds.Submit(ctx, s.manager, &ds.SubmitRequest[*schema.Message]{TaskID: b.TaskID, SubAgentName: b.AgentVersion, Input: input, Description: description, SessionID: b.ParentSessionID, ChildSessionID: b.ChildSessionID})
	if errors.Is(err, bt.ErrAlreadyExists) {
		old, _, loadErr := loadBinding(ctx, s.store.DB(), b.TaskID)
		if loadErr != nil || old.RequestHash != b.RequestHash || old.ExecutionContract != b.ExecutionContract {
			return harness.BackgroundTask{}, errors.Join(harness.ErrTaskOriginConflict, loadErr)
		}
		return s.Get(ctx, actor, b.TaskID)
	}
	if task == nil {
		return harness.BackgroundTask{}, err
	}
	s.wake()
	accepted, getErr := s.Get(ctx, actor, task.Spec.ID)
	return accepted, errors.Join(err, getErr)
}
