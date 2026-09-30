package desktopconfig

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"go.yaml.in/yaml/v3"
)

type agentWrite struct {
	original, name, soul string
	data                 map[string]any
}

func (s service) save(document map[string]any, validateOnly bool) (any, error) {
	lock, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()
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
	if document["config_revision"] != configRevision || document["extensions_revision"] != extensionsRevision {
		return nil, errors.New("配置已变化，请重新加载后保存")
	}
	models, names, err := validatedModels(list(document, "models"), list(data, "models"))
	if err != nil {
		return nil, err
	}
	defaultModel := str(document, "default_model", "")
	if !names[defaultModel] {
		return nil, errors.New("默认模型必须引用已配置模型")
	}
	runtime, err := requireObject(document, "runtime")
	if err != nil {
		return nil, err
	}
	if err := validateRuntime(runtime, names); err != nil {
		return nil, err
	}
	memoryDoc, err := requireObject(document, "memory")
	if err != nil {
		return nil, err
	}
	memory, err := validatedMemory(memoryDoc, readObject(data, "memory"), names, runtime)
	if err != nil {
		return nil, err
	}
	sandboxDoc, err := requireObject(document, "sandbox")
	if err != nil {
		return nil, err
	}
	sandbox, err := validatedSandbox(sandboxDoc, readObject(data, "sandbox"), runtime)
	if err != nil {
		return nil, err
	}
	groups, err := validatedNamedList(list(document, "tool_groups"), list(data, "tool_groups"), false)
	if err != nil {
		return nil, fmt.Errorf("tool_groups: %w", err)
	}
	tools, err := validatedNamedList(list(document, "tools"), list(data, "tools"), true)
	if err != nil {
		return nil, fmt.Errorf("tools: %w", err)
	}
	if err := validateToolGroups(groups, tools); err != nil {
		return nil, err
	}
	agents, err := validatedAgents(list(document, "agents"), names, paths.agents)
	if err != nil {
		return nil, err
	}
	if str(runtime, "agent_name", "") != "" {
		return nil, errors.New("Go ACP 尚不支持选择自定义主 Agent，请清空 ACP Agent")
	}
	subagentsDoc, err := requireObject(document, "subagents")
	if err != nil {
		return nil, err
	}
	subagents, err := validatedSubagents(subagentsDoc, readObject(data, "subagents"), names)
	if err != nil {
		return nil, err
	}
	evolutionDoc, err := requireObject(document, "skill_evolution")
	if err != nil {
		return nil, err
	}
	evolution, err := validatedEvolution(evolutionDoc, readObject(data, "skill_evolution"), names)
	if err != nil {
		return nil, err
	}
	candidate := cloneMap(data)
	candidate["models"] = models
	candidate["default_model"] = defaultModel
	candidate["memory"] = memory
	candidate["sandbox"] = sandbox
	candidate["tool_groups"] = groups
	candidate["tools"] = tools
	candidate["subagents"] = subagents
	candidate["skill_evolution"] = evolution
	local := object(candidate, "local_acp")
	for key := range runtimeDefaults {
		if value, ok := runtime[key]; ok {
			local[key] = value
		}
	}
	if _, err := bridgePolicy(candidate); err != nil {
		return nil, err
	}
	skillsConfig := object(candidate, "skills")
	skillsEnabled, ok := document["skills_enabled"].(bool)
	if !ok {
		return nil, errors.New("skills_enabled 必须是布尔值")
	}
	skillsConfig["enabled"] = skillsEnabled
	states := object(extensions, "skills")
	for _, item := range list(document, "skills") {
		skill, ok := item.(map[string]any)
		if !ok {
			return nil, errors.New("Skill 条目结构无效")
		}
		name := str(skill, "name", "")
		if name == "" {
			return nil, errors.New("Skill 名称不能为空")
		}
		enabled, ok := skill["enabled"].(bool)
		if !ok {
			return nil, errors.New("Skill enabled 必须是布尔值")
		}
		state := object(states, name)
		state["enabled"] = enabled
	}
	if validateOnly {
		return map[string]any{"valid": true}, nil
	}
	backup, err := s.backup(paths)
	if err != nil {
		return nil, err
	}
	if err := writeYAMLAtomic(s.config, candidate); err != nil {
		return nil, fmt.Errorf("保存主配置失败: %w", err)
	}
	if err := writeJSONAtomic(paths.extensions, extensions); err != nil {
		return nil, s.rollback(paths, backup, err)
	}
	if err := applyAgents(paths.agents, agents); err != nil {
		return nil, s.rollback(paths, backup, err)
	}
	result, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	result["backup"] = backup
	return result, nil
}

func validatedModels(incoming, existing []any) ([]any, map[string]bool, error) {
	previous := make(map[string]map[string]any)
	for _, item := range existing {
		if model, ok := item.(map[string]any); ok {
			previous[str(model, "name", "")] = model
		}
	}
	output := make([]any, 0, len(incoming))
	names := map[string]bool{}
	usedOriginal := map[string]bool{}
	for _, item := range incoming {
		model, ok := item.(map[string]any)
		if !ok {
			return nil, nil, errors.New("模型条目结构无效")
		}
		name := strings.TrimSpace(str(model, "name", ""))
		if name == "" || names[name] {
			return nil, nil, errors.New("模型名称必须非空且唯一")
		}
		use := strings.TrimSpace(str(model, "use_path", ""))
		id := strings.TrimSpace(str(model, "model", ""))
		if id == "" {
			return nil, nil, fmt.Errorf("模型 %s 缺少模型 ID", name)
		}
		provider, providerErr := harness.NativeModelProvider(use)
		if providerErr != nil {
			return nil, nil, fmt.Errorf("模型 %s 的提供商不受 Go ACP 支持", name)
		}
		advanced, ok := model["advanced"].(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("模型 %s 的高级设置必须是对象", name)
		}
		original := str(model, "original_name", "")
		if original != "" {
			if usedOriginal[original] {
				return nil, nil, errors.New("两个模型不能引用同一原始模型")
			}
			usedOriginal[original] = true
		}
		old := previous[original]
		baseURL := strings.TrimSpace(str(model, "base_url", ""))
		// A redacted credential is reusable only for the same named provider and endpoint.
		sameBackend := old != nil && nativeProvider(old) == provider && str(old, "base_url", "") == baseURL
		var previousAdvanced any
		if sameBackend {
			previousAdvanced = old
		}
		raw, _ := restore(advanced, previousAdvanced).(map[string]any)
		if hasRedacted(raw) {
			return nil, nil, fmt.Errorf("模型 %s 的隐藏凭据已变化，请重新输入", name)
		}
		raw["name"] = name
		delete(raw, "use")
		raw["provider"] = provider
		raw["model"] = id
		for _, key := range []string{"supports_thinking", "supports_reasoning_effort", "supports_vision"} {
			raw[key] = boolValue(model, key, false)
		}
		for _, key := range []string{"display_name", "description", "base_url"} {
			value := strings.TrimSpace(str(model, key, ""))
			if value != "" {
				raw[key] = value
			} else {
				delete(raw, key)
			}
		}
		key := strings.TrimSpace(str(model, "api_key", ""))
		if boolValue(model, "clear_api_key", false) {
			delete(raw, "api_key")
		} else if key != "" && key != redacted {
			raw["api_key"] = key
		} else if sameBackend && str(old, "api_key", "") != "" {
			raw["api_key"] = old["api_key"]
		} else if key == redacted {
			return nil, nil, fmt.Errorf("模型 %s 的凭据已变化，请重新输入", name)
		}
		if _, ok := raw["api_key"]; !ok {
			raw["api_key"] = ""
		}
		output = append(output, raw)
		names[name] = true
	}
	if len(output) == 0 {
		return nil, nil, errors.New("至少配置一个模型")
	}
	return output, names, nil
}

func validateRuntime(runtime map[string]any, models map[string]bool) error {
	if selected := str(runtime, "model_name", ""); selected != "" && !models[selected] {
		return errors.New("ACP 模型必须引用已配置模型")
	}
	if boolValue(runtime, "goal_auto_continue", false) {
		return errors.New("Go ACP 尚不支持 goal_auto_continue")
	}
	for key, limits := range map[string][2]float64{"max_active_connections": {1, 128}, "max_active_runs": {1, 128}, "max_concurrent_subagents": {1, 4}, "run_timeout_seconds": {0.001, 86400}, "queue_timeout_seconds": {0.001, 86400}, "inactive_session_retention_days": {1, 3650}, "closed_session_retention_days": {0, 3650}, "session_cleanup_interval_seconds": {60, 86400}} {
		value := number(runtime, key, -1)
		if value < limits[0] || value > limits[1] {
			return fmt.Errorf("runtime.%s 超出范围", key)
		}
	}
	mode := str(runtime, "permission_mode", "")
	if mode != "off" && mode != "dangerous" && mode != "all" {
		return errors.New("runtime.permission_mode 必须是 off、dangerous 或 all")
	}
	scope := str(runtime, "memory_scope", "")
	if scope != "session" && scope != "workspace" && scope != "global" {
		return errors.New("runtime.memory_scope 必须是 session、workspace 或 global")
	}
	if len(str(runtime, "prompt_overlay", "")) > 65536 {
		return errors.New("runtime.prompt_overlay 超过长度限制")
	}
	return nil
}

func validatedMemory(document, previous map[string]any, models map[string]bool, runtime map[string]any) (map[string]any, error) {
	if str(document, "manager_class", "") != "deermem" {
		return nil, errors.New("Go ACP 仅支持本地 DeerMem")
	}
	mode := str(document, "mode", "")
	if mode != "middleware" && mode != "tool" {
		return nil, errors.New("memory.mode 必须是 middleware 或 tool")
	}
	if selected := str(document, "model_name", ""); selected != "" && !models[selected] {
		return nil, errors.New("记忆模型必须引用已配置模型")
	}
	if str(runtime, "memory_scope", "") == "global" && boolValue(document, "enabled", true) {
		return nil, errors.New("Go 记忆存储暂不支持 global 范围，请选择 workspace 或 session")
	}
	if mode == "tool" && boolValue(document, "enabled", true) && !boolValue(document, "retrieval_enabled", true) {
		return nil, errors.New("memory.mode=tool 要求启用检索")
	}
	for key, limits := range map[string][2]float64{"shutdown_flush_timeout_seconds": {0.1, 300}, "debounce_seconds": {1, 300}, "max_facts": {10, 500}, "fact_confidence_threshold": {0, 1}, "fact_dedup_similarity_threshold": {0.5, 1}, "max_injection_tokens": {100, 8000}, "retrieval_top_k": {1, 100}} {
		value := number(document, key, -1)
		if value < limits[0] || value > limits[1] {
			return nil, fmt.Errorf("memory.%s 超出范围", key)
		}
	}
	advanced, ok := document["advanced"].(map[string]any)
	if !ok {
		return nil, errors.New("memory.advanced 必须是对象")
	}
	backendAdvanced, ok := document["backend_advanced"].(map[string]any)
	if !ok {
		return nil, errors.New("memory.backend_advanced 必须是对象")
	}
	backend := cloneMap(backendAdvanced)
	for _, key := range memoryBackendFields {
		backend[key] = document[key]
	}
	backend, _ = restore(backend, readObject(previous, "backend_config")).(map[string]any)
	raw := cloneMap(advanced)
	for _, key := range []string{"enabled", "manager_class", "mode", "injection_enabled", "shutdown_flush_timeout_seconds"} {
		raw[key] = document[key]
	}
	raw["backend_config"] = backend
	raw, _ = restore(raw, previous).(map[string]any)
	if hasRedacted(raw) {
		return nil, errors.New("记忆配置含无法恢复的隐藏值")
	}
	return raw, nil
}

func validatedSandbox(document, previous, runtime map[string]any) (map[string]any, error) {
	if str(document, "use", "") != "local" && str(document, "use", "") != "deerflow.sandbox.local:LocalSandboxProvider" {
		return nil, errors.New("桌面 Go ACP 仅支持本地沙箱")
	}
	advanced, ok := document["advanced"].(map[string]any)
	if !ok {
		return nil, errors.New("sandbox.advanced 必须是对象")
	}
	raw := cloneMap(advanced)
	delete(raw, "use")
	raw["provider"] = "local"
	raw["allow_host_bash"] = boolValue(document, "allow_host_bash", false)
	raw["allow_host_tools"] = boolValue(document, "allow_host_tools", false)
	raw, _ = restore(raw, previous).(map[string]any)
	if hasRedacted(raw) {
		return nil, errors.New("沙箱配置含无法恢复的隐藏值")
	}
	if boolValue(runtime, "enable_bash", false) && !boolValue(raw, "allow_host_bash", false) {
		return nil, errors.New("启用 bash 时需要允许宿主命令")
	}
	return raw, nil
}

func validatedNamedList(incoming, previous []any, tool bool) ([]any, error) {
	old := map[string]map[string]any{}
	for _, item := range previous {
		if value, ok := item.(map[string]any); ok {
			old[str(value, "name", "")] = value
		}
	}
	output := make([]any, 0, len(incoming))
	seen := map[string]bool{}
	for _, item := range incoming {
		value, ok := item.(map[string]any)
		if !ok {
			return nil, errors.New("条目结构无效")
		}
		name := strings.TrimSpace(str(value, "name", ""))
		if name == "" || seen[name] {
			return nil, errors.New("名称必须非空且唯一")
		}
		seen[name] = true
		prior := old[name]
		if tool && prior != nil && str(prior, "use", "") != str(value, "use", "") {
			prior = nil
		}
		restored, _ := restore(value, prior).(map[string]any)
		if hasRedacted(restored) {
			return nil, fmt.Errorf("%s 的隐藏值已变化，请重新输入", name)
		}
		if tool {
			encoded, err := yaml.Marshal(restored)
			if err != nil {
				return nil, fmt.Errorf("%s 的工具配置无效", name)
			}
			var setting harness.BuiltinToolConfig
			if err := yaml.Unmarshal(encoded, &setting); err != nil {
				return nil, fmt.Errorf("%s 的工具参数类型无效", name)
			}
			if err := setting.Validate(); err != nil {
				return nil, err
			}
			delete(restored, "use")
		}
		output = append(output, restored)
	}
	return output, nil
}

func validateToolGroups(groups, tools []any) error {
	known := map[string]bool{}
	for _, item := range groups {
		known[str(item.(map[string]any), "name", "")] = true
	}
	for _, item := range tools {
		tool := item.(map[string]any)
		if !known[str(tool, "group", "")] {
			return errors.New("工具引用了未知工具组")
		}
	}
	return nil
}

var agentNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)

func validatedAgents(incoming []any, models map[string]bool, root string) ([]agentWrite, error) {
	result := make([]agentWrite, 0, len(incoming))
	seen := map[string]bool{}
	for _, item := range incoming {
		agent, ok := item.(map[string]any)
		if !ok {
			return nil, errors.New("Agent 条目结构无效")
		}
		name := str(agent, "name", "")
		original := str(agent, "original_name", "")
		if !agentNamePattern.MatchString(name) || seen[strings.ToLower(name)] {
			return nil, errors.New("Agent 名称必须非空且唯一，只能使用字母、数字、下划线和连字符")
		}
		if original != "" && !agentNamePattern.MatchString(original) {
			return nil, errors.New("Agent 原名称无效")
		}
		if selected := str(agent, "model", ""); selected != "" && !models[selected] {
			return nil, fmt.Errorf("Agent %s 引用了未知模型", name)
		}
		if original == "" {
			if _, err := os.Stat(filepath.Join(root, name)); err == nil {
				return nil, fmt.Errorf("Agent %s 已存在", name)
			}
		}
		raw := withoutKeys(agent, "original_name", "soul", "invalid")
		if original != "" {
			previous, err := readYAML(filepath.Join(root, original, "config.yaml"))
			if err != nil {
				return nil, fmt.Errorf("Agent %s 的原配置无法读取", name)
			}
			raw, _ = restore(raw, previous).(map[string]any)
		}
		if hasRedacted(raw) {
			return nil, fmt.Errorf("Agent %s 的隐藏值已变化，请重新输入", name)
		}
		result = append(result, agentWrite{original: original, name: name, soul: str(agent, "soul", ""), data: raw})
		seen[strings.ToLower(name)] = true
	}
	return result, nil
}

func validatedSubagents(document, previous map[string]any, models map[string]bool) (map[string]any, error) {
	advanced, ok := document["advanced"].(map[string]any)
	if !ok {
		return nil, errors.New("subagents.advanced 必须是对象")
	}
	raw := cloneMap(advanced)
	for _, key := range []string{"enabled", "timeout_seconds", "max_turns", "agents", "custom_agents"} {
		if value, ok := document[key]; ok {
			raw[key] = value
		}
	}
	raw, _ = restore(raw, previous).(map[string]any)
	if hasRedacted(raw) {
		return nil, errors.New("子代理配置含无法恢复的隐藏值")
	}
	for _, collection := range []string{"agents", "custom_agents"} {
		items, ok := raw[collection].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("subagents.%s 必须是对象", collection)
		}
		for name, value := range items {
			details, ok := value.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("子代理 %s 配置无效", name)
			}
			selected := str(details, "model", "")
			if selected != "" && selected != "inherit" && !models[selected] {
				return nil, fmt.Errorf("子代理 %s 引用了未知模型", name)
			}
		}
	}
	return raw, nil
}

func validatedEvolution(document, previous map[string]any, models map[string]bool) (map[string]any, error) {
	advanced, ok := document["advanced"].(map[string]any)
	if !ok {
		return nil, errors.New("skill_evolution.advanced 必须是对象")
	}
	raw := cloneMap(advanced)
	for key, value := range document {
		if key != "advanced" {
			raw[key] = value
		}
	}
	raw, _ = restore(raw, previous).(map[string]any)
	if hasRedacted(raw) {
		return nil, errors.New("Skill evolution 配置含无法恢复的隐藏值")
	}
	for _, key := range []string{"generation_model_name", "moderation_model_name", "evaluation_model_name"} {
		if selected := str(raw, key, ""); selected != "" && !models[selected] {
			return nil, fmt.Errorf("skill_evolution.%s 引用了未知模型", key)
		}
	}
	return raw, nil
}

func (s service) backup(paths configPaths) (string, error) {
	root := filepath.Join(s.userData, "backups")
	backup := filepath.Join(root, time.Now().UTC().Format("20060102-150405-000000"))
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(backup); errors.Is(err, os.ErrNotExist) {
			break
		}
		backup = filepath.Join(root, fmt.Sprintf("%s-%02d", time.Now().UTC().Format("20060102-150405-000000"), i))
	}
	if err := os.MkdirAll(backup, 0700); err != nil {
		return "", err
	}
	for _, path := range []string{s.config, paths.extensions} {
		body, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(backup, filepath.Base(path)), body, 0600); err != nil {
			return "", err
		}
	}
	if err := copyDirectory(paths.agents, filepath.Join(backup, "agents")); err != nil {
		return "", err
	}
	return backup, nil
}

func copyDirectory(source, destination string) error {
	if _, err := os.Stat(source); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("Agent 目录包含符号链接")
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		if !entry.Type().IsRegular() {
			return errors.New("Agent 目录包含不支持的文件")
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		defer input.Close()
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, io.LimitReader(input, 4<<20+1))
		return errors.Join(err, out.Close())
	})
}

func applyAgents(root string, agents []agentWrite) error {
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, agent := range agents {
		if agent.original != "" && agent.original != agent.name {
			if err := os.Rename(filepath.Join(root, agent.original), filepath.Join(root, agent.name)); err != nil {
				return err
			}
		}
		target := filepath.Join(root, agent.name)
		if err := os.MkdirAll(target, 0700); err != nil {
			return err
		}
		if err := writeYAMLAtomic(filepath.Join(target, "config.yaml"), agent.data); err != nil {
			return err
		}
		soulPath := filepath.Join(target, "SOUL.md")
		if strings.TrimSpace(agent.soul) == "" {
			if err := os.Remove(soulPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		} else if err := writeAtomic(soulPath, []byte(strings.TrimRight(agent.soul, "\r\n")+"\n")); err != nil {
			return err
		}
		keep[agent.name] = true
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() && !keep[entry.Name()] {
			if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s service) rollback(paths configPaths, backup string, cause error) error {
	for _, path := range []string{s.config, paths.extensions} {
		body, err := os.ReadFile(filepath.Join(backup, filepath.Base(path)))
		if err == nil {
			_ = writeAtomic(path, body)
		}
	}
	_ = os.RemoveAll(paths.agents)
	_ = copyDirectory(filepath.Join(backup, "agents"), paths.agents)
	return fmt.Errorf("保存失败，已尝试恢复保存前配置；备份位于 %s: %w", backup, cause)
}
