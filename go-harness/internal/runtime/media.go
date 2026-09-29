package runtime

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/assets"
)

func (s *Store) appendModelImageEvent(ctx context.Context, e harness.RunEvent, store *assets.Store) (saved harness.RunEvent, returnErr error) {
	if len(e.Content) != 1 || e.SessionID == "" || e.RunID == "" {
		return e, harness.ErrInvalidInput
	}
	prepared := store.TakeModelImage(e.SessionID, e.RunID, e.Content[0])
	if prepared == nil {
		return e, harness.ErrInvalidInput
	}
	committed := false
	defer func() { returnErr = errors.Join(returnErr, prepared.Finish(committed)) }()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return e, err
	}
	defer tx.Rollback()
	var sessionID, status string
	if err = tx.QueryRowContext(ctx, `SELECT session_id,status FROM harness_runs WHERE id=?`, e.RunID).Scan(&sessionID, &status); err != nil {
		return e, err
	}
	if sessionID != e.SessionID || status != "running" {
		return e, harness.ErrReceiptConflict
	}
	if attempt, ok := ctx.Value(executionAttemptKey{}).(*executionAttempt); ok {
		if _, err = checkExecutionLease(ctx, tx, attempt.lease); err != nil {
			return e, err
		}
	}
	if attempt, ok := ctx.Value(backgroundInteractionAttemptKey{}).(*BackgroundInteractionAttempt); ok {
		if err = attempt.check(ctx, tx, true); err != nil {
			return e, err
		}
	}
	if err = prepared.AttachModelImage(ctx, tx); err != nil {
		return e, err
	}
	saved, err = appendEventTx(ctx, tx, e)
	if err != nil {
		return e, err
	}
	if err = tx.Commit(); err != nil {
		return e, err
	}
	committed = true
	return saved, nil
}

func (s *Service) ImageInputEnabled() bool {
	if s.Assets == nil {
		return false
	}
	for _, model := range s.modelOptions() {
		if s.Media.SupportsVision(model.Value) {
			return true
		}
	}
	return false
}

// ProjectContent hydrates one authorized event block only for transport. Stored
// inputs/history and the engine's durable messages keep their AssetRef instead.
func (s *Service) ProjectContent(ctx context.Context, sessionID string, c harness.Content) (harness.Content, error) {
	if c.Asset == nil {
		return c, nil
	}
	if s.Assets == nil {
		return harness.Content{}, fmt.Errorf("asset storage is not configured")
	}
	switch c.Type {
	case "image":
		if c.Asset.Kind != harness.AssetImage {
			return harness.Content{}, harness.ErrInvalidInput
		}
		data, err := s.Assets.Resolve(ctx, sessionID, *c.Asset)
		if err != nil {
			return harness.Content{}, err
		}
		c.Data = base64.StdEncoding.EncodeToString(data)
		c.URI = ""
	case "resource_link":
		uri, err := s.Assets.LocalURI(ctx, sessionID, *c.Asset)
		if err != nil {
			return harness.Content{}, err
		}
		c.URI = uri
	default:
		return harness.Content{}, harness.ErrInvalidInput
	}
	c.Asset = nil
	return c, nil
}

func (s *Service) ListArtifacts(ctx context.Context, owner, sessionID string) ([]harness.Content, error) {
	ctx, release, err := s.Coordinator.Begin(ctx, sessionID, owner)
	if err != nil {
		return nil, err
	}
	defer release()
	if s.Assets == nil {
		return []harness.Content{}, nil
	}
	return s.Assets.ListArtifacts(ctx, sessionID)
}

func (s *Service) ResolveAsset(ctx context.Context, owner, sessionID string, ref harness.AssetRef) ([]byte, error) {
	ctx, release, err := s.Coordinator.Begin(ctx, sessionID, owner)
	if err != nil {
		return nil, err
	}
	defer release()
	if s.Assets == nil {
		return nil, harness.ErrNotFound
	}
	return s.Assets.Resolve(ctx, sessionID, ref)
}
