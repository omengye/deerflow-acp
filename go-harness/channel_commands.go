package deerflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/automation"
	"github.com/omengye/deerflow-acp/go-harness/internal/channels"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
)

func (c *Client) channelExecutionCommand(ctx context.Context, id, workspace string, command []string, approval string) (out automation.Outcome, err error) {
	out = automation.Outcome{SessionID: id, Status: "completed"}
	if id == "" {
		out.Text = "尚未创建会话，请发送问题或 /new。"
		return out, nil
	}
	if (command[0] == "/approve" || command[0] == "/deny") && len(command) != 2 || (command[0] != "/approve" && command[0] != "/deny" && len(command) != 1) {
		out.Text = "命令格式错误。使用 /status、/resume、/stop、/approve 确认码 或 /deny 确认码。"
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	owner := hr.NewID()
	defer func() { err = errors.Join(err, c.service.Disconnect(context.Background(), owner)) }()
	x, err := c.service.Load(ctx, owner, id, workspace, false, nil)
	if err != nil {
		out.Text = channelAttention(id, nil, err)
		return out, err
	}
	state, err := c.service.Execution(ctx, owner, id, "")
	if errors.Is(err, harness.ErrNotFound) {
		out.Text = "当前权限：" + channelPermissionName(approval) + "\n会话尚无任务。"
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if command[0] == "/stop" {
		if state.Status != harness.ExecutionWaitingInput {
			out.Text = "当前没有此微信可停止的运行或待确认任务。"
			return out, nil
		}
		_, err = c.service.CancelExecution(ctx, owner, id, harness.CancelExecutionRequest{RunID: state.RunID, ExpectedVersion: state.Version})
		if err == nil {
			out.Text = "已取消待确认任务。已完成的操作不会撤销，可发送新问题。"
		}
		return out, err
	}
	if command[0] == "/status" || state.Status != harness.ExecutionWaitingInput || !state.Resumable {
		out.Text, err = c.channelWaitingNotice(ctx, owner, x, state, approval)
		return out, err
	}
	if approval == harness.ApprovalReadOnly || (x.ApprovalMode == harness.ApprovalAllowAlways && approval != harness.ApprovalAllowAlways) {
		out.Text = "当前权限不允许恢复这个任务的原始授权。可用 /stop 取消后重新提问，或 /new 开始新会话。"
		return out, nil
	}
	dispatch, ok := channels.DispatchFromContext(ctx)
	if !ok {
		return out, harness.ErrPermissionDenied
	}
	requests, err := c.service.ExecutionPermissions(ctx, owner, id, state)
	if err != nil {
		return out, err
	}
	if command[0] == "/approve" || command[0] == "/deny" {
		decision := harness.AllowOnce
		if command[0] == "/deny" {
			decision = harness.RejectOnce
		}
		if err = c.channels.Decide(ctx, dispatch, state, command[1], decision); err != nil {
			out.Text = "确认码无效、已使用或已过期；请发送 /status 获取当前确认。"
			return out, nil
		}
	}
	required := []harness.ExecutionInteraction{}
	for _, p := range state.WaitingInputs {
		if approval != harness.ApprovalAllowAlways || channelExplicitPermission(p, requests) {
			required = append(required, p)
		}
	}
	decisions, ready, err := c.channels.TakeDecisions(ctx, dispatch, state, required)
	if err != nil {
		return out, err
	}
	if !ready {
		out.Text, err = c.channelWaitingNotice(ctx, owner, x, state, approval)
		return out, err
	}
	expected := map[string]harness.PermissionRequest{}
	for _, p := range requests {
		expected[p.ID] = p
	}
	approve := func(_ context.Context, p harness.PermissionRequest) (harness.PermissionDecision, error) {
		if original, ok := expected[p.ID]; ok {
			if p.SessionID != id || p.RunID != state.RunID || p.ConfigVersion != original.ConfigVersion || p.ToolName != original.ToolName || string(p.Arguments) != string(original.Arguments) {
				return harness.PermissionCancelled, harness.ErrExecutionConflict
			}
			if d, ok := decisions[p.ID]; ok {
				return d, nil
			}
		}
		if approval == harness.ApprovalAllowAlways && !harness.ExplicitSkillEditApproval(p) {
			return harness.AllowOnce, nil
		}
		return harness.PermissionCancelled, nil
	}
	var tools channelTools
	var mu sync.Mutex
	var answer strings.Builder
	emit := func(ctx context.Context, e harness.RunEvent) error {
		if e.Kind == "text_delta" && (e.AgentName == "" || e.AgentName == "deerflow") {
			mu.Lock()
			if answer.Len() < 16000 {
				answer.WriteString(e.Text[:min(len(e.Text), 16000-answer.Len())])
			}
			mu.Unlock()
		}
		return tools.observe(ctx, e)
	}
	result, err := c.service.ResumeExecution(ctx, owner, id, harness.ResumeExecutionRequest{RunID: state.RunID, ExpectedVersion: state.Version}, emit, approve)
	out.Text = strings.ToValidUTF8(answer.String(), "")
	if s := tools.summary(result.Execution); s != "" {
		out.Text = s + "\n\n" + out.Text
	}
	if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted {
		out.Status = "needs_attention"
		out.Attention = channelAttention(id, result.Execution, err)
		if result.Execution != nil && result.Execution.Status == harness.ExecutionWaitingInput {
			notice, e := c.channelWaitingNotice(ctx, owner, x, *result.Execution, approval)
			if e == nil {
				out.Attention = notice
			}
		}
	}
	return out, err
}

func channelPermissionName(mode string) string {
	switch mode {
	case harness.ApprovalAllowAlways:
		return "Full Access"
	case harness.ApprovalReadOnly:
		return "只读"
	default:
		return "跟随全局/确认模式"
	}
}

func (c *Client) channelWaitingNotice(ctx context.Context, owner string, x harness.Session, state harness.ExecutionState, approval string) (string, error) {
	text := "当前权限：" + channelPermissionName(approval) + "\n会话：" + x.ID + "\n任务状态：" + string(state.Status)
	if state.Status != harness.ExecutionWaitingInput {
		return text, nil
	}
	if !state.Resumable {
		return text + "\n当前检查点无法安全恢复，请在桌面检查，或 /new 开始新会话。", nil
	}
	dispatch, ok := channels.DispatchFromContext(ctx)
	if !ok {
		return channelAttention(x.ID, &state, nil), nil
	}
	requests, err := c.service.ExecutionPermissions(ctx, owner, x.ID, state)
	if err != nil {
		return "", err
	}
	for _, p := range state.WaitingInputs {
		if approval == harness.ApprovalAllowAlways && !channelExplicitPermission(p, requests) {
			text += "\n待恢复：" + channelToolDescription(p.ToolName)
			continue
		}
		var request harness.PermissionRequest
		for _, v := range requests {
			if v.ID == p.ID {
				request = v
				break
			}
		}
		review, safe := channelConfirmationReview(request)
		if !safe || len(text)+len(review) > 6500 {
			text += "\n此调用的完整变更不适合微信展示，请在桌面检查；不会自动批准。"
			continue
		}
		code, e := c.channels.Confirmation(ctx, dispatch, state, p)
		if e != nil {
			return "", e
		}
		if code == "" {
			text += "\n此调用已收到决定，等待其他确认或 /resume。"
			continue
		}
		text += "\n\n" + review + "\n/approve " + code + "\n/deny " + code + "\n（10 分钟有效，仅确认这一次调用）"
	}
	return text + "\n\n/resume 接着原任务执行，不重跑原问题；/stop 取消；/new 新会话。", nil
}

func channelExplicitPermission(p harness.ExecutionInteraction, requests []harness.PermissionRequest) bool {
	for _, request := range requests {
		if request.ID == p.ID {
			return harness.ExplicitSkillEditApproval(request)
		}
	}
	return true
}

func channelConfirmationReview(p harness.PermissionRequest) (string, bool) {
	if p.ID == "" {
		return "", false
	}
	if p.ToolName != "skill_edit" {
		// Generic shell/extension arguments cannot safely be summarized as authority.
		if p.ToolName != "web_fetch" && p.ToolName != "web_search" {
			return "", false
		}
		return "待确认：" + channelToolDescription(p.ToolName) + "\n参数不在微信展示；批准仅针对当前这一条已记录调用。", true
	}
	var in preparedSkillEdit
	if json.Unmarshal(p.Arguments, &in) != nil || in.Action != "apply" || in.Review == nil {
		return "", false
	}
	review := in.Review
	text := "应用已安装 Skill 修改：" + review.Target.Name
	for _, change := range review.Changes {
		if strings.ContainsAny(change.Path, "\r\n") {
			return "", false
		}
		text += "\n文件：" + change.Path + "\n" + strings.ReplaceAll(change.Preview, "\n", "\n| ")
	}
	sum := sha256.Sum256(p.Arguments)
	text += "\n变更摘要指纹：" + hex.EncodeToString(sum[:])[:12]
	return text, len(text) <= 4500 && len(review.Changes) > 0 && !strings.ContainsRune(text, 0)
}
