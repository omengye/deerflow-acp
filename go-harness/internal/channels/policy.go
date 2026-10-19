package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/channelproto"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/automation"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
)

type FullAccessGrant struct {
	Account   string `json:"account"`
	Owner     string `json:"owner"`
	Workspace string `json:"workspace"`
	GrantedAt int64  `json:"granted_ms"`
}

type Dispatch struct {
	Instance, Account, Conversation, Sender, Workspace, MessageID string
	Revision                                                      int64
}
type dispatchKey struct{}

func DispatchFromContext(ctx context.Context) (Dispatch, bool) {
	d, ok := ctx.Value(dispatchKey{}).(Dispatch)
	return d, ok
}

// WithDispatch is host-only provenance, never decoded from plugin/model input.
func WithDispatch(ctx context.Context, d Dispatch) context.Context {
	return context.WithValue(ctx, dispatchKey{}, d)
}
func (d Dispatch) scope() string {
	b, _ := json.Marshal([]string{d.Instance, d.Account, d.Conversation, d.Sender, d.Workspace})
	return string(b)
}

// ScheduleSource captures the locally granted capability, not the model's text.
func (m *Manager) ScheduleSource(d Dispatch, sessionID string) automation.Source {
	s := automation.Source{Kind: d.Instance, Account: d.Account, Sender: d.Sender, Workspace: d.Workspace, Conversation: d.Conversation, MessageID: d.MessageID, SessionID: sessionID}
	workspace, err := session.NormalizeWorkspace(d.Workspace)
	if err != nil {
		return s
	}
	s.Workspace = workspace
	i, err := m.get(d.Instance)
	if err != nil {
		return s
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.cfg.Enabled && i.revision == d.Revision && i.auth.Account == d.Account && i.auth.Owner == d.Sender && i.cfg.Workspace == workspace && fullAccessAuthorized(i.cfg, i.auth) && effectiveApproval(i.cfg, i.auth) == harness.ApprovalAllowAlways {
		s.Grant = fmt.Sprint(i.cfg.FullAccess.GrantedAt)
	}
	return s
}

func (m *Manager) AuthorizeSchedule(s automation.Source, full bool) bool {
	i, err := m.get(s.Kind)
	if err != nil {
		return false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.cfg.Enabled || i.auth.Account != s.Account || i.cfg.Workspace != s.Workspace || !allowed(i.cfg, i.auth, s.Sender) {
		return false
	}
	return !full || s.Grant != "" && fullAccessAuthorized(i.cfg, i.auth) && effectiveApproval(i.cfg, i.auth) == harness.ApprovalAllowAlways && s.Grant == fmt.Sprint(i.cfg.FullAccess.GrantedAt)
}

func fullAccessAuthorized(cfg Config, auth channelproto.Auth) bool {
	g := cfg.FullAccess
	return g != nil && auth.Owner != "" && g.Account == auth.Account && g.Owner == auth.Owner && g.Workspace == cfg.Workspace && len(auth.Secret) > 0
}
func effectiveApproval(cfg Config, auth channelproto.Auth) string {
	switch cfg.Approval {
	case harness.ApprovalAllowAlways:
		if fullAccessAuthorized(cfg, auth) {
			return harness.ApprovalAllowAlways
		}
		return harness.ApprovalGlobal
	case harness.ApprovalGlobal, harness.ApprovalReadOnly:
		return cfg.Approval
	}
	if cfg.LegacyApproval != "" {
		return cfg.LegacyApproval
	}
	return harness.ApprovalGlobal
}
func dispatchApproval(admitted, current string) string {
	if admitted == harness.ApprovalReadOnly || current == harness.ApprovalReadOnly {
		return harness.ApprovalReadOnly
	}
	if admitted == harness.ApprovalRejectAlways || current == harness.ApprovalRejectAlways {
		return harness.ApprovalRejectAlways
	}
	if admitted == harness.ApprovalAllowAlways && current == harness.ApprovalAllowAlways {
		return current
	}
	if admitted == harness.ApprovalAsk || current == harness.ApprovalAsk {
		return harness.ApprovalAsk
	}
	return harness.ApprovalGlobal
}
func approvalLabel(mode string) string {
	switch mode {
	case harness.ApprovalAllowAlways:
		return "Full Access"
	case harness.ApprovalReadOnly:
		return "只读"
	case harness.ApprovalAsk:
		return "逐次确认"
	case harness.ApprovalRejectAlways:
		return "拒绝受限工具"
	default:
		return "跟随全局"
	}
}

// SetPermissions is local-management only. Chat may select full only within this grant.
func (m *Manager) SetPermissions(ctx context.Context, id string, revision int64, approval string) (any, error) {
	i, err := m.get(id)
	if err != nil {
		return nil, err
	}
	i.op.Lock()
	defer i.op.Unlock()
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.active != nil {
		return nil, userError("任务正在运行；请先发送 /stop 并等待结束")
	}
	var running bool
	if err = m.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_channel_messages WHERE instance=? AND status IN ('running','sending'))`, i.id).Scan(&running); err != nil {
		return nil, err
	}
	if running {
		return nil, userError("渠道正在处理消息，请稍后重试")
	}
	if revision != i.revision {
		return nil, userError("channel changed; refresh before saving")
	}
	if err = m.setPermissionsLocked(ctx, i, approval, true, "desktop", ""); err != nil {
		return nil, err
	}
	return map[string]any{"saved": true, "revision": i.revision}, nil
}
func (m *Manager) setPermissionsLocked(ctx context.Context, i *instance, approval string, local bool, source, message string) error {
	if approval != harness.ApprovalGlobal && approval != harness.ApprovalAllowAlways && approval != harness.ApprovalReadOnly {
		return userError("invalid channel approval")
	}
	if i.auth.Owner == "" || len(i.auth.Secret) == 0 || i.cfg.Workspace == "" {
		return userError("请先绑定本人账号并选择工作目录")
	}
	if approval == harness.ApprovalAllowAlways && !local && !fullAccessAuthorized(i.cfg, i.auth) {
		return userError("本机尚未授权此微信绑定使用 Full Access，请先在渠道设置授权一次")
	}
	old := i.cfg
	i.cfg.Approval = approval
	if local {
		i.cfg.FullAccess = nil
		if approval == harness.ApprovalAllowAlways {
			i.cfg.FullAccess = &FullAccessGrant{i.auth.Account, i.auth.Owner, i.cfg.Workspace, time.Now().UnixMilli()}
		}
	}
	i.revision++
	tx, err := m.db.BeginTx(ctx, nil)
	if err == nil {
		defer tx.Rollback()
		err = m.persistWith(ctx, tx, i)
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO harness_channel_policy_audit(instance,account,owner,workspace,approval,source,revision,created_ms) VALUES(?,?,?,?,?,?,?,?)`, i.id, i.auth.Account, i.auth.Owner, i.cfg.Workspace, approval, source, i.revision, time.Now().UnixMilli())
		}
		if err == nil && message != "" {
			_, err = tx.ExecContext(ctx, `UPDATE harness_channel_messages SET status='ready',reply=? WHERE instance=? AND account=? AND id=? AND status='running'`, "权限已设为 "+approvalLabel(approval)+"。新收到的消息使用此策略；旧待审批任务请发送 /resume。", i.id, i.auth.Account, message)
		}
		if err == nil {
			err = tx.Commit()
		}
	}
	if err != nil {
		i.cfg = old
		i.revision--
	}
	return err
}

func controlCommand(text string) bool {
	text = strings.TrimSpace(text)
	return text == "/status" || text == "/stop" || text == "/help" || text == "/permission" || strings.HasPrefix(text, "/permission ")
}
