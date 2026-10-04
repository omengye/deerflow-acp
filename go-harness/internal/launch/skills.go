package launch

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"go.yaml.in/yaml/v3"
)

// InspectConfiguredSkills uses the launcher's actual source/import resolution
// without opening a runtime/database or constructing models. Desktop saves use
// it before any writes so managed and import sources have the same checks.
func InspectConfiguredSkills(configPath string, document map[string]any) (harness.SkillsConfig, error) {
	data, err := yaml.Marshal(document)
	if err != nil {
		return harness.SkillsConfig{}, fmt.Errorf("skills: cannot encode configuration")
	}
	var source runtimeDocument
	if err := yaml.Unmarshal(data, &source); err != nil {
		return harness.SkillsConfig{}, fmt.Errorf("skills: invalid configuration shape")
	}
	var cfg deerflow.Config
	err = applyConfiguredSkills(configPath, source, &cfg, nil)
	return cfg.Skills, err
}

func skillsFlags(flags *flag.FlagSet, cfg *deerflow.Config) {
	flags.Func("skills-config", "explicit Skills JSON file with sources, limits, install and includeGlobal/names selection; no implicit scanning", func(path string) error {
		if strings.TrimSpace(path) == "" {
			return fmt.Errorf("skills configuration path is required")
		}
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("cannot open skills configuration: %w", err)
		}
		defer file.Close()
		const maxConfig = 1 << 20
		data, err := io.ReadAll(io.LimitReader(file, maxConfig+1))
		if err != nil || len(data) > maxConfig {
			return fmt.Errorf("skills configuration exceeds limit or could not be read")
		}
		var document struct {
			Sources        []harness.SkillSource  `json:"sources"`
			Limits         harness.SkillLimits    `json:"limits"`
			Install        []harness.SkillInstall `json:"install"`
			IncludeGlobal  bool                   `json:"includeGlobal"`
			Names          []string               `json:"names"`
			BuiltinSources []string               `json:"builtinSources"`
			ManagedSources []string               `json:"managedSources"`
			ExportRoot     string                 `json:"exportRoot"`
		}
		dec := json.NewDecoder(strings.NewReader(string(data)))
		dec.DisallowUnknownFields()
		if len(strings.TrimSpace(string(data))) == 0 || strings.TrimSpace(string(data))[0] != '{' {
			return fmt.Errorf("skills configuration must be a JSON object")
		}
		if err = dec.Decode(&document); err != nil {
			return fmt.Errorf("invalid skills configuration: %w", err)
		}
		var extra any
		if dec.Decode(&extra) != io.EOF {
			return fmt.Errorf("skills configuration must contain one object")
		}
		if len(document.Install) > 128 {
			return fmt.Errorf("too many startup skill installations")
		}
		cfg.Skills = harness.SkillsConfig{Sources: document.Sources, Limits: document.Limits, Install: document.Install, BuiltinSources: document.BuiltinSources, ManagedSources: document.ManagedSources, ExportRoot: document.ExportRoot}
		cfg.SkillSelection = harness.SkillSelection{IncludeGlobal: document.IncludeGlobal, Names: document.Names}
		return nil
	})
}
