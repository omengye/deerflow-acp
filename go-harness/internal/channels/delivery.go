package channels

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/channelproto"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/automation"
)

func allowed(cfg Config, auth channelproto.Auth, sender string) bool {
	if sender == "" {
		return false
	}
	if auth.Owner != "" {
		return auth.Owner == sender
	}
	for _, id := range cfg.LegacyAllowed {
		if id == sender {
			return true
		}
	}
	return false
}
func (m *Manager) step(ctx context.Context, i *instance) error {
	i.mu.Lock()
	if !i.cfg.Enabled || len(i.auth.Secret) == 0 {
		i.mu.Unlock()
		return nil
	}
	if err := m.ensureProcess(ctx, i); err != nil {
		i.mu.Unlock()
		return err
	}
	p, auth, cursor, cfg := i.proc, i.auth, i.cursor, i.cfg
	i.mu.Unlock()
	handled, err := m.process(ctx, i, p, auth, cfg)
	if err != nil || handled {
		return err
	}
	var batch channelproto.Batch
	if err = p.Call(ctx, "poll", channelproto.Poll{Auth: auth, Cursor: cursor, Options: cfg.Options}, &batch); err != nil {
		i.mu.Lock()
		if i.proc == p {
			i.proc.Close()
			i.proc = nil
		}
		i.mu.Unlock()
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if len(batch.Messages) > 500 || len(batch.Cursor) > 65536 {
		return userError("plugin batch exceeds bounds")
	}
	if err = m.accept(ctx, i, auth, cfg, batch); err != nil {
		return err
	}
	i.mu.Lock()
	i.connected = true
	i.lastSuccess = time.Now().UTC()
	i.lastError = ""
	i.mu.Unlock()
	return nil
}
func (m *Manager) accept(ctx context.Context, i *instance, auth channelproto.Auth, cfg Config, batch channelproto.Batch) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.auth.Account != auth.Account {
		return userError("channel account changed")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var bound *channelproto.Auth
	for _, msg := range batch.Messages {
		if msg.Group || msg.ID == "" || len(msg.ID) > 512 || msg.Sender == "" || len(msg.Sender) > 512 || msg.Conversation == "" || len(msg.Conversation) > 512 || len(msg.Context) > 8192 || !utf8.ValidString(msg.Text) {
			continue
		}
		if !allowed(cfg, auth, msg.Sender) {
			if auth.Owner == "" && len(cfg.LegacyAllowed) == 0 && i.pairCode != "" && time.Now().Before(i.pairExpires) && strings.TrimSpace(msg.Text) == "/pair "+i.pairCode {
				copy := auth
				copy.Owner = msg.Sender
				plain, _ := json.Marshal(copy)
				sealed, e := sealCredentials(plain)
				if e != nil {
					return e
				}
				if _, err = tx.ExecContext(ctx, `UPDATE harness_channels SET auth=? WHERE id=?`, sealed, i.id); err != nil {
					return err
				}
				bound = &copy
				auth = copy
			}
			continue
		}
		status, reply, text := "queued", "", msg.Text
		if controlCommand(text) {
			text = strings.TrimSpace(text)
		}
		if msg.Unsupported || strings.TrimSpace(text) == "" || len(text) > 16384 {
			status = "ready"
			reply = "This channel currently accepts text only (up to 16 KiB). Use the desktop for attachments."
			text = ""
		}
		var duplicate bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_channel_messages WHERE instance=? AND account=? AND id=?)`, i.id, auth.Account, msg.ID).Scan(&duplicate); err != nil {
			return err
		}
		if duplicate {
			continue
		}
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM harness_channel_messages WHERE instance=? AND status IN ('queued','ready','running','sending')`, i.id).Scan(&count); err != nil {
			return err
		}
		if count >= 1000 {
			return userError("channel inbox is full; cursor not advanced")
		}
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO harness_channel_messages(instance,account,id,conversation,sender,context,text,status,reply,client_id,workspace,created_ms) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, i.id, auth.Account, msg.ID, msg.Conversation, msg.Sender, msg.Context, text, status, reply, "df-"+rand.Text(), cfg.Workspace, time.Now().UnixMilli())
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO harness_channel_admissions VALUES(?,?,?,?)`, i.id, auth.Account, msg.ID, effectiveApproval(cfg, auth)); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_channels SET cursor=? WHERE id=?`, batch.Cursor, i.id); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	i.cursor = batch.Cursor
	if bound != nil {
		i.auth = *bound
		i.pairCode = ""
	}
	return nil
}
func (m *Manager) process(ctx context.Context, i *instance, p transport, auth channelproto.Auth, cfg Config) (bool, error) {
	var msg channelproto.Message
	var state, reply, session, key, workspace string
	i.mu.Lock()
	if auth.Account != i.auth.Account || auth.Owner != i.auth.Owner || cfg.Workspace != i.cfg.Workspace {
		i.mu.Unlock()
		return false, userError("channel identity changed")
	}
	cfg = i.cfg
	revision := i.revision
	busy := i.active != nil
	i.mu.Unlock()
	controlSQL := `(trim(m.text) IN ('/status','/stop','/help','/permission') OR trim(m.text) LIKE '/permission %')`
	query := `SELECT id,conversation,sender,context,text,status,reply,session_id,client_id,workspace FROM harness_channel_messages m WHERE instance=? AND account=? AND status IN ('queued','ready') AND ((` + controlSQL + `) OR (NOT EXISTS(SELECT 1 FROM harness_channel_messages u WHERE u.instance=m.instance AND u.account=m.account AND u.conversation=m.conversation AND u.sender=m.sender AND u.workspace=m.workspace AND u.status IN ('unknown','running','sending'))))`
	if busy {
		query += ` AND (status='ready' OR (` + controlSQL + `))`
	}
	query += ` ORDER BY CASE WHEN (` + controlSQL + `) THEN 0 ELSE 1 END,rowid LIMIT 1`
	err := m.db.QueryRowContext(ctx, query, i.id, auth.Account).Scan(&msg.ID, &msg.Conversation, &msg.Sender, &msg.Context, &msg.Text, &state, &reply, &session, &key, &workspace)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	launched := false
	// Any exit after a durable claim must leave a reviewable receipt, even if a
	// session lookup or the first terminal write fails. Never strand 'running'.
	defer func() {
		if launched {
			return
		}
		persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_, _ = m.db.ExecContext(persist, `UPDATE harness_channel_messages SET status='unknown' WHERE instance=? AND account=? AND id=? AND status IN ('running','sending')`, i.id, auth.Account, msg.ID)
	}()
	if !allowed(cfg, auth, msg.Sender) || workspace != cfg.Workspace {
		_, err = m.db.ExecContext(ctx, `UPDATE harness_channel_messages SET status='ignored' WHERE instance=? AND account=? AND id=?`, i.id, auth.Account, msg.ID)
		return true, err
	}
	if state == "queued" {
		claim, e := m.db.ExecContext(ctx, `UPDATE harness_channel_messages SET status='running' WHERE instance=? AND account=? AND id=? AND status='queued'`, i.id, auth.Account, msg.ID)
		if e != nil {
			return true, e
		}
		if n, e := claim.RowsAffected(); e != nil || n != 1 {
			return true, e
		}
		err = m.db.QueryRowContext(ctx, `SELECT session_id FROM harness_channel_threads WHERE instance=? AND account=? AND conversation=? AND sender=? AND workspace=?`, i.id, auth.Account, msg.Conversation, msg.Sender, cfg.Workspace).Scan(&session)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return true, err
		}
		dispatch := Dispatch{i.id, auth.Account, msg.Conversation, msg.Sender, cfg.Workspace, msg.ID, revision}
		ctx = WithDispatch(ctx, dispatch)
		if controlCommand(msg.Text) {
			if err = m.control(ctx, i, auth, cfg, msg, session); err != nil {
				return true, err
			}
		} else if async, _ := ctx.Value(asyncDispatchKey{}).(bool); async {
			runCtx, cancel := context.WithCancel(ctx)
			active := &activeDispatch{account: auth.Account, conversation: msg.Conversation, sender: msg.Sender, workspace: cfg.Workspace, cancel: cancel, done: make(chan struct{})}
			i.mu.Lock()
			if i.revision != dispatch.Revision || i.auth.Account != auth.Account || i.auth.Owner != auth.Owner {
				i.mu.Unlock()
				cancel()
				return true, userError("channel policy changed before dispatch")
			}
			i.active = active
			i.mu.Unlock()
			launched = true
			go func() {
				defer close(active.done)
				defer cancel()
				_ = m.execute(ctx, runCtx, i, auth, cfg, msg, session)
			}()
			return true, nil
		} else {
			if err = m.execute(ctx, ctx, i, auth, cfg, msg, session); err != nil {
				return true, err
			}
		}
		if err = m.db.QueryRowContext(ctx, `SELECT reply FROM harness_channel_messages WHERE instance=? AND account=? AND id=?`, i.id, auth.Account, msg.ID).Scan(&reply); err != nil {
			return true, err
		}
	}
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	claim, err := m.db.ExecContext(ctx, `UPDATE harness_channel_messages SET status='sending' WHERE instance=? AND account=? AND id=? AND status='ready'`, i.id, auth.Account, msg.ID)
	if err != nil {
		return true, err
	}
	if n, e := claim.RowsAffected(); e != nil || n != 1 {
		return true, e
	}
	var ack struct {
		Sent bool `json:"sent"`
	}
	err = p.Call(ctx, "send", channelproto.Send{Auth: auth, Message: msg, Key: key, Text: reply, Options: cfg.Options}, &ack)
	if err == nil && !ack.Sent {
		err = userError("plugin did not acknowledge delivery")
	}
	terminal := "sent"
	if err != nil {
		terminal = "unknown"
	}
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_, saveErr := m.db.ExecContext(persist, `UPDATE harness_channel_messages SET status=? WHERE instance=? AND account=? AND id=? AND status='sending'`, terminal, i.id, auth.Account, msg.ID)
	if err != nil {
		i.mu.Lock()
		if i.proc == p {
			i.proc.Close()
			i.proc = nil
		}
		i.mu.Unlock()
	}
	return true, errors.Join(err, saveErr)
}
func boundedText(text string) string {
	if len(text) > 10000 {
		r := []rune(text)
		return string(r[:min(2400, len(r))]) + "\n[truncated; see desktop session]"
	}
	return text
}
func (m *Manager) History(ctx context.Context, id string) (any, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT id,account,sender,status,session_id,created_ms,workspace FROM harness_channel_messages WHERE instance=? ORDER BY rowid DESC LIMIT 100`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []any{}
	for rows.Next() {
		var msg, account, sender, status, session, workspace string
		var ms int64
		if err = rows.Scan(&msg, &account, &sender, &status, &session, &ms, &workspace); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": msg, "account": account, "sender": sender, "status": status, "session_id": session, "created_ms": ms, "workspace": workspace})
	}
	return out, rows.Err()
}
func (m *Manager) Review(ctx context.Context, id, msg string) error {
	i, err := m.get(id)
	if err != nil {
		return err
	}
	i.mu.Lock()
	account := i.auth.Account
	i.mu.Unlock()
	r, err := m.db.ExecContext(ctx, `UPDATE harness_channel_messages SET status='reviewed' WHERE instance=? AND account=? AND id=? AND status='unknown'`, id, account, msg)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n != 1 {
		return userError("unknown channel message not found")
	}
	return err
}
func (m *Manager) CanNotify(target *automation.NotificationTarget) bool {
	if target == nil {
		return false
	}
	i, err := m.get(target.Channel)
	if err != nil {
		return false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.cfg.Enabled && i.auth.Account == target.Account && allowed(i.cfg, i.auth, target.Sender) && target.Conversation != ""
}
func (m *Manager) LegacyTarget(sender string) *automation.NotificationTarget {
	i, err := m.get("wechat")
	if err != nil {
		return nil
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return &automation.NotificationTarget{Channel: "wechat", Account: i.auth.Account, Conversation: sender, Sender: sender}
}

// This callback only uses the caller's transaction, never plugin/network I/O.
func (m *Manager) EnqueueNotificationTx(ctx context.Context, tx *sql.Tx, spec automation.Spec, occ automation.Occurrence) error {
	target := spec.Notify
	if target == nil && spec.NotifyWeChat != "" {
		target = &automation.NotificationTarget{Channel: "wechat", Conversation: spec.NotifyWeChat, Sender: spec.NotifyWeChat}
	}
	if target == nil {
		return nil
	}
	var rawCfg, rawAuth, rawManifest []byte
	err := tx.QueryRowContext(ctx, `SELECT config,auth,manifest FROM harness_channels WHERE id=?`, target.Channel).Scan(&rawCfg, &rawAuth, &rawManifest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var cfg Config
	var auth channelproto.Auth
	var manifest Manifest
	if err = json.Unmarshal(rawManifest, &manifest); err != nil {
		return err
	}
	if err = json.Unmarshal(rawCfg, &cfg); err != nil {
		return err
	}
	if !cfg.Enabled || len(rawAuth) == 0 {
		return nil
	}
	plain, err := openCredentials(rawAuth)
	if err != nil {
		return err
	}
	if err = json.Unmarshal(plain, &auth); err != nil {
		return err
	}
	copyTarget := *target
	if copyTarget.Account == "" && spec.NotifyWeChat != "" {
		copyTarget.Account = auth.Account
	}
	target = &copyTarget
	if auth.Account != target.Account || !allowed(cfg, auth, target.Sender) {
		return nil
	}
	var contextToken string
	err = tx.QueryRowContext(ctx, `SELECT context FROM harness_channel_messages WHERE instance=? AND account=? AND conversation=? AND sender=? AND context<>'' ORDER BY rowid DESC LIMIT 1`, target.Channel, target.Account, target.Conversation, target.Sender).Scan(&contextToken)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	state := "ready"
	if contextToken == "" && manifest.Descriptor.RequiresReplyContext {
		state = "unknown"
	}
	reply := boundedText(fmt.Sprintf("Scheduled task: %s\nStatus: %s\nSession: %s\n%s", spec.Name, occ.Outcome.Status, occ.Outcome.SessionID, occ.Outcome.Text))
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO harness_channel_messages(instance,account,id,conversation,sender,context,text,status,reply,session_id,client_id,workspace,created_ms) VALUES(?,?,?,?,?,?,'',?,?,?,?,?,?)`, target.Channel, target.Account, "schedule-"+occ.ID, target.Conversation, target.Sender, contextToken, state, reply, occ.Outcome.SessionID, "df-schedule-"+occ.ID, cfg.Workspace, time.Now().UnixMilli())
	return err
}

func (m *Manager) execute(parent, ctx context.Context, i *instance, auth channelproto.Auth, cfg Config, msg channelproto.Message, session string) error {
	defer func() {
		persist, cancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
		defer cancel()
		_, _ = m.db.ExecContext(persist, `UPDATE harness_channel_messages SET status='unknown' WHERE instance=? AND account=? AND id=? AND status='running'`, i.id, auth.Account, msg.ID)
	}()
	approval := effectiveApproval(cfg, auth)
	admitted := harness.ApprovalGlobal
	e := m.db.QueryRowContext(parent, `SELECT approval FROM harness_channel_admissions WHERE instance=? AND account=? AND id=?`, i.id, auth.Account, msg.ID).Scan(&admitted)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	approval = dispatchApproval(admitted, approval)
	out, runErr := m.run(ctx, session, cfg.Workspace, msg.Text, approval, func(id string) error {
		tx, e := m.db.BeginTx(ctx, nil)
		if e != nil {
			return e
		}
		defer tx.Rollback()
		if _, e = tx.ExecContext(ctx, `INSERT INTO harness_channel_threads VALUES(?,?,?,?,?,?) ON CONFLICT(instance,account,conversation,sender,workspace) DO UPDATE SET session_id=excluded.session_id`, i.id, auth.Account, msg.Conversation, msg.Sender, cfg.Workspace, id); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE harness_channel_messages SET session_id=? WHERE instance=? AND account=? AND id=?`, id, i.id, auth.Account, msg.ID); e != nil {
			return e
		}
		if e = tx.Commit(); e != nil {
			return e
		}
		session = id
		return nil
	})
	reply := out.Text
	if runErr != nil || out.Status != "completed" {
		notice := out.Attention
		if notice == "" {
			notice = "任务需要在桌面端检查权限或恢复状态。会话：" + session + "\n请打开设置 > 渠道 > 微信 > 查看最近任务。本条消息不会自动重试；也可发送 /new 开始新会话。"
		}
		reply = attentionReply(notice, reply)
	}
	if reply == "" {
		reply = "Completed. Desktop session: " + session
	}
	reply = boundedText(reply)
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	terminal := "ready"
	if parent.Err() != nil {
		terminal = "unknown"
	}
	_, err := m.db.ExecContext(persist, `UPDATE harness_channel_messages SET status=?,reply=? WHERE instance=? AND account=? AND id=? AND status='running'`, terminal, reply, i.id, auth.Account, msg.ID)
	cancel()
	if err != nil {
		return err
	}
	return nil
}

// Preserve a complete confirmation even when model prose needs truncation.
func attentionReply(notice, text string) string {
	if len(notice) > 9000 {
		return "确认详情过长，请在桌面端查看；本条不提供微信批准。"
	}
	remaining := 9900 - len(notice)
	if len(text) > remaining {
		text = text[:remaining]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
		text += "\n[truncated; see desktop session]"
	}
	return notice + "\n\n" + text
}
