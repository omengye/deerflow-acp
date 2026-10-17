package channels

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/omengye/deerflow-acp/go-harness/channelproto"
)

// MigrateWeChat copies legacy state once. Old tables remain intact for backup
// and diagnostics; the old worker must never be started alongside this manager.
func (m *Manager) MigrateWeChat(ctx context.Context, cfg Config) error {
	i, err := m.get("wechat")
	if err != nil {
		if cfg.Enabled {
			return errors.New("enabled legacy WeChat requires the bundled channel plugin")
		}
		return nil
	}
	_, err = m.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS harness_channel_migrations(name TEXT PRIMARY KEY)`)
	if err != nil {
		return err
	}
	var exists bool
	if err = m.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_channel_migrations WHERE name='wechat-v1')`).Scan(&exists); err != nil || exists {
		return err
	}
	if err = m.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='harness_wechat_auth')`).Scan(&exists); err != nil {
		return err
	}
	if !exists && cfg.Workspace == "" {
		_, err = m.db.ExecContext(ctx, `INSERT INTO harness_channel_migrations VALUES('wechat-v1')`)
		return err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.auth = channelproto.Auth{}
	if cfg.LegacyApproval == "" && len(cfg.LegacyAllowed) > 0 {
		cfg.LegacyApproval = "read_only"
	}
	if cfg.Enabled && len(cfg.LegacyAllowed) == 0 {
		return errors.New("legacy enabled WeChat requires its original sender policy")
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var auth []byte
	var cursor string
	if exists {
		err = tx.QueryRowContext(ctx, `SELECT credentials,cursor FROM harness_wechat_auth WHERE id=1`).Scan(&auth, &cursor)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if len(auth) > 0 {
			plain, e := openCredentials(auth)
			if e != nil {
				return e
			}
			var old struct {
				Account string `json:"ilink_bot_id"`
			}
			if e = json.Unmarshal(plain, &old); e != nil {
				return e
			}
			i.auth.Account = old.Account
			i.auth.Secret = plain
			payload, _ := json.Marshal(i.auth)
			auth, err = sealCredentials(payload)
			if err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO harness_channel_threads SELECT 'wechat',bot,sender,sender,?,session_id FROM harness_wechat_threads`, cfg.Workspace); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO harness_channel_messages(instance,account,id,conversation,sender,context,text,status,reply,session_id,client_id,workspace,created_ms) SELECT 'wechat',bot,id,sender,sender,context_token,text,CASE WHEN status IN ('running','sending') THEN 'unknown' ELSE status END,reply,session_id,client_id,?,created_ms FROM harness_wechat_messages`, cfg.Workspace); err != nil {
			return err
		}
	}
	i.cfg = cfg
	i.cursor = cursor
	raw, _ := json.Marshal(cfg)
	if _, err = tx.ExecContext(ctx, `UPDATE harness_channels SET config=?,auth=?,cursor=? WHERE id='wechat'`, raw, auth, cursor); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO harness_channel_migrations VALUES('wechat-v1')`); err != nil {
		return err
	}
	return tx.Commit()
}
