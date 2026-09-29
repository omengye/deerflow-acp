package background

import (
	"context"
	"errors"
	"fmt"

	"github.com/cloudwego/eino/adk"
	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type fencedSessionStore struct {
	service *Service
	scope   TaskScope
	write   bool
}

func (s *Service) childStore(ctx context.Context, task *bt.Task) (adk.SessionEventStore[*schema.Message], error) {
	b, _, err := loadBinding(ctx, s.store.DB(), task.Spec.ID)
	if err != nil {
		return nil, err
	}
	scope, write := ScopeFromContext(ctx)
	if write && (scope.Binding.TaskID != task.Spec.ID || scope.Attempt != task.Attempt) {
		return nil, bt.ErrLeaseLost
	}
	return &fencedSessionStore{service: s, scope: TaskScope{Binding: b, Attempt: task.Attempt}, write: write}, nil
}

func (f *fencedSessionStore) AppendEvents(ctx context.Context, id string, events []*adk.SessionEvent[*schema.Message]) error {
	if !f.write || id != f.scope.Binding.ChildSessionID {
		return harness.ErrPermissionDenied
	}
	tx, err := f.service.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = f.service.checkChildTx(ctx, tx, f.scope, true); err != nil {
		return err
	}
	f.service.mu.Lock()
	state := f.service.attempts[attemptKey(f.scope)]
	f.service.mu.Unlock()
	if state == nil {
		return bt.ErrLeaseLost
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.ready {
		return bt.ErrLeaseLost
	}
	if err = f.service.store.AppendEventsTx(ctx, tx, id, events); err != nil {
		return err
	}
	return tx.Commit()
}
func (f *fencedSessionStore) LoadEvents(ctx context.Context, id string, req *adk.LoadSessionEventsRequest) (*adk.LoadSessionEventsResult[*schema.Message], error) {
	if id != f.scope.Binding.ChildSessionID {
		return nil, harness.ErrPermissionDenied
	}
	return f.service.store.LoadEvents(ctx, id, req)
}

// SessionStoreForAttempt lets explicit executors use the same fenced provider.
// Read-only progress access is internal; public callers use authorized APIs.
func (s *Service) SessionStoreForAttempt(ctx context.Context) (adk.SessionEventStore[*schema.Message], error) {
	scope, ok := ScopeFromContext(ctx)
	if !ok {
		return nil, harness.ErrPermissionDenied
	}
	return &fencedSessionStore{service: s, scope: scope, write: true}, nil
}

// CheckpointsForAttempt returns the native runner store. Writes are staged;
// lifecycle hooks persist them only after cleanup in the final task transaction.
func (s *Service) CheckpointsForAttempt() adk.CheckPointStore {
	return checkpointDispatcher{service: s}
}

type checkpointDispatcher struct{ service *Service }

func (c checkpointDispatcher) authorized(ctx context.Context, key string) (*attemptState, error) {
	v, ok := ctx.Value(attemptContextKey{}).(*attemptContext)
	if !ok || v == nil || v.state == nil {
		return nil, harness.ErrPermissionDenied
	}
	if key != v.state.scope.Binding.TaskID+"/checkpoint" {
		return nil, harness.ErrPermissionDenied
	}
	return v.state, nil
}
func (c checkpointDispatcher) Get(ctx context.Context, key string) ([]byte, bool, error) {
	state, err := c.authorized(ctx, key)
	if err != nil {
		return nil, false, err
	}
	if err = c.service.CheckEffect(ctx); err != nil {
		return nil, false, err
	}
	state.mu.Lock()
	write, ok := state.writes[key]
	state.mu.Unlock()
	if ok {
		if write.Delete {
			return nil, false, nil
		}
		return append([]byte(nil), write.Data...), true, nil
	}
	return c.service.store.Get(ctx, key)
}
func (c checkpointDispatcher) Set(ctx context.Context, key string, data []byte) error {
	return c.stage(ctx, key, checkpointWrite{Data: append([]byte(nil), data...)})
}
func (c checkpointDispatcher) Delete(ctx context.Context, key string) error {
	return c.stage(ctx, key, checkpointWrite{Delete: true})
}
func (c checkpointDispatcher) stage(ctx context.Context, key string, write checkpointWrite) error {
	state, err := c.authorized(ctx, key)
	if err != nil {
		return err
	}
	if len(write.Data) > c.service.config.MaxCheckpointBytes {
		return fmt.Errorf("background: checkpoint exceeds %d bytes", c.service.config.MaxCheckpointBytes)
	}
	tx, err := c.service.store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = c.service.checkChildTx(ctx, tx, state.scope, true); err != nil {
		return err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.ready {
		return errors.New("background: checkpoint write after attempt cleanup")
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	state.writes[key] = write
	return nil
}
