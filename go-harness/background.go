package deerflow

import (
	"context"
	"errors"
	"time"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func backgroundAPIError(err error) error {
	switch {
	case errors.Is(err, bt.ErrNotFound):
		return errors.Join(harness.ErrNotFound, err)
	case errors.Is(err, bt.ErrVersionConflict), errors.Is(err, bt.ErrIllegalTransition), errors.Is(err, bt.ErrAlreadyTerminal):
		return errors.Join(harness.ErrExecutionConflict, err)
	default:
		return err
	}
}

func (h *backgroundHost) task(ctx context.Context, actor harness.TaskActor, id string) (harness.BackgroundTask, error) {
	for range 3 {
		task, err := h.service.Get(ctx, actor, id)
		if err != nil {
			return task, err
		}
		if task.Status != "waiting_input" {
			return task, nil
		}
		binding, err := h.service.BindingForTask(ctx, actor, id)
		if err != nil {
			return task, err
		}
		pending, err := h.interactions.Interaction(ctx, actor, binding)
		if err != nil {
			return task, err
		}
		if pending == nil || pending.TaskVersion != task.Version {
			continue
		}
		task.Interaction = pending
		return task, nil
	}
	return harness.BackgroundTask{}, harness.ErrExecutionConflict
}

func (h *backgroundHost) BackgroundTask(ctx context.Context, actor harness.TaskActor, id string) (out harness.BackgroundTask, err error) {
	defer func() { err = backgroundAPIError(err) }()
	return h.task(ctx, actor, id)
}

func (h *backgroundHost) BackgroundTasks(ctx context.Context, actor harness.TaskActor, after string, limit int) (out []harness.BackgroundTask, err error) {
	defer func() { err = backgroundAPIError(err) }()
	if limit < 0 || limit > 100 {
		return nil, harness.ErrInvalidInput
	}
	tasks, err := h.service.List(ctx, actor, after, limit)
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		if tasks[i].Status == "waiting_input" {
			tasks[i], err = h.task(ctx, actor, tasks[i].ID)
			if err != nil {
				return nil, err
			}
		}
	}
	return tasks, nil
}

func (h *backgroundHost) WaitBackgroundTask(ctx context.Context, actor harness.TaskActor, id string, after int64) (out harness.BackgroundTask, err error) {
	defer func() { err = backgroundAPIError(err) }()
	if after < 0 {
		return harness.BackgroundTask{}, harness.ErrInvalidInput
	}
	wait, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err = h.service.Wait(wait, actor, id, after)
	if err != nil && !(errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil) {
		return harness.BackgroundTask{}, err
	}
	return h.task(ctx, actor, id)
}

func (h *backgroundHost) CancelBackgroundTask(ctx context.Context, actor harness.TaskActor, id string) (out harness.BackgroundTask, err error) {
	defer func() { err = backgroundAPIError(err) }()
	return h.service.Cancel(ctx, actor, id, "cancelled by session owner")
}

func (h *backgroundHost) ResumeBackgroundTask(ctx context.Context, actor harness.TaskActor, id string, version int64) (out harness.BackgroundTask, err error) {
	defer func() { err = backgroundAPIError(err) }()
	return h.service.ReleaseSuspensionVersion(ctx, actor, id, version)
}

func (h *backgroundHost) ApproveBackgroundTask(ctx context.Context, actor harness.TaskActor, id string, request harness.TaskApproval) (out harness.BackgroundTask, err error) {
	defer func() { err = backgroundAPIError(err) }()
	return h.service.ResolveApproval(ctx, actor, id, request)
}

func (h *backgroundHost) BackgroundNotifications(ctx context.Context, actor harness.TaskActor, after int64, limit int) (out []harness.BackgroundNotification, err error) {
	defer func() { err = backgroundAPIError(err) }()
	if after < 0 || limit < 0 || limit > 100 {
		return nil, harness.ErrInvalidInput
	}
	return h.service.ListInbox(ctx, actor, after, limit)
}

func (h *backgroundHost) AcknowledgeBackgroundNotification(ctx context.Context, actor harness.TaskActor, id string) (err error) {
	defer func() { err = backgroundAPIError(err) }()
	return h.service.AcknowledgeInbox(ctx, actor, id)
}

var _ harness.BackgroundController = (*backgroundHost)(nil)
var _ harness.BackgroundTaskResumer = (*backgroundHost)(nil)

func (c *Client) backgroundOperation(sessionID string) (harness.TaskActor, func(), error) {
	done, err := c.operation()
	if err != nil {
		return harness.TaskActor{}, nil, err
	}
	if c.background == nil {
		done()
		return harness.TaskActor{}, nil, harness.ErrBackgroundUnavailable
	}
	return harness.TaskActor{OwnerID: c.owner, SessionID: sessionID}, done, nil
}

func (c *Client) BackgroundTask(ctx context.Context, sessionID, taskID string) (harness.BackgroundTask, error) {
	actor, done, err := c.backgroundOperation(sessionID)
	if err != nil {
		return harness.BackgroundTask{}, err
	}
	defer done()
	return c.background.BackgroundTask(ctx, actor, taskID)
}

// BackgroundTasks lists tasks owned by an attached parent. after is the last
// task ID returned by the previous page; limit is at most 100 (zero uses 100).
func (c *Client) BackgroundTasks(ctx context.Context, sessionID, after string, limit int) ([]harness.BackgroundTask, error) {
	actor, done, err := c.backgroundOperation(sessionID)
	if err != nil {
		return nil, err
	}
	defer done()
	return c.background.BackgroundTasks(ctx, actor, after, limit)
}

// WaitBackgroundTask waits at most ten seconds for a newer version. Timeout
// returns the current snapshot. Caller cancellation stops only this wait.
func (c *Client) WaitBackgroundTask(ctx context.Context, sessionID, taskID string, afterVersion int64) (harness.BackgroundTask, error) {
	actor, done, err := c.backgroundOperation(sessionID)
	if err != nil {
		return harness.BackgroundTask{}, err
	}
	defer done()
	return c.background.WaitBackgroundTask(ctx, actor, taskID, afterVersion)
}

func (c *Client) CancelBackgroundTask(ctx context.Context, sessionID, taskID string) (harness.BackgroundTask, error) {
	actor, done, err := c.backgroundOperation(sessionID)
	if err != nil {
		return harness.BackgroundTask{}, err
	}
	defer done()
	return c.background.CancelBackgroundTask(ctx, actor, taskID)
}

// ResumeBackgroundTask explicitly releases a safely suspended task, such as
// after a clean shutdown. version must match the current task snapshot. This
// cannot answer permission waits or replay failed/uncertain effects.
func (c *Client) ResumeBackgroundTask(ctx context.Context, sessionID, taskID string, version int64) (harness.BackgroundTask, error) {
	actor, done, err := c.backgroundOperation(sessionID)
	if err != nil {
		return harness.BackgroundTask{}, err
	}
	defer done()
	return c.background.ResumeBackgroundTask(ctx, actor, taskID, version)
}

// ApproveBackgroundTask resolves a saved interaction batch. The caller supplies
// only its public identity/version and decisions, never native resume targets.
// Every answer is validated and committed with the native resume transition.
func (c *Client) ApproveBackgroundTask(ctx context.Context, sessionID, taskID string, request harness.TaskApproval) (harness.BackgroundTask, error) {
	actor, done, err := c.backgroundOperation(sessionID)
	if err != nil {
		return harness.BackgroundTask{}, err
	}
	defer done()
	return c.background.ApproveBackgroundTask(ctx, actor, taskID, request)
}

func (c *Client) BackgroundNotifications(ctx context.Context, sessionID string, after int64, limit int) ([]harness.BackgroundNotification, error) {
	actor, done, err := c.backgroundOperation(sessionID)
	if err != nil {
		return nil, err
	}
	defer done()
	return c.background.BackgroundNotifications(ctx, actor, after, limit)
}

func (c *Client) AcknowledgeBackgroundNotification(ctx context.Context, sessionID, notificationID string) error {
	actor, done, err := c.backgroundOperation(sessionID)
	if err != nil {
		return err
	}
	defer done()
	return c.background.AcknowledgeBackgroundNotification(ctx, actor, notificationID)
}
