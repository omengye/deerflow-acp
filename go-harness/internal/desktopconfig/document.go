package desktopconfig

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"

	"go.yaml.in/yaml/v3"
)

var runtimeDefaults = map[string]any{
	"model_name": nil, "agent_name": nil, "thinking_enabled": true, "plan_mode": true,
	"subagent_enabled": false, "max_concurrent_subagents": 2, "max_active_connections": 16,
	"max_active_runs": 2, "run_timeout_seconds": 600, "queue_timeout_seconds": 600,
	"session_cleanup_enabled": true, "inactive_session_retention_days": 30,
	"closed_session_retention_days": 30, "session_cleanup_interval_seconds": 3600,
	"goal_auto_continue": false, "goal_max_continuations": 3,
	"goal_max_no_progress_continuations": 2, "permission_mode": "dangerous",
	"memory_scope": "workspace", "enable_bash": false, "tool_allowlist": nil,
	"tool_denylist": []any{}, "prompt_overlay": "",
}

var memoryDefaults = map[string]any{
	"enabled": true, "manager_class": "deermem", "mode": "middleware",
	"injection_enabled": true, "shutdown_flush_timeout_seconds": 30,
	"storage_path": "", "storage_class": "deerflow.agents.memory.storage.FileMemoryStorage",
	"debounce_seconds": 30, "model_name": nil, "max_facts": 100,
	"fact_confidence_threshold": 0.7, "fact_dedup_enabled": false,
	"fact_dedup_similarity_threshold": 0.7, "max_injection_tokens": 2000,
	"retrieval_enabled": true, "retrieval_top_k": 12, "retrieval_index_path": "",
}

var memoryBackendFields = []string{
	"storage_path", "storage_class", "debounce_seconds", "model_name", "max_facts",
	"fact_confidence_threshold", "fact_dedup_enabled", "fact_dedup_similarity_threshold",
	"max_injection_tokens", "retrieval_enabled", "retrieval_top_k", "retrieval_index_path",
}

func (s service) snapshot() (map[string]any, error) {
	data, err := readYAML(s.config)
	if err != nil {
		return nil, err
	}
	paths := s.paths(data)
	extensions, err := readJSON(paths.extensions)
	if err != nil {
		return nil, err
	}
	configRevision, err := hashFile(s.config)
	if err != nil {
		return nil, err
	}
	extensionsRevision, err := hashFile(paths.extensions)
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
	api := readObject(data, "api")
	for key := range runtime {
		if value, ok := local[key]; ok {
			runtime[key] = value
		}
	}
	for _, key := range []string{"thinking_enabled", "plan_mode"} {
		if _, ok := local[key]; !ok {
			if value, ok := api[key]; ok {
				runtime[key] = value
			}
		}
	}
	if runtime["permission_mode"] == false {
		runtime["permission_mode"] = "off"
	}
	memoryRaw := readObject(data, "memory")
	backend := readObject(memoryRaw, "backend_config")
	memory := cloneMap(memoryDefaults)
	for key := range memory {
		if value, ok := memoryRaw[key]; ok {
			memory[key] = value
		}
	}
	for _, key := range memoryBackendFields {
		if value, ok := backend[key]; ok {
			memory[key] = value
		}
	}
	memory["advanced"] = redact(withoutKeys(memoryRaw, append([]string{"backend_config"}, keys(memoryDefaults)...)...), "")
	memory["backend_advanced"] = redact(withoutKeys(backend, memoryBackendFields...), "")
	sandboxRaw := readObject(data, "sandbox")
	sandboxProvider := str(sandboxRaw, "provider", str(sandboxRaw, "use", "local"))
	if sandboxProvider == "deerflow.sandbox.local:LocalSandboxProvider" {
		sandboxProvider = "local"
	}
	sandbox := map[string]any{"use": sandboxProvider, "allow_host_bash": boolValue(sandboxRaw, "allow_host_bash", false), "allow_host_tools": boolValue(sandboxRaw, "allow_host_tools", false), "advanced": redact(withoutKeys(sandboxRaw, "provider", "use", "allow_host_bash", "allow_host_tools"), "")}
	subagentsRaw := readObject(data, "subagents")
	subagents := redact(subagentsRaw, "").(map[string]any)
	if _, ok := subagents["enabled"]; !ok {
		subagents["enabled"] = true
	}
	if _, ok := subagents["timeout_seconds"]; !ok {
		subagents["timeout_seconds"] = 900
	}
	if _, ok := subagents["max_turns"]; !ok {
		subagents["max_turns"] = nil
	}
	if _, ok := subagents["agents"]; !ok {
		subagents["agents"] = map[string]any{}
	}
	if _, ok := subagents["custom_agents"]; !ok {
		subagents["custom_agents"] = map[string]any{}
	}
	subagents["builtin_agents"] = []any{}
	subagents["advanced"] = redact(subagentsRaw, "")
	evolutionRaw := readObject(data, "skill_evolution")
	evolution := redact(evolutionRaw, "").(map[string]any)
	evolution["advanced"] = redact(evolutionRaw, "")
	agents, err := agentDocuments(paths.agents)
	if err != nil {
		return nil, err
	}
	skills, err := skillDocuments(paths.skills, extensions)
	if err != nil {
		return nil, err
	}
	homePath := func(value, fallback string) string {
		if value == "" {
			value = fallback
		}
		if filepath.IsAbs(value) {
			return filepath.Clean(value)
		}
		return filepath.Join(paths.home, value)
	}
	return map[string]any{
		"config_revision": configRevision, "extensions_revision": extensionsRevision,
		"default_model": defaultModel, "models": models, "runtime": runtime, "memory": memory,
		"agents": agents, "subagents": subagents, "sandbox": sandbox,
		"tool_groups": redact(list(data, "tool_groups"), ""), "tools": redact(list(data, "tools"), ""),
		"skills_enabled": boolValue(readObject(data, "skills"), "enabled", true), "skills": skills,
		"skill_evolution": evolution,
		"paths":           map[string]any{"config": s.config, "extensions": paths.extensions, "skills": paths.skills, "agents": paths.agents, "user_data": s.userData, "memory": homePath(str(memory, "storage_path", ""), "memory.json"), "memory_index": homePath(str(memory, "retrieval_index_path", ""), "memory-fts5.sqlite3")},
	}, nil
}

func modelDocument(raw map[string]any) map[string]any {
	known := []string{"name", "display_name", "description", "provider", "use", "model", "api_key", "base_url", "supports_thinking", "supports_reasoning_effort", "supports_vision"}
	key := str(raw, "api_key", "")
	reference := ""
	if envReference.MatchString(strings.TrimSpace(key)) {
		reference = key
	}
	return map[string]any{
		"original_name": str(raw, "name", ""), "name": str(raw, "name", ""),
		"display_name": str(raw, "display_name", ""), "description": str(raw, "description", ""),
		"use_path": nativeProvider(raw), "model": str(raw, "model", ""),
		"api_key": reference, "api_key_configured": key != "", "api_key_literal": key != "" && reference == "", "clear_api_key": false,
		"base_url": str(raw, "base_url", ""), "supports_thinking": boolValue(raw, "supports_thinking", false),
		"supports_reasoning_effort": boolValue(raw, "supports_reasoning_effort", false),
		"supports_vision":           boolValue(raw, "supports_vision", false), "advanced": redact(withoutKeys(raw, known...), ""),
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
func keys(raw map[string]any) []string {
	result := make([]string, 0, len(raw))
	for key := range raw {
		result = append(result, key)
	}
	return result
}

func agentDocuments(root string) ([]any, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return []any{}, nil
	}
	if err != nil {
		return nil, err
	}
	output := make([]any, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(root, entry.Name(), "config.yaml")
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		raw, err := readYAML(path)
		if err != nil {
			output = append(output, map[string]any{"original_name": entry.Name(), "name": entry.Name(), "description": "Invalid agent config", "model": nil, "tool_groups": []any{}, "skills": nil, "soul": "", "invalid": true})
			continue
		}
		entryDoc := redact(raw, "").(map[string]any)
		entryDoc["original_name"] = entry.Name()
		if str(entryDoc, "name", "") == "" {
			entryDoc["name"] = entry.Name()
		}
		if _, ok := entryDoc["tool_groups"]; !ok {
			entryDoc["tool_groups"] = []any{}
		}
		if _, ok := entryDoc["skills"]; !ok {
			entryDoc["skills"] = nil
		}
		if _, ok := entryDoc["memory_enabled"]; !ok {
			entryDoc["memory_enabled"] = true
		}
		soul, err := os.ReadFile(filepath.Join(root, entry.Name(), "SOUL.md"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		entryDoc["soul"] = string(soul)
		output = append(output, entryDoc)
	}
	return output, nil
}

func skillDocuments(root string, extensions map[string]any) ([]any, error) {
	states := readObject(extensions, "skills")
	output := make([]any, 0)
	for _, category := range []string{"public", "custom"} {
		categoryRoot := filepath.Join(root, category)
		if _, err := os.Stat(categoryRoot); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		seen := 0
		err := filepath.WalkDir(categoryRoot, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			seen++
			if seen > 4096 {
				return errors.New("Skills directory exceeds 4096 entries")
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return filepath.SkipDir
			}
			if entry.IsDir() || entry.Name() != "SKILL.md" {
				return nil
			}
			name, description := skillHeader(path)
			if name == "" {
				return nil
			}
			state, _ := states[name].(map[string]any)
			output = append(output, map[string]any{"name": name, "description": description, "category": category, "enabled": boolValue(state, "enabled", true), "path": filepath.Dir(path)})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(output, func(i, j int) bool {
		a := output[i].(map[string]any)
		b := output[j].(map[string]any)
		return str(a, "name", "") < str(b, "name", "")
	})
	return output, nil
}

func skillHeader(path string) (string, string) {
	file, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, 32<<10))
	if err != nil {
		return "", ""
	}
	text := strings.ReplaceAll(string(body), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return "", ""
	}
	end := strings.Index(text[4:], "\n---\n")
	if end < 0 {
		return "", ""
	}
	var metadata struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if yaml.Unmarshal([]byte(text[4:4+end]), &metadata) != nil || metadata.Name == "" {
		return "", ""
	}
	return metadata.Name, metadata.Description
}

func requireObject(data map[string]any, name string) (map[string]any, error) {
	value, ok := data[name].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object", name)
	}
	return value, nil
}
