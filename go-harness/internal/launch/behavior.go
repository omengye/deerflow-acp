package launch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillbehavior"
)

// LoadBehaviorSuite is shared by save/validate and launch. It reads an explicit
// host file once; later worker calls use this immutable snapshot. The candidate
// package is never consulted for its own fixtures or expected answers.
func LoadBehaviorSuite(configPath, suitePath string, destinations []string) (*harness.SkillBehaviorOptions, error) {
	if strings.TrimSpace(suitePath) == "" || len(suitePath) > 4096 || strings.ContainsRune(suitePath, 0) {
		return nil, fmt.Errorf("skill_evolution.behavior.suite_path: requires a bounded host suite path")
	}
	path := resolveConfigPath(configPath, suitePath)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > skillbehavior.MaxSuiteBytes {
		return nil, fmt.Errorf("skill_evolution.behavior.suite_path: requires a regular suite file of at most 1 MiB")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("skill_evolution.behavior.suite_path: cannot open host suite file")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
		return nil, fmt.Errorf("skill_evolution.behavior.suite_path: file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, skillbehavior.MaxSuiteBytes+1))
	if err != nil || len(data) > skillbehavior.MaxSuiteBytes {
		return nil, fmt.Errorf("skill_evolution.behavior.suite_path: cannot read bounded suite file")
	}
	return parseBehaviorRegistry(data, destinations)
}

// The registry embeds suites, not paths or generated assertions. The same
// bounded, immutable document is validated at save and startup.
func parseBehaviorRegistry(data []byte, destinations []string) (*harness.SkillBehaviorOptions, error) {
	canonical, err := skillbehavior.CanonicalJSON(data)
	if err != nil {
		return nil, fmt.Errorf("skill_evolution.behavior.suite_path: invalid host JSON: %w", err)
	}
	var header struct {
		Version string `json:"version"`
	}
	if err = json.Unmarshal(canonical, &header); err != nil {
		return nil, fmt.Errorf("skill_evolution.behavior.suite_path: invalid suite header")
	}
	entries := []json.RawMessage{canonical}
	if header.Version == "skill-behavior-registry-v1" {
		var registry struct {
			Version string            `json:"version"`
			Suites  []json.RawMessage `json:"suites"`
		}
		decoder := json.NewDecoder(bytes.NewReader(canonical))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&registry); err != nil || len(registry.Suites) < 1 || len(registry.Suites) > 32 {
			return nil, fmt.Errorf("skill_evolution.behavior.suite_path: registry requires 1..32 embedded suites and no unknown fields")
		}
		entries = registry.Suites
	}
	result := &harness.SkillBehaviorOptions{}
	seen := map[string]bool{}
	for _, entry := range entries {
		suite, err := skillbehavior.Parse(entry)
		if err != nil {
			return nil, fmt.Errorf("skill_evolution.behavior.suite_path: invalid host fixture suite: %w", err)
		}
		if !slices.Contains(destinations, suite.SourceID) {
			return nil, fmt.Errorf("skill_evolution.behavior.suite_path: suite target is not an authorized candidate source")
		}
		key := suite.SourceID + "/" + suite.Name
		if seen[key] {
			return nil, fmt.Errorf("skill_evolution.behavior.suite_path: duplicate suite target")
		}
		seen[key] = true
		result.Suites = append(result.Suites, suite)
	}
	return result, nil
}
