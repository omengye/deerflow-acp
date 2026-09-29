package launch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// LoadACPAgentsConfig reads an explicit host-owned JSON allowlist. Only env
// values may refer to process environment variables; model input cannot alter
// the executable, arguments or credentials.
func LoadACPAgentsConfig(path string) (map[string]harness.ACPAgentConfig, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > 1<<20 {
		return nil, errors.New("ACP agent configuration exceeds 1 MiB")
	}
	var agents map[string]harness.ACPAgentConfig
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&agents); err != nil {
		return nil, fmt.Errorf("parse ACP agents: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("ACP agent configuration has trailing data")
	}
	if len(agents) == 0 || len(agents) > 16 {
		return nil, errors.New("ACP agent configuration requires 1..16 entries")
	}
	for name, config := range agents {
		for key, value := range config.Env {
			if strings.HasPrefix(value, "$") {
				variable := strings.TrimPrefix(value, "$")
				if variable == "" || strings.ContainsAny(variable, "${} ") {
					return nil, fmt.Errorf("ACP agent %q has invalid environment reference", name)
				}
				resolved, ok := os.LookupEnv(variable)
				if !ok {
					return nil, fmt.Errorf("ACP agent %q requires environment variable %s", name, variable)
				}
				config.Env[key] = resolved
			}
		}
		agents[name] = config
	}
	return agents, nil
}
