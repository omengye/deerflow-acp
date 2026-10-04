package agents

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// Catalog pins the complete delegation graph, including roles not yet invoked.
// Only host-owned definitions and model references are stored; provider routes
// and credentials continue to come from the current host configuration.
type Catalog struct {
	Version int                     `json:"version"`
	Config  harness.SubagentsConfig `json:"config"`
	Digest  string                  `json:"digest"`
}

func Snapshot(config *harness.SubagentsConfig) (*Catalog, error) {
	if config == nil {
		return nil, nil
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	if len(raw) > 1<<20 {
		return nil, fmt.Errorf("%w: subagent definitions exceed 1 MiB", harness.ErrInvalidInput)
	}
	c := &Catalog{Version: 1, Digest: fmt.Sprintf("%x", sha256.Sum256(raw))}
	if err = json.Unmarshal(raw, &c.Config); err != nil {
		return nil, err
	}
	return c, c.Validate()
}

func (c *Catalog) Validate() error {
	if c == nil || c.Version != 1 {
		return fmt.Errorf("%w: unsupported role catalog", harness.ErrInvalidInput)
	}
	if err := c.Config.Validate(nil); err != nil {
		return err
	}
	raw, err := json.Marshal(c.Config)
	if err != nil {
		return err
	}
	if len(raw) > 1<<20 || c.Digest != fmt.Sprintf("%x", sha256.Sum256(raw)) {
		return fmt.Errorf("%w: role catalog digest changed", harness.ErrInvalidInput)
	}
	return nil
}
