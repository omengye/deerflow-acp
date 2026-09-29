package agent

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
)

const listArtifactsMethod = "_deerflow/artifacts/list"

type promptRequest struct {
	SessionID string
	Prompt    []harness.Content
}

// AssetRef is an internal SDK capability. ACP clients supply supported content
// blocks; arbitrary durable references and unrecognized payloads are rejected.
func decodePrompt(raw json.RawMessage) (promptRequest, error) {
	invalid := func() (promptRequest, error) {
		return promptRequest{}, rpcError(protocol.InvalidParams, "Invalid prompt content")
	}
	if rejectDuplicateKeys(raw) != nil {
		return invalid()
	}
	fields, err := strictObject(raw, "sessionId", "prompt", "_meta")
	if err != nil {
		return invalid()
	}
	var req promptRequest
	if json.Unmarshal(fields["sessionId"], &req.SessionID) != nil || !validID(req.SessionID, 256) {
		return invalid()
	}
	var parts []json.RawMessage
	prompt := bytes.TrimSpace(fields["prompt"])
	if len(prompt) == 0 || prompt[0] != '[' || json.Unmarshal(prompt, &parts) != nil || len(parts) == 0 || len(parts) > 1024 {
		return invalid()
	}
	for _, part := range parts {
		var header struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(part, &header) != nil {
			return invalid()
		}
		allowed := []string{"type", "annotations", "_meta"}
		switch header.Type {
		case "text":
			allowed = append(allowed, "text")
		case "image":
			allowed = append(allowed, "data", "mimeType", "uri", "name")
		case "resource_link":
			allowed = append(allowed, "uri", "name", "mimeType", "size", "description")
		default:
			return invalid()
		}
		block, err := strictObject(part, allowed...)
		if err != nil {
			return invalid()
		}
		var c harness.Content
		if json.Unmarshal(part, &c) != nil {
			return invalid()
		}
		switch c.Type {
		case "text":
			if _, ok := block["text"]; !ok {
				return invalid()
			}
		case "image":
			if c.Data == "" || c.MimeType == "" || c.URI != "" {
				return invalid()
			}
		case "resource_link":
			if c.URI == "" || c.Name == "" {
				return invalid()
			}
		}
		req.Prompt = append(req.Prompt, c)
	}
	return req, nil
}

// Only ACP fields are emitted. Internal asset metadata never enters wire
// history, and each image is hydrated separately to remain below frame limits.
func (a *Agent) wireContent(ctx context.Context, sessionID string, c harness.Content) (map[string]any, error) {
	c, err := a.service.ProjectContent(ctx, sessionID, c)
	if err != nil {
		return nil, err
	}
	switch c.Type {
	case "text":
		return map[string]any{"type": "text", "text": c.Text}, nil
	case "image":
		return map[string]any{"type": "image", "data": c.Data, "mimeType": c.MimeType}, nil
	case "resource_link":
		m := map[string]any{"type": "resource_link", "uri": c.URI, "name": c.Name}
		if c.MimeType != "" {
			m["mimeType"] = c.MimeType
		}
		if c.Size != nil {
			m["size"] = *c.Size
		}
		if c.Description != "" {
			m["description"] = c.Description
		}
		return m, nil
	default:
		return nil, harness.ErrInvalidInput
	}
}

func (a *Agent) artifactRequest(ctx context.Context, raw json.RawMessage) (any, error) {
	invalid := func() (any, error) { return nil, rpcError(protocol.InvalidParams, "Invalid artifact parameters") }
	if len(raw) > 4096 || rejectDuplicateKeys(raw) != nil {
		return invalid()
	}
	fields, err := strictObject(raw, "sessionId")
	if err != nil {
		return invalid()
	}
	var sessionID string
	if json.Unmarshal(fields["sessionId"], &sessionID) != nil || !validID(sessionID, 256) {
		return invalid()
	}
	// Keep the ownership lease for both listing and projection.
	ctx, release, err := a.service.Coordinator.Begin(ctx, sessionID, a.owner)
	if err != nil {
		return nil, err
	}
	defer release()
	items := make([]any, 0)
	if a.service.Assets == nil {
		return map[string]any{"artifacts": items}, nil
	}
	contents, err := a.service.Assets.ListArtifacts(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	for _, c := range contents {
		item, err := a.wireContent(ctx, sessionID, c)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return map[string]any{"artifacts": items}, nil
}
