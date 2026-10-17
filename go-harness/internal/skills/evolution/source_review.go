package evolution

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/internal/learning"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

// ReviewSource makes a NEW candidate from a blocked create's configured
// directory. It never reuses approval/evaluation for different bytes.
func (s *Service) ReviewSource(ctx context.Context, id string, expected int64, key string) (Proposal, error) {
	if !validManagementKey(key) {
		return Proposal{}, invalid("source review requires a request key")
	}
	var request Request
	var prior Proposal
	err := s.withProposal(ctx, id, func(tx *skills.PublicationTx, p *Proposal) error {
		if p.Action != "create" || p.StateVersion != expected || p.Status != "stale" && p.Status != "failed" && p.Status != "rejected" {
			return ErrConflict
		}
		request = Request{SourceID: p.Target.SourceID, Name: p.SkillName, Action: "create", RequestKey: "source-review/" + id + "/" + key, Reason: p.Reason, Author: "desktop", Origin: "manual_agent", Trigger: Trigger{Type: "operator_source_review", Summary: "Review current source for " + id}}
		var priorID string
		err := tx.SQL().QueryRowContext(ctx, `SELECT id FROM df_skill_proposals WHERE source_key=? AND name=? AND request_key=?`, p.Target.SourceIdentity, p.SkillName, request.RequestKey).Scan(&priorID)
		if err == nil {
			prior, err = readProposal(ctx, tx.SQL(), priorID)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		head, err := tx.Current()
		if err != nil {
			return err
		}
		if head != p.Base || head.Exists {
			return ErrConflict
		}
		candidate, err := tx.SourceCandidate()
		if err != nil {
			return err
		}
		request.Base, request.Files = head, candidate.Files()
		// Local configuration may contain credentials. Never send those bytes
		// to review models or include them in a newly generated diff.
		for _, content := range request.Files {
			if learning.ValidatePromotionInstructions(string(content)) != nil {
				return &skills.PublicationConflict{Code: "source_credentials_require_local_handling"}
			}
		}
		return nil
	})
	if err != nil || prior.ID != "" {
		return prior, err
	}
	return s.Submit(ctx, request)
}

func validManagementKey(key string) bool {
	return key != "" && strings.TrimSpace(key) == key && bounded(key, 128)
}
