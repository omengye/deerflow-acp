package deerflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/automation"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/channels"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
)

func channelTaskWait(s *harness.ExecutionState) bool {
	return s != nil && s.Status == harness.ExecutionWaitingInput && len(s.WaitingTasks) > 0 && len(s.WaitingInputs) == 0 && s.BlockedReason == "" &&
		(s.Automatic == nil || s.Automatic.Status != "blocked" && s.Automatic.Status != "stopped")
}

func channelBlockedNotice(s harness.ExecutionState) string {
	if s.Automatic != nil && s.Automatic.Status == "blocked" {
		return "恢复受阻：自动重试次数已用完，请在桌面检查并恢复原任务。"
	}
	return "恢复受阻：检查点、会话配置或工具执行记录未通过校验，请在桌面检查。"
}

func (c *Client) continueChannelRun(ctx context.Context, d channels.Dispatch, wait channels.TaskWait) (*automation.Outcome, error) {
	ctx = hr.WithTransportCancellation(ctx)
	sessionID, runID := wait.SessionID, wait.RunID
	var status harness.ExecutionStatus
	err := c.store.DB().QueryRowContext(ctx, `SELECT status FROM harness_executions WHERE session_id=? AND run_id=?`, sessionID, runID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return &automation.Outcome{Text: "原任务记录已移除。"}, nil
	}
	if err != nil {
		return nil, err
	}
	out := &automation.Outcome{SessionID: sessionID, RunID: runID}
	switch status {
	case harness.ExecutionCompleted:
		return c.channelRunAnswer(ctx, out)
	case harness.ExecutionFailed:
		out.Text = "任务执行失败。请在桌面查看会话 " + sessionID
		return out, nil
	case harness.ExecutionCancelled:
		out.Text = "任务已取消。会话：" + sessionID
		return out, nil
	case harness.ExecutionNeedsReconciliation:
		out.Text = "工具执行结果待核对。请在桌面查看会话 " + sessionID
		return out, nil
	case harness.ExecutionWaitingInput:
	default:
		return nil, nil
	}
	owner := hr.NewID()
	x, err := c.service.Load(ctx, owner, sessionID, d.Workspace, false, nil)
	if err != nil {
		if errors.Is(err, harness.ErrAttachedElsewhere) || errors.Is(err, harness.ErrBusy) {
			return nil, nil // The desktop owner may already be completing this run.
		}
		return nil, err
	}
	defer c.service.Disconnect(context.Background(), owner)
	state, err := c.service.Execution(ctx, owner, sessionID, runID)
	if err != nil || state.Status != harness.ExecutionWaitingInput {
		return nil, err
	}
	if wait.StopRequested {
		_, err := c.service.CancelExecution(ctx, owner, sessionID, harness.CancelExecutionRequest{RunID: runID, ExpectedVersion: state.Version})
		return nil, err
	}
	if x.ApprovalMode != wait.Approval {
		out.Text = "恢复受阻：微信当前权限与原任务不一致。请在桌面检查会话 " + sessionID
		return out, nil
	}
	if !channelTaskWait(&state) {
		out.Text, err = c.channelWaitingNotice(ctx, owner, x, state, wait.Approval)
		return out, err
	}
	result, err := c.service.ContinueTaskWait(ctx, owner, state, harness.SessionUpdates{
		Permission: func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
			return harness.PermissionCancelled, nil
		},
	})
	if result.Execution != nil && result.Execution.Status == harness.ExecutionCompleted {
		return c.channelRunAnswer(ctx, out)
	}
	// Re-read the durable outcome on the next check (including a lost race,
	// a new permission wait, or the runtime's bounded automatic retry policy).
	if errors.Is(err, harness.ErrBusy) || errors.Is(err, harness.ErrExecutionConflict) || errors.Is(err, harness.ErrExecutionWaitingInput) {
		return nil, nil
	}
	var limit *budget.LimitError
	if errors.Is(err, harness.ErrResourcesIncompatible) || errors.Is(err, harness.ErrResourcesWaiting) ||
		errors.Is(err, harness.ErrPermissionDenied) || errors.As(err, &limit) && !limit.Temporary || errors.Is(err, harness.ErrReconciliationRequired) {
		out.Text = "任务需要在桌面检查预算、资源授权或工具执行记录后恢复。会话：" + sessionID
		return out, nil
	}
	return nil, err
}

// The finishing owner can be the desktop or another daemon incarnation.
// Reconstruct only committed output belonging to the bound run.
func (c *Client) channelRunAnswer(ctx context.Context, out *automation.Outcome) (*automation.Outcome, error) {
	rows, err := c.store.DB().QueryContext(ctx, `SELECT event FROM harness_events WHERE session_id=? AND run_id=? AND json_extract(CAST(event AS TEXT),'$.kind') IN ('text_delta','image_delta','tool_end') ORDER BY sequence`, out.SessionID, out.RunID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var answer strings.Builder
	images := channelImages{session: out.SessionID}
	for rows.Next() {
		var raw []byte
		var event harness.RunEvent
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &event); err != nil {
			return nil, err
		}
		images.observe(event)
		if event.AgentName != "" && event.AgentName != "deerflow" {
			continue
		}
		if event.Kind == "tool_end" {
			answer.Reset()
		}
		if event.Kind == "text_delta" && answer.Len() < 16000 {
			answer.WriteString(event.Text[:min(len(event.Text), 16000-answer.Len())])
		}
	}
	out.Text = strings.ToValidUTF8(answer.String(), "")
	images.finish(out)
	if out.Text == "" && len(out.Images) == 0 {
		out.Text = "任务已完成。桌面会话：" + out.SessionID
	}
	return out, rows.Err()
}
