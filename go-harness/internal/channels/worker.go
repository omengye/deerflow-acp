package channels

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/channelproto"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type asyncDispatchKey struct{}
type activeDispatch struct {
	account, conversation, sender, workspace string
	cancel                                   context.CancelFunc
	done                                     chan struct{}
}

func (a *activeDispatch) matches(auth channelproto.Auth, cfg Config, msg channelproto.Message) bool {
	return a.account == auth.Account && a.conversation == msg.Conversation && a.sender == msg.Sender && a.workspace == cfg.Workspace
}
func (m *Manager) workerStep(ctx context.Context, i *instance) error {
	i.mu.Lock()
	if i.active != nil {
		select {
		case <-i.active.done:
			i.active = nil
		default:
		}
	}
	i.mu.Unlock()
	return m.step(context.WithValue(ctx, asyncDispatchKey{}, true), i)
}
func (m *Manager) joinActive(i *instance) {
	i.mu.Lock()
	a := i.active
	if a != nil {
		a.cancel()
	}
	i.mu.Unlock()
	if a != nil {
		<-a.done
	}
	i.mu.Lock()
	i.active = nil
	i.mu.Unlock()
}
func (m *Manager) control(ctx context.Context, i *instance, auth channelproto.Auth, cfg Config, msg channelproto.Message, session string) error {
	command := strings.TrimSpace(msg.Text)
	reply := ""
	i.mu.Lock()
	if i.auth.Account != auth.Account || i.auth.Owner != auth.Owner || i.cfg.Workspace != cfg.Workspace {
		i.mu.Unlock()
		return userError("channel identity changed")
	}
	active := i.active
	if active != nil {
		select {
		case <-active.done:
			active = nil
		default:
		}
	}
	policy := effectiveApproval(i.cfg, i.auth)
	if d, ok := DispatchFromContext(ctx); !ok || d.Revision != i.revision {
		i.mu.Unlock()
		return userError("channel policy changed")
	}
	switch {
	case command == "/help":
		reply = "/new 新会话\n/status 当前权限和任务状态\n/permission full|global|read_only 切换权限\n/resume 恢复待确认任务\n/approve 确认码 或 /deny 确认码\n/stop 停止当前微信任务\nFull Access 不绕过本机工具限制。"
	case command == "/permission":
		reply = "当前权限：" + approvalLabel(policy) + "。使用 /permission full、/permission global 或 /permission read_only。"
	case strings.HasPrefix(command, "/permission "):
		mode := strings.TrimSpace(strings.TrimPrefix(command, "/permission "))
		if mode == "full" {
			mode = harness.ApprovalAllowAlways
		}
		if active != nil {
			reply = "任务正在运行，先发送 /stop 并等待结束，再修改权限。"
		} else if err := m.setPermissionsLocked(ctx, i, mode, false, "wechat", msg.ID); err != nil {
			var public *UserError
			if errors.As(err, &public) {
				reply = public.Message
			} else {
				i.mu.Unlock()
				return err
			}
		} else {
			i.mu.Unlock()
			return nil
		}
	case active != nil && active.matches(auth, cfg, msg) && command == "/stop":
		active.cancel()
		// Explicit stop drops already queued ordinary prompts, never replays them.
		_, err := m.db.ExecContext(ctx, `UPDATE harness_channel_messages SET status='ignored' WHERE instance=? AND account=? AND conversation=? AND sender=? AND workspace=? AND status='queued' AND id<>?`, i.id, auth.Account, msg.Conversation, msg.Sender, cfg.Workspace, msg.ID)
		if err != nil {
			i.mu.Unlock()
			return err
		}
		reply = "已请求停止当前任务，正在等待工具清理完成。已取消排队的问题；已产生的效果不会自动撤销。"
	case active != nil && active.matches(auth, cfg, msg) && command == "/status":
		reply = "当前权限：" + approvalLabel(policy) + "\n任务正在运行。发送 /stop 可请求停止。\n会话：" + session
	}
	i.mu.Unlock()
	if reply == "" {
		out, err := m.run(ctx, session, cfg.Workspace, command, policy, func(string) error { return nil })
		reply = out.Text
		if out.Attention != "" {
			reply = attentionReply(out.Attention, reply)
		}
		if err != nil && reply == "" {
			reply = "暂时无法读取或操作该任务，请稍后重试 /status。"
		}
	}
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_, err := m.db.ExecContext(persist, `UPDATE harness_channel_messages SET status='ready',reply=?,session_id=? WHERE instance=? AND account=? AND id=? AND status='running'`, boundedText(reply), session, i.id, auth.Account, msg.ID)
	return err
}
