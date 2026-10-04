package skills

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// ResolveManagedSources is shared by YAML launch and desktop validation. It
// opens and closes directory handles to enforce the registry's scope/link rules;
// it never scans packages, creates directories or writes a database.
func ResolveManagedSources(configPath string, sources, exportRoot any) (harness.SkillsConfig, error) {
	var config harness.SkillsConfig
	if sources != nil {
		data, err := json.Marshal(sources)
		if err != nil || len(data) > 1<<20 || len(data) == 0 || data[0] != '[' {
			return config, fmt.Errorf("skills.managed_sources: requires a bounded array of source definitions")
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&config.Sources); err != nil {
			return config, fmt.Errorf("skills.managed_sources: expected id, root, scope and optional workspace")
		}
	}
	if len(config.Sources) > 32 {
		return config, fmt.Errorf("skills.managed_sources: at most 32 sources are supported")
	}
	base, err := filepath.Abs(filepath.Dir(configPath))
	if err != nil {
		return config, err
	}
	resolve := func(path string) string {
		if path == "" {
			return ""
		}
		if filepath.IsAbs(path) {
			return filepath.Clean(path)
		}
		return filepath.Join(base, path)
	}
	seen := map[string]bool{}
	for index, source := range config.Sources {
		// These IDs already bind import installations/checkpoints. Managed
		// sources must not silently change an existing source's policy.
		if !validName(source.ID) || seen[source.ID] || source.ID == "python-public" || source.ID == "python-custom" {
			return config, fmt.Errorf("skills.managed_sources.%d.id: requires a unique non-reserved source name", index)
		}
		seen[source.ID] = true
		source.Root, source.Workspace = resolve(source.Root), resolve(source.Workspace)
		opened, err := openSource(source)
		if err != nil {
			return config, fmt.Errorf("skills.managed_sources.%d: %w", index, err)
		}
		config.Sources[index] = opened.config
		if err := opened.root.Close(); err != nil {
			return config, err
		}
		config.ManagedSources = append(config.ManagedSources, source.ID)
	}
	if exportRoot != nil {
		path, ok := exportRoot.(string)
		if !ok {
			return config, fmt.Errorf("skills.export_root: must be a path string")
		}
		config.ExportRoot = resolve(path)
	}
	if config.ExportRoot != "" {
		target, err := openExportTarget(config.ExportRoot)
		if err != nil {
			return config, fmt.Errorf("skills.export_root: %w", err)
		}
		if err := target.root.Close(); err != nil {
			return config, err
		}
	} else if len(config.ManagedSources) > 0 {
		return config, fmt.Errorf("skills.export_root: required for managed sources")
	}
	return config, nil
}
