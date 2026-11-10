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
		err = m.db.QueryRowContext(ctx, `SELECT m.id,m.conversation,m.sender,m.context,r.client_id FROM harness_channel_receipts r JOIN harness_channel_messages m ON m.instance=r.instance AND m.account=r.account AND m.id=r.id WHERE r.instance=? AND r.account=? AND r.status='pending' AND m.status='queued' AND m.workspace=? ORDER BY m.rowid LIMIT 1`, i.id, auth.Account, cfg.Workspace).Scan(&msg.ID, &msg.Conversation, &msg.Sender, &msg.Context, &key)
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
		claim, err := m.db.ExecContext(ctx, `UPDATE harness_channel_receipts SET status=? WHERE instance=? AND account=? AND id=? AND status='pending'`, state, i.id, auth.Account, msg.ID)
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
		err = p.Call(receiptCtx, "send", channelproto.Send{Auth: auth, Message: msg, Key: key, Text: receiptText, Options: cfg.Options}, &ack)
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
		_, err = m.db.ExecContext(persist, `UPDATE harness_channel_receipts SET status='sent' WHERE instance=? AND account=? AND id=? AND status='unknown'`, i.id, auth.Account, msg.ID)
		stop()
		if err != nil {
			return false, err
		}
	}
	return false, nil
}
