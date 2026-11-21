package channels

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/channelproto"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/automation"
)

type TaskWait struct {
	SessionID, RunID, Approval string
	StopRequested              bool
}

func (m *Manager) TracksTaskWaits(id string) bool {
	i, err := m.get(id)
	if err != nil {
		return false
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return m.Continue != nil && i.manifest.ID == "wechat"
}

func BindRunTx(ctx context.Context, tx *sql.Tx, d Dispatch, sessionID, runID string) error {
	// A permission command may resume an older run. Bind its reply only if
	// the original message has already settled, and only in the same thread.
	var foreign, otherWaiting bool
	err := tx.QueryRowContext(ctx, `SELECT coalesce(max(r.session_id<>? OR m.instance<>? OR m.account<>? OR m.conversation<>? OR m.sender<>? OR m.workspace<>?),0),
coalesce(max(m.id<>? AND m.status='waiting_task'),0) FROM harness_channel_runs r JOIN harness_channel_messages m USING(instance,account,id) WHERE r.run_id=?`,
		sessionID, d.Instance, d.Account, d.Conversation, d.Sender, d.Workspace, d.MessageID, runID).Scan(&foreign, &otherWaiting)
	if err != nil {
		return err
	}
	if foreign {
		return harness.ErrExecutionConflict
	}
	if otherWaiting {
		return nil
	}
	if _, err = tx.ExecContext(ctx, `UPDATE harness_channel_messages SET session_id=? WHERE instance=? AND account=? AND id=? AND session_id='' AND status='running' AND
EXISTS(SELECT 1 FROM harness_channel_threads WHERE instance=? AND account=? AND conversation=? AND sender=? AND workspace=? AND session_id=?)`,
		sessionID, d.Instance, d.Account, d.MessageID, d.Instance, d.Account, d.Conversation, d.Sender, d.Workspace, sessionID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO harness_channel_runs(run_id,session_id,instance,account,id)
SELECT ?,session_id,instance,account,id FROM harness_channel_messages WHERE instance=? AND account=? AND id=? AND session_id=? AND conversation=? AND sender=? AND workspace=? AND status='running'
ON CONFLICT(instance,account,id) DO UPDATE SET id=excluded.id WHERE run_id=excluded.run_id AND session_id=excluded.session_id`, runID, d.Instance, d.Account, d.MessageID, sessionID, d.Conversation, d.Sender, d.Workspace)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return errors.Join(harness.ErrExecutionConflict, err)
	}
	return nil
}

func (m *Manager) HasPendingRun(ctx context.Context, d Dispatch, runID string) (bool, error) {
	var pending bool
	err := m.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_channel_runs r JOIN harness_channel_messages m USING(instance,account,id) WHERE r.run_id=? AND m.instance=? AND m.account=? AND m.conversation=? AND m.sender=? AND m.workspace=? AND m.status IN ('running','waiting_task'))`, runID, d.Instance, d.Account, d.Conversation, d.Sender, d.Workspace).Scan(&pending)
	return pending, err
}

// The original inbox row owns the final reply and its idempotency key. Only
// a waiting task is polled; an ambiguous outbound send still requires review.
func (m *Manager) continueWaiting(ctx context.Context, i *instance, auth channelproto.Auth, cfg Config, revision int64) (bool, error) {
	var msg channelproto.Message
	var wait TaskWait
	var admitted string
	now := time.Now()
	err := m.db.QueryRowContext(ctx, `SELECT m.id,m.conversation,m.sender,r.session_id,r.run_id,r.stop_requested,COALESCE(a.approval,'global') FROM harness_channel_messages m
JOIN harness_channel_runs r USING(instance,account,id) LEFT JOIN harness_channel_admissions a USING(instance,account,id)
WHERE m.instance=? AND m.account=? AND m.workspace=? AND m.status='waiting_task' AND r.next_check_ms<=? ORDER BY r.next_check_ms,m.rowid LIMIT 1`, i.id, auth.Account, cfg.Workspace, now.UnixMilli()).Scan(&msg.ID, &msg.Conversation, &msg.Sender, &wait.SessionID, &wait.RunID, &wait.StopRequested, &admitted)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !allowed(cfg, auth, msg.Sender) {
		_, err = m.db.ExecContext(ctx, `UPDATE harness_channel_messages SET status='unknown' WHERE instance=? AND account=? AND id=? AND status='waiting_task'`, i.id, auth.Account, msg.ID)
		return true, err
	}
	if _, err = m.db.ExecContext(ctx, `UPDATE harness_channel_runs SET next_check_ms=? WHERE instance=? AND account=? AND id=?`, now.Add(3*time.Second).UnixMilli(), i.id, auth.Account, msg.ID); err != nil {
		return false, err
	}
	d := Dispatch{i.id, auth.Account, msg.Conversation, msg.Sender, cfg.Workspace, msg.ID, revision}
	runCtx, active := newActiveDispatch(WithDispatch(ctx, d), auth, cfg, msg)
	active.waitSession = wait.SessionID
	wait.Approval = dispatchApproval(admitted, effectiveApproval(cfg, auth))
	i.mu.Lock()
	if i.active != nil || i.revision != revision || i.auth.Account != auth.Account || i.auth.Owner != auth.Owner {
		i.mu.Unlock()
		active.cancel()
		return false, nil
	}
	i.active = active
	i.mu.Unlock()
	go func() {
		defer m.wakeWorker(i)
		defer close(active.done)
		defer active.cancel()
		out, err := m.Continue(runCtx, d, wait)
		if err == nil && out != nil {
			err = m.finishWaiting(ctx, d, wait.SessionID, out)
		}
		if err != nil && ctx.Err() == nil {
			i.mu.Lock()
			i.lastError = "暂时无法读取后台任务，正在重试。"
			i.mu.Unlock()
		}
	}()
	return true, nil
}

func (m *Manager) finishWaiting(ctx context.Context, d Dispatch, sessionID string, out *automation.Outcome) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	reply := boundedText(out.Text)
	change, err := tx.ExecContext(ctx, `UPDATE harness_channel_messages SET status='ready',reply=? WHERE instance=? AND account=? AND id=? AND session_id=? AND status='waiting_task'`, reply, d.Instance, d.Account, d.MessageID, sessionID)
	if err != nil {
		return err
	}
	if n, err := change.RowsAffected(); err != nil || n == 0 {
		return err
	}
	if err = saveImagesTx(ctx, tx, d.Instance, d.Account, d.MessageID, out.Images); err != nil {
		return err
	}
	return tx.Commit()
}
