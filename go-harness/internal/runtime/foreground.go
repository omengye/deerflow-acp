package runtime

import (
	"context"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func (s *Store) hasBackgroundSessions(ctx context.Context) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='harness_background_specs')`).Scan(&exists)
	return exists, err
}

func (s *Store) requireForegroundSession(ctx context.Context, id string) error {
	exists, err := s.hasBackgroundSessions(ctx)
	if err != nil || !exists {
		return err
	}
	var child bool
	if err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_background_specs WHERE child_session_id=?)`, id).Scan(&child); err != nil {
		return err
	}
	if child {
		return harness.ErrNotFound
	}
	return nil
}
