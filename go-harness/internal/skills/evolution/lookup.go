package evolution

import (
	"context"
	"database/sql"
	"errors"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

// LookupRequest returns an already committed candidate for a host-issued tool
// identity. The caller must verify authorship and current model scope.
func (s *Service) LookupRequest(ctx context.Context, source, name, key string) (Proposal, error) {
	var result Proposal
	if key == "" || !bounded(key, 256) {
		return result, invalid("request key is invalid")
	}
	err := s.registry.WithPublication(ctx, source, name, func(tx *skills.PublicationTx) error {
		var id string
		if err := tx.SQL().QueryRowContext(ctx, `SELECT id FROM df_skill_proposals WHERE source_key=? AND name=? AND request_key=?`, tx.Target().SourceIdentity, name, key).Scan(&id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return harness.ErrNotFound
			}
			return err
		}
		var err error
		result, err = readProposal(ctx, tx.SQL(), id)
		if err == nil && result.Target != tx.Target() {
			return harness.ErrPermissionDenied
		}
		return err
	})
	return result, err
}
