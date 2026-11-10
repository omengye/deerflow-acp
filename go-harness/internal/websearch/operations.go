package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"
)

// ConfigureJSON is shared by the installer configuration CLI and desktop bridge.
// Credentials are accepted only by the host configuration path, never a model.
func ConfigureJSON(manager *Manager, operation string, body json.RawMessage) (Status, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if operation == "search-status" {
		return manager.Status()
	}
	if operation == "search-select" {
		var request struct {
			Provider string `json:"provider"`
		}
		if err := decode(body, &request); err != nil {
			return Status{}, err
		}
		return manager.Select(ctx, request.Provider)
	}
	var request Update
	if err := decode(body, &request); err != nil {
		return Status{}, err
	}
	return manager.Update(ctx, request)
}

func decode(body []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("invalid search configuration request")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("invalid search configuration request")
	}
	return nil
}
