package agents

import (
	"bytes"
	"fmt"
	"io"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"go.yaml.in/yaml/v3"
)

// DecodeConfig is shared by launch and desktop validation. Unknown fields must
// not become settings that can be saved but have no execution meaning.
func DecodeConfig(data []byte, models map[string]bool) (harness.SubagentsConfig, error) {
	var config harness.SubagentsConfig
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return config, fmt.Errorf("subagents: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return config, fmt.Errorf("subagents: expected one configuration document")
	}
	return config, config.Validate(models)
}
