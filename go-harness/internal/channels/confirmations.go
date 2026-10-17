package channels

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func (m *Manager) checkDispatch(d Dispatch) (*instance, error) {
	i, err := m.get(d.Instance)
	if err != nil {
		return nil, err
	}
	i.mu.Lock()
	if !i.cfg.Enabled || i.auth.Account != d.Account || i.auth.Owner != d.Sender || i.cfg.Workspace != d.Workspace || len(i.auth.Secret) == 0 || i.revision != d.Revision {
		i.mu.Unlock()
		return nil, userError("渠道授权已变化，请重新发送 /status")
	}
	return i, nil // caller releases mu after its transaction
}

func (m *Manager) Confirmation(ctx context.Context, d Dispatch, state harness.ExecutionState, p harness.ExecutionInteraction) (string, error) {
	i, err := m.checkDispatch(d)
	if err != nil {
		return "", err
	}
	defer i.mu.Unlock()
	var code string
	now := time.Now().UnixMilli()
	var decided bool
	if err = m.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_channel_confirmations WHERE scope=? AND session_id=? AND run_id=? AND execution_version=? AND intent_id=? AND intent_version=? AND digest=? AND config_version=? AND policy_revision=? AND expires_ms>? AND used=0 AND decision IN ('allow_once','reject_once'))`, d.scope(), state.SessionID, state.RunID, state.Version, p.ID, p.Version, p.ArgumentsDigest, p.ConfigVersion, d.Revision, now).Scan(&decided); err != nil {
		return "", err
	}
	if decided {
		return "", nil
	}
	err = m.db.QueryRowContext(ctx, `SELECT code FROM harness_channel_confirmations WHERE scope=? AND session_id=? AND run_id=? AND execution_version=? AND intent_id=? AND intent_version=? AND digest=? AND config_version=? AND policy_revision=? AND expires_ms>? AND used=0 AND decision=''`, d.scope(), state.SessionID, state.RunID, state.Version, p.ID, p.Version, p.ArgumentsDigest, p.ConfigVersion, d.Revision, now).Scan(&code)
	if err == nil {
		return code, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	code = rand.Text()[:12]
	_, err = m.db.ExecContext(ctx, `INSERT INTO harness_channel_confirmations(code,scope,session_id,run_id,execution_version,intent_id,intent_version,digest,config_version,policy_revision,expires_ms) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, code, d.scope(), state.SessionID, state.RunID, state.Version, p.ID, p.Version, p.ArgumentsDigest, p.ConfigVersion, d.Revision, time.Now().Add(10*time.Minute).UnixMilli())
	return code, err
}
func (m *Manager) Decide(ctx context.Context, d Dispatch, state harness.ExecutionState, code string, decision harness.PermissionDecision) error {
	if decision != harness.AllowOnce && decision != harness.RejectOnce {
		return harness.ErrInvalidInput
	}
	i, err := m.checkDispatch(d)
	if err != nil {
		return err
	}
	defer i.mu.Unlock()
	r, err := m.db.ExecContext(ctx, `UPDATE harness_channel_confirmations SET decision=? WHERE code=? AND scope=? AND session_id=? AND run_id=? AND execution_version=? AND policy_revision=? AND used=0 AND decision='' AND expires_ms>?`, decision, code, d.scope(), state.SessionID, state.RunID, state.Version, d.Revision, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n != 1 {
		return userError("确认码无效、已使用或已过期。请发送 /status 获取当前任务确认码。")
	}
	return err
}

// TakeDecisions consumes only a complete matching set before any resume effect.
// A crash never reuses confirmation authority; /status can issue fresh codes.
func (m *Manager) TakeDecisions(ctx context.Context, d Dispatch, state harness.ExecutionState, required []harness.ExecutionInteraction) (map[string]harness.PermissionDecision, bool, error) {
	i, err := m.checkDispatch(d)
	if err != nil {
		return nil, false, err
	}
	defer i.mu.Unlock()
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	result := map[string]harness.PermissionDecision{}
	codes := []string{}
	for _, p := range required {
		var code string
		var decision harness.PermissionDecision
		err = tx.QueryRowContext(ctx, `SELECT code,decision FROM harness_channel_confirmations WHERE scope=? AND session_id=? AND run_id=? AND execution_version=? AND intent_id=? AND intent_version=? AND digest=? AND config_version=? AND policy_revision=? AND expires_ms>? AND used=0 AND decision IN ('allow_once','reject_once') ORDER BY rowid DESC LIMIT 1`, d.scope(), state.SessionID, state.RunID, state.Version, p.ID, p.Version, p.ArgumentsDigest, p.ConfigVersion, d.Revision, time.Now().UnixMilli()).Scan(&code, &decision)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		result[p.ID] = decision
		codes = append(codes, code)
	}
	for _, code := range codes {
		if _, err = tx.ExecContext(ctx, `UPDATE harness_channel_confirmations SET used=1 WHERE code=? AND used=0`, code); err != nil {
			return nil, false, err
		}
	}
	err = tx.Commit()
	return result, err == nil, err
}
