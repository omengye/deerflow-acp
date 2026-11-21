package channels

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/channelproto"
)

const receiptText = "👌 收到"

func wantsReceipt(msg channelproto.Message) bool {
	return strings.TrimSpace(msg.Context) != "" && strings.TrimSpace(msg.Text) != "" && !receiptExcludedCommand(msg.Text)
}

func receiptExcludedCommand(text string) bool {
	words := strings.Fields(text)
	if len(words) == 0 {
		return false
	}
	// Host commands already return a short response without calling the model.
	switch words[0] {
	case "/new", "/help", "/status", "/permission", "/resume", "/approve", "/deny", "/stop", "/pair":
		return true
	}
	return false
}

// Preserve the exact syntax consumed by control and channelRun. Text merely
// starting with /new or /help can still be a model prompt and needs its quote.
func hostCommand(text string) bool {
	if controlCommand(text) || strings.TrimSpace(text) == "/new" {
		return true
	}
	words := strings.Fields(text)
	if len(words) > 0 {
		switch words[0] {
		case "/status", "/resume", "/approve", "/deny", "/stop":
			return true
		}
	}
	return false
}

// sendReceipts runs after durable admission and before another provider poll,
// including while an earlier model run is active. Receipts never enter the model
// history or the answer's delivery state machine. A bounded batch keeps control
// commands and model dispatch responsive even when many messages arrive at once.
// stopped means the transport was retired (or its identity changed).
func (m *Manager) sendReceipts(ctx context.Context, i *instance, p transport, auth channelproto.Auth, cfg Config) (stopped bool, err error) {
	i.mu.Lock()
	wechat := i.manifest.ID == "wechat"
	i.mu.Unlock()
	if !wechat {
		return false, nil
	}
	receiptCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for range 8 {
		if receiptCtx.Err() != nil {
			return false, ctx.Err()
		}
		i.mu.Lock()
		current := i.cfg.Enabled && i.auth.Account == auth.Account && i.auth.Owner == auth.Owner && i.cfg.Workspace == cfg.Workspace && i.proc == p
		cfg = i.cfg
		i.mu.Unlock()
		if !current {
			return true, nil
		}
		var msg channelproto.Message
		var key string
		var waiting bool
		err = m.db.QueryRowContext(ctx, `SELECT id,conversation,sender,context,client_id,waiting FROM (
SELECT m.id,m.conversation,m.sender,m.context,r.client_id,0 AS waiting,m.rowid AS position FROM harness_channel_receipts r JOIN harness_channel_messages m USING(instance,account,id)
WHERE r.instance=?1 AND r.account=?2 AND r.status='pending' AND m.status='queued' AND m.workspace=?3
UNION ALL SELECT m.id,m.conversation,m.sender,m.context,m.client_id||'-wait',1,m.rowid FROM harness_channel_runs r JOIN harness_channel_messages m USING(instance,account,id)
WHERE r.instance=?1 AND r.account=?2 AND r.notice='pending' AND m.status='waiting_task' AND m.workspace=?3) ORDER BY position LIMIT 1`, i.id, auth.Account, cfg.Workspace).Scan(&msg.ID, &msg.Conversation, &msg.Sender, &msg.Context, &key, &waiting)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		state := "unknown"
		if !allowed(cfg, auth, msg.Sender) {
			state = "ignored"
		}
		// Claim before I/O. If the process dies or delivery is ambiguous, never
		// retry this optional receipt and never fence the actual user request.
		update := `UPDATE harness_channel_receipts SET status=? WHERE instance=? AND account=? AND id=? AND status=?`
		text := receiptText
		if waiting {
			update = `UPDATE harness_channel_runs SET notice=? WHERE instance=? AND account=? AND id=? AND notice=?`
			text = "正在等待后台任务，完成后会继续。"
		}
		claim, err := m.db.ExecContext(ctx, update, state, i.id, auth.Account, msg.ID, "pending")
		if err != nil {
			return false, err
		}
		n, err := claim.RowsAffected()
		if err != nil {
			return false, err
		}
		if n != 1 || state == "ignored" {
			continue
		}
		var ack struct {
			Sent bool `json:"sent"`
		}
		err = p.Call(receiptCtx, "send", channelproto.Send{Auth: auth, Message: msg, Key: key, Text: text, Options: cfg.Options}, &ack)
		if err != nil || !ack.Sent {
			i.mu.Lock()
			if i.proc == p {
				i.proc.Close()
				i.proc = nil
			}
			i.mu.Unlock()
			if errors.Is(err, channelproto.ErrAuthExpired) {
				return true, err
			}
			// Let the worker reconnect without the normal ten-second delivery
			// backoff. The prompt is still queued and can run normally.
			return true, ctx.Err()
		}
		persist, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		_, err = m.db.ExecContext(persist, update, "sent", i.id, auth.Account, msg.ID, "unknown")
		stop()
		if err != nil {
			return false, err
		}
	}
	return false, nil
}
