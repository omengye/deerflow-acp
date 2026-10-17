package deerflow

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/automation"
	"github.com/omengye/deerflow-acp/go-harness/internal/channels"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
)

// Channel commands never reach the model or change an old execution's approvals.
func (c *Client) channelRun(ctx context.Context, existing, workspace, prompt, approval string, bind func(string) error) (out automation.Outcome, err error) {
	command := strings.Fields(strings.TrimSpace(prompt))
	if len(command) > 0 {
		switch command[0] {
		case "/status", "/resume", "/approve", "/deny", "/stop":
			return c.channelExecutionCommand(ctx, existing, workspace, command, approval)
		}
	}
	if strings.TrimSpace(prompt) == "/new" {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		owner := hr.NewID()
		defer func() { err = errors.Join(err, c.service.Disconnect(context.Background(), owner)) }()
		x, err := c.service.NewSession(ctx, owner, workspace)
		if err != nil {
			return out, err
		}
		if approval == "" {
			approval = harness.ApprovalGlobal
		}
		if _, err = c.service.SetConfigOption(ctx, owner, x.ID, "approval", approval); err != nil {
			return out, err
		}
		// Binding and the command's session receipt commit together in the inbox.
		if err = bind(x.ID); err != nil {
			return out, err
		}
		return automation.Outcome{SessionID: x.ID, Status: "completed", Text: "已创建并切换到新会话。请发送你的问题。\n会话：" + x.ID + "\n旧会话和记录仍保留，待审批任务不会自动批准或重试。"}, nil
	}
	var tools channelTools
	out, execution, err := c.unattendedWithEvents(ctx, existing, workspace, prompt, approval, bind, tools.observe)
	if out.SessionID == "" {
		out.SessionID = existing
	}
	if err != nil || out.Status != "completed" {
		out.Attention = channelAttention(out.SessionID, execution, err)
		if execution != nil && execution.Status == harness.ExecutionWaitingInput {
			if _, ok := channels.DispatchFromContext(ctx); ok {
				status, statusErr := c.channelExecutionCommand(ctx, out.SessionID, workspace, []string{"/status"}, approval)
				if statusErr == nil {
					out.Attention = status.Text
				}
			}
		}
	}
	if summary := tools.summary(execution); summary != "" {
		out.Text = summary + "\n\n" + out.Text
	}
	return out, err
}

type channelTool struct{ id, name, status string }
type channelTools struct {
	mu      sync.Mutex
	items   []channelTool
	omitted bool
}

func (t *channelTools) observe(_ context.Context, e harness.RunEvent) error {
	if e.Kind != "tool_start" && e.Kind != "tool_execute" && e.Kind != "tool_end" {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := range t.items {
		if t.items[i].id == e.ToolCallID {
			t.items[i].status = e.Status
			return nil
		}
	}
	if len(t.items) == 8 {
		t.items = t.items[1:]
		t.omitted = true
	}
	t.items = append(t.items, channelTool{e.ToolCallID, e.ToolName, e.Status})
	return nil
}

func (t *channelTools) summary(state *harness.ExecutionState) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.items) == 0 {
		return ""
	}
	lines := []string{"工具调用摘要"}
	if t.omitted {
		lines = append(lines, "（仅显示最近 8 项，完整记录见桌面端）")
	}
	for _, item := range t.items {
		status := "未完成，请查看桌面端"
		switch item.status {
		case "completed":
			status = "已完成"
		case "failed":
			status = "未成功"
		}
		if state != nil {
			for _, pending := range state.WaitingInputs {
				if pending.ToolCallID == item.id {
					status = "等待确认"
				}
			}
		}
		lines = append(lines, "- "+channelToolDescription(item.name)+"："+status)
	}
	return strings.Join(lines, "\n")
}

// Only host-authored labels are exported. Never forward arguments, tool output,
// descriptions, URLs, commands or raw errors; these can contain credentials.
func channelToolDescription(name string) string {
	labels := map[string]string{
		"skill": "加载 Skill", "skill_inspect": "检查 Skill 信息",
		"read_skill_file": "读取 Skill 文件", "list_skill_files": "列出 Skill 文件",
		"list_directory": "列出目录", "read_file": "读取文件",
		"write_file": "写入文件", "edit_file": "编辑文件",
		"web_fetch": "访问网页或网络接口", "web_search": "搜索网页",
		"bash": "执行终端命令", "shell": "执行终端命令", "execute_command": "执行终端命令",
		"ask_clarification": "请求补充信息", "task": "执行子任务",
		"skill_edit": "修改已安装的 Skill",
	}
	if label, ok := labels[name]; ok {
		return name + "（" + label + "）"
	}
	return "扩展工具（名称和参数请在桌面端查看）"
}

func channelAttention(session string, state *harness.ExecutionState, err error) string {
	lines := []string{"任务尚未完成，需要在桌面端检查或恢复。"}
	if state != nil && state.Status == harness.ExecutionWaitingInput {
		lines[0] = "当前会话有待确认的任务，尚未完成。"
		for i, pending := range state.WaitingInputs {
			if i == 8 {
				lines = append(lines, "更多待确认工具请在桌面端查看。")
				break
			}
			lines = append(lines, "待确认工具："+channelToolDescription(pending.ToolName))
		}
		if len(state.WaitingInputs) > 0 {
			lines = append(lines, "当前权限策略要求桌面端确认这些调用；微信不会自动批准。")
		}
		if !state.Resumable {
			lines = append(lines, "此任务目前不能直接恢复，请先在桌面端检查。")
		}
	} else if errors.Is(err, harness.ErrAttachedElsewhere) || errors.Is(err, harness.ErrBusy) {
		lines[0] = "当前会话正被桌面端或其他执行占用，本条消息未开始执行。"
	}
	lines = append(lines, "会话："+session, "请打开设置 > 渠道 > 微信 > 查看最近任务 > 打开任务。", "处理后请在微信重新发送未执行的问题。也可发送 /new 切换到新会话；旧任务和记录会保留。本条消息不会自动重试。")
	return strings.Join(lines, "\n")
}
