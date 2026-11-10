package evolution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/omengye/deerflow-acp/go-harness/internal/learning"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

// Persist the paid response before staging objects. Resource pressure and a
// process restart must not turn one generation into repeated provider calls.
func (s *Service) generatedOutput(ctx context.Context, job string) (skillmaintenance.Candidate, bool, error) {
	var candidate skillmaintenance.Candidate
	var raw []byte
	var sha string
	err := s.db.QueryRowContext(ctx, `SELECT payload,payload_sha FROM df_skill_generation_outputs WHERE job_id=?`, job).Scan(&raw, &sha)
	if errors.Is(err, sql.ErrNoRows) {
		return candidate, false, nil
	}
	if err != nil {
		return candidate, false, err
	}
	if len(raw) > 1<<20 || hash(raw) != sha || json.Unmarshal(raw, &candidate) != nil {
		return candidate, false, invalid("generated output integrity failure")
	}
	return candidate, true, nil
}

func (s *Service) persistGeneratedOutput(ctx context.Context, job string, c skillmaintenance.Candidate) error {
	if err := validateFiles(c.Files); err != nil {
		return err
	}
	r, err := readGeneration(ctx, s.db, job)
	if err != nil {
		return err
	}
	pkg, err := s.registry.PrepareCandidate(r.input.Target.Name, c.Files)
	if err != nil {
		return err
	}
	if err = validateCandidateCredentials(pkg, r.input.DiscoveryID != ""); err != nil {
		return err
	}
	c.Reason, err = learning.RedactPromotionReason(c.Reason)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(c)
	if err != nil || len(raw) > 1<<20 {
		return invalid("generated output exceeds storage bounds")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = skillmaintenance.GuardTx(ctx, tx, job); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO df_skill_generation_outputs VALUES(?,?,?)`, job, raw, hash(raw)); err != nil {
		return err
	}
	return tx.Commit()
}
