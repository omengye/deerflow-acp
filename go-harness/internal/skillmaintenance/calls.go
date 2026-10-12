package skillmaintenance

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"time"
)

type claimBroker struct {
	store *Store
	claim Claim
}

func (s *Store) Broker(c Claim) Broker { return &claimBroker{store: s, claim: c} }
func (b *claimBroker) Reserve(ctx context.Context, r ModelCall) (Receipt, error) {
	return b.store.reserve(ctx, b.claim, r)
}
func (b *claimBroker) Settle(ctx context.Context, r Receipt, u Usage) error {
	return b.store.settle(ctx, b.claim, r, u)
}

// The full estimate plus output cap is charged in the admission transaction.
// A crash, timeout, cancellation or invalid usage cannot refund that charge.
func (s *Store) reserve(ctx context.Context, c Claim, r ModelCall) (Receipt, error) {
	switch r.Stage {
	case "generation", "security", "quality", "behavior", "experience_extract", "experience_consolidate", "skill_match":
	default:
		return Receipt{}, ErrInvalid
	}
	if !digest(r.ModelIdentity) || !digest(r.InputDigest) || r.InputTokens < 1 || r.InputTokens > 8<<20 || r.MaxOutputTokens < 1 || r.MaxOutputTokens > 131072 {
		return Receipt{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Receipt{}, err
	}
	defer tx.Rollback()
	now, err := s.clock(ctx, tx)
	if err != nil {
		return Receipt{}, err
	}
	j, err := s.check(ctx, tx, c, now)
	if err != nil {
		return Receipt{}, err
	}
	if !stageAllowed(j.Request.Kind, r.Stage) {
		return Receipt{}, ErrInvalid
	}
	if s.admit != nil {
		if err = s.admit(ctx, tx, j); err != nil {
			return Receipt{}, err
		}
	}
	if s.callAdmission != nil {
		if err = s.callAdmission(ctx, tx, j, r, now); err != nil {
			return Receipt{}, err
		}
	}
	var active int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM df_skill_maintenance_calls WHERE job_id=? AND claim_token=? AND state='reserved'`, j.ID, c.token).Scan(&active); err != nil {
		return Receipt{}, err
	}
	if active != 0 {
		return Receipt{}, ErrConflict
	}
	day := now.Format("2006-01-02")
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO df_skill_maintenance_days(day,model_calls,charged_tokens) VALUES(?,0,0)`, day)
	if err != nil {
		return Receipt{}, err
	}
	charge := r.InputTokens + r.MaxOutputTokens
	result, err := tx.ExecContext(ctx, `UPDATE df_skill_maintenance_days SET model_calls=model_calls+1,charged_tokens=charged_tokens+? WHERE day=? AND model_calls<? AND charged_tokens<=?`, charge, day, min(j.Policy.MaxDailyModelCalls, s.policy.MaxDailyModelCalls), min(j.Policy.MaxDailyTokens, s.policy.MaxDailyTokens)-charge)
	if err != nil {
		return Receipt{}, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return Receipt{}, err
	}
	if n != 1 {
		return Receipt{}, ErrBudget
	}
	receipt := Receipt{ID: "smc_" + rand.Text(), JobID: j.ID, Attempt: j.Attempts, Day: day, Stage: r.Stage, ModelIdentity: r.ModelIdentity, InputDigest: r.InputDigest, ReservedTokens: charge, ChargedTokens: charge, State: "reserved", Estimated: true}
	result, err = tx.ExecContext(ctx, `INSERT INTO df_skill_maintenance_calls(id,job_id,attempt,claim_token,day,stage,model_identity,input_digest,reserved_tokens,charged_tokens,state) VALUES(?,?,?,?,?,?,?,?,?,?,'reserved')`, receipt.ID, j.ID, j.Attempts, c.token, day, r.Stage, r.ModelIdentity, r.InputDigest, charge, charge)
	if err != nil {
		return Receipt{}, err
	}
	receipt.Sequence, err = result.LastInsertId()
	if err != nil {
		return Receipt{}, err
	}
	return receipt, tx.Commit()
}

func stageAllowed(kind JobKind, stage string) bool {
	switch kind {
	case SkillGeneration:
		return stage == "generation" || stage == "security" || stage == "quality" || stage == "behavior"
	case SkillBehavior:
		return stage == "security" || stage == "quality" || stage == "behavior"
	case ExperienceExtract, ExperienceConsolidate, SkillMatch:
		return stage == string(kind)
	default:
		return false
	}
}

const receiptColumns = `sequence,id,job_id,attempt,day,stage,model_identity,input_digest,reserved_tokens,charged_tokens,input_tokens,output_tokens,state,estimated`

type scanner interface{ Scan(...any) error }

func scanReceipt(row scanner) (Receipt, error) {
	var r Receipt
	err := row.Scan(&r.Sequence, &r.ID, &r.JobID, &r.Attempt, &r.Day, &r.Stage, &r.ModelIdentity, &r.InputDigest, &r.ReservedTokens, &r.ChargedTokens, &r.InputTokens, &r.OutputTokens, &r.State, &r.Estimated)
	return r, err
}

func (s *Store) settle(ctx context.Context, c Claim, r Receipt, u Usage) error {
	if u.InputTokens < 0 || u.OutputTokens < 0 || u.InputTokens > 1<<40 || u.OutputTokens > 1<<40 || !u.Known && (u.InputTokens != 0 || u.OutputTokens != 0) {
		return ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now, err := s.clock(ctx, tx)
	if err != nil {
		return err
	}
	if _, err = s.check(ctx, tx, c, now); err != nil {
		return err
	}
	stored, err := scanReceipt(tx.QueryRowContext(ctx, `SELECT `+receiptColumns+` FROM df_skill_maintenance_calls WHERE id=?`, r.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	var token, prior string
	if err = tx.QueryRowContext(ctx, `SELECT claim_token,settlement_digest FROM df_skill_maintenance_calls WHERE id=?`, r.ID).Scan(&token, &prior); err != nil {
		return err
	}
	if stored.JobID != c.Job.ID || stored.Attempt != c.Job.Attempts || token != c.token || r.JobID != stored.JobID || r.Sequence != stored.Sequence || r.Attempt != stored.Attempt || r.Day != stored.Day || r.Stage != stored.Stage || r.ModelIdentity != stored.ModelIdentity || r.InputDigest != stored.InputDigest || r.ReservedTokens != stored.ReservedTokens {
		return ErrConflict
	}
	digest := hash(u)
	if stored.State == "settled" {
		if prior != digest {
			return ErrConflict
		}
		return tx.Commit()
	}
	if stored.State != "reserved" {
		return ErrConflict
	}
	charged := stored.ReservedTokens
	if u.Known {
		charged = u.InputTokens + u.OutputTokens
	}
	// Attribute late settlement to the reservation's UTC day, even across midnight.
	_, err = tx.ExecContext(ctx, `UPDATE df_skill_maintenance_days SET charged_tokens=charged_tokens+? WHERE day=?`, charged-stored.ChargedTokens, stored.Day)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE df_skill_maintenance_calls SET state='settled',charged_tokens=?,input_tokens=?,output_tokens=?,estimated=?,settlement_digest=? WHERE id=?`, charged, u.InputTokens, u.OutputTokens, !u.Known, digest, r.ID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Receipts is a bounded sequence inventory. A source-authorizing service must
// check the job's source identity before exposing it to an operator or model.
func (s *Store) Receipts(ctx context.Context, id string, after int64, limit int) ([]Receipt, error) {
	return ReadReceipts(ctx, s.db, id, after, limit)
}

func ReadReceipts(ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, id string, after int64, limit int) ([]Receipt, error) {
	if after < 0 || limit < 1 || limit > 100 {
		return nil, ErrInvalid
	}
	rows, err := db.QueryContext(ctx, `SELECT `+receiptColumns+` FROM df_skill_maintenance_calls WHERE job_id=? AND sequence>? ORDER BY sequence LIMIT ?`, id, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Receipt, 0)
	for rows.Next() {
		r, err := scanReceipt(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

func (s *Store) DayUsage(ctx context.Context, day string) (DayUsage, error) {
	result := DayUsage{Day: day}
	if parsed, err := time.Parse("2006-01-02", day); err != nil || parsed.Format("2006-01-02") != day {
		return result, ErrInvalid
	}
	err := s.db.QueryRowContext(ctx, `SELECT model_calls,charged_tokens FROM df_skill_maintenance_days WHERE day=?`, day).Scan(&result.ModelCalls, &result.ChargedTokens)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return result, err
}
