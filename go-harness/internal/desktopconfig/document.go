package desktopconfig

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/configschema"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

var runtimeDefaults = configschema.Defaults("runtime")
var memoryDefaults = configschema.Defaults("memory")

func (s service) snapshot() (map[string]any, error) {
	data, err := readYAML(s.config)
	if err != nil {
		return nil, err
	}
	if value := data["tools"]; value != nil {
		if _, ok := value.([]any); !ok {
			return nil, errors.New("tools 必须是数组")
		}
	}
	if value := data["sandbox"]; value != nil {
		if _, ok := value.(map[string]any); !ok {
			return nil, errors.New("sandbox 必须是对象")
		}
	}
	if value := data["skills"]; value != nil {
		settings, ok := value.(map[string]any)
		if !ok {
			return nil, errors.New("skills 必须是对象")
		}
		if enabled := settings["enabled"]; enabled != nil {
			if err := configschema.ValidateValue("/skills_enabled", enabled); err != nil {
				return nil, err
			}
		}
	}
	paths := s.paths(data)
	skillSources, skillSourceDiagnostics, err := configschema.ProjectSkillSources(readObject(data, "skills"))
	if err != nil {
		return nil, err
	}
	skillSourceDiagnostics = append(skillSourceDiagnostics, s.skillDirectoryDiagnostics...)
	if boolValue(readObject(data, "skills"), "enabled", true) {
		settings := readObject(data, "skills")
		if _, err := skills.ResolveManagedSources(s.config, settings["managed_sources"], settings["export_root"]); err != nil {
			skillSourceDiagnostics = append(skillSourceDiagnostics, err.Error())
		}
	}
	configRevision, err := hashFile(s.config)
	if err != nil {
		return nil, err
	}
	extensionsRevision, err := hashFile(paths.extensions)
	if err != nil {
		return nil, err
	}
	controls, err := configschema.ProjectSubagentControls(data)
	if err != nil {
		return nil, err
	}
	models := make([]any, 0)
	for _, item := range list(data, "models") {
		if raw, ok := item.(map[string]any); ok {
			models = append(models, modelDocument(raw))
		}
	}
	defaultModel := str(data, "default_model", "")
	if defaultModel == "" && len(models) > 0 {
		defaultModel = str(models[0].(map[string]any), "name", "")
	}
	runtime := cloneMap(runtimeDefaults)
	local := readObject(data, "local_acp")
	for key := range runtime {
		if value, ok := local[key]; ok && value != nil {
			runtime[key] = value
		}
	}
	if str(local, "model_name", "") == "" {
		if selected := str(readObject(data, "api"), "model_name", ""); selected != "" {
			runtime["model_name"] = selected
		}
	}
	// Mirror the launch fallback so an unrelated save preserves the timeout.
	if local["run_timeout_seconds"] == nil {
		if value := readObject(data, "api")["chat_request_timeout"]; value != nil {
			runtime["run_timeout_seconds"] = value
		}
	}
	if runtime["permission_mode"] == false {
		runtime["permission_mode"] = "off"
	}
	for _, key := range []string{"permission_mode", "memory_scope"} {
		if runtime[key] == "" {
			runtime[key] = runtimeDefaults[key]
		}
	}
	memoryRaw := readObject(data, "memory")
	backend := readObject(memoryRaw, "backend_config")
	memory := cloneMap(memoryDefaults)
	for key := range memory {
		if value, ok := memoryRaw[key]; ok && value != nil {
			memory[key] = value
		}
	}
	if value := backend["retrieval_enabled"]; value != nil {
		memory["retrieval_enabled"] = value
	}
	if memory["mode"] == "" {
		memory["mode"] = memoryDefaults["mode"]
	}
	sandboxRaw := readObject(data, "sandbox")
	sandboxProvider := str(sandboxRaw, "provider", str(sandboxRaw, "use", "local"))
	if sandboxProvider == "deerflow.sandbox.local:LocalSandboxProvider" {
		sandboxProvider = "local"
	}
	sandbox := configschema.Defaults("sandbox")
	sandbox["provider"] = sandboxProvider
	for _, key := range []string{"allow_host_bash", "allow_host_tools"} {
		if value := sandboxRaw[key]; value != nil {
			sandbox[key] = value
		}
	}
	tools, toolDiagnostics, err := toolDocuments(list(data, "tools"))
	if err != nil {
		return nil, err
	}
	for key := range withoutKeys(sandboxRaw, "provider", "use", "allow_host_bash", "allow_host_tools") {
		toolDiagnostics = append(toolDiagnostics, "sandbox."+key+"：原数据保留，当前 Go 配置不读取此设置")
	}
	if len(list(data, "tool_groups")) > 0 {
		toolDiagnostics = append(toolDiagnostics, "tool_groups：仅保留历史分组数据，不参与 Go 工具授权")
	}
	sort.Strings(toolDiagnostics)
	subagents := configschema.Defaults("subagents")
	for key, value := range readObject(data, "subagents") {
		if value != nil {
			subagents[key] = redact(value, key)
		}
	}
	subagents["enabled"], subagents["max_concurrent"] = controls.Enabled, controls.MaxConcurrent
	if value := data["skill_evolution"]; value != nil {
		if _, ok := value.(map[string]any); !ok {
			return nil, fmt.Errorf("skill_evolution: must be an object")
		}
	}
	evolutionRaw := readObject(data, "skill_evolution")
	evolution, ignoredEvolution, err := configschema.ProjectEvolution(evolutionRaw)
	if err != nil {
		return nil, err
	}
	evolution = redact(evolution, "").(map[string]any)

	return map[string]any{
		"config_migration": map[string]any{
			"required": controls.Legacy, "target_version": configschema.NativeConfigVersion,
			"source_revision": configRevision, "source_version": data["config_version"],
			"legacy_local_enabled": controls.LegacyLocalEnabled, "legacy_role_enabled": controls.LegacyRoleEnabled,
			"enabled": controls.Enabled, "max_concurrent": controls.MaxConcurrent,
			"removed_paths": []string{"local_acp.subagent_enabled", "local_acp.max_concurrent_subagents"},
			"cli_note":      "新版显式 --disable-subagents 参数优先于 subagents.enabled；旧版 subagents.enabled=false 不可被 CLI 重新开启的规则仅保留在未迁移文件中。迁移前请核对自定义启动参数。",
		},
		"config_migration_ack": "",
		"config_schema":        configschema.Describe(),
		"config_revision":      configRevision, "extensions_revision": extensionsRevision,
		"default_model": defaultModel, "models": models, "runtime": runtime, "memory": memory,
		"subagents": subagents, "sandbox": sandbox,
		"tools": tools, "tool_diagnostics": toolDiagnostics,
		"skills_enabled":                boolValue(readObject(data, "skills"), "enabled", true),
		"skill_sources":                 skillSources,
		"skill_source_diagnostics":      skillSourceDiagnostics,
		"skill_evolution":               evolution,
		"skill_evolution_migration_ack": "",
		"skill_evolution_migration": map[string]any{
			"required":        len(ignoredEvolution) > 0 || evolution["mode"] != "review" || evolution["security_fail_closed"] != true,
			"source_revision": configRevision, "removed_paths": ignoredEvolution,
			"target_mode": "review", "security_fail_closed": true,
		},
		"paths": map[string]any{"config": s.config, "extensions": paths.extensions, "skills": paths.skills, "user_data": s.userData},
	}, nil
}

func modelDocument(raw map[string]any) map[string]any {
	contextWindow := any(0)
	if value := raw["context_window"]; value != nil {
		contextWindow = value
	}
	key := str(raw, "api_key", "")
	reference := ""
	if envReference.MatchString(strings.TrimSpace(key)) {
		reference = key
	}
	return map[string]any{
		"original_name": str(raw, "name", ""), "name": str(raw, "name", ""),
		"display_name": str(raw, "display_name", ""), "description": str(raw, "description", ""),
		"provider": nativeProvider(raw), "model": str(raw, "model", ""),
		"api_key": reference, "api_key_configured": key != "", "api_key_literal": key != "" && reference == "", "clear_api_key": false,
		"base_url":        str(raw, "base_url", ""),
		"supports_vision": boolValue(raw, "supports_vision", false), "context_window": contextWindow,
	}
}

func nativeProvider(raw map[string]any) string {
	value := str(raw, "provider", str(raw, "use", ""))
	if normalized, err := harness.NativeModelProvider(value); err == nil {
		return normalized
	}
	return value
}

func withoutKeys(raw map[string]any, excludes ...string) map[string]any {
	excluded := make(map[string]bool, len(excludes))
	for _, key := range excludes {
		excluded[key] = true
	}
	out := make(map[string]any)
	for key, value := range raw {
		if !excluded[key] {
			out[key] = value
		}
	}
	return out
}
func requireObject(data map[string]any, name string) (map[string]any, error) {
	value, ok := data[name].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object", name)
	}
	return value, nil
}
