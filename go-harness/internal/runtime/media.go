package runtime

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

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
