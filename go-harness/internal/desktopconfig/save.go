package desktopconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
	"github.com/omengye/deerflow-acp/go-harness/internal/configschema"
	"github.com/omengye/deerflow-acp/go-harness/internal/launch"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills/evolution"
	"go.yaml.in/yaml/v3"
)

func (s service) save(document map[string]any, validateOnly bool) (any, error) {
	if _, exists := document["skills"]; exists {
		return nil, errors.New("skills 旧文件列表编辑已移除，请重新加载并通过技能目录管理启停")
	}
	if _, exists := document["agents"]; exists {
		return nil, errors.New("旧主 Agent 编辑器已移除，请重新加载；原目录保留供显式导入")
	}
	if _, exists := document["tool_groups"]; exists {
		return nil, errors.New("tool_groups 编辑入口已移除，请重新加载；原始分组数据保留供迁移")
	}
	if schema, exists := document["config_schema"]; exists {
		declared, ok := schema.(map[string]any)
		current := configschema.Describe()
		if !ok || declared["version"] != current.Version || declared["hash"] != current.Hash {
			return nil, errors.New("配置字段定义已变化，请重新加载；当前草稿尚未保存")
		}
	}
	lock, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()
	data, err := readYAML(s.config)
	if err != nil {
		return nil, err
	}
	if _, err := configschema.ArtifactSizeLimitMiB(data); err != nil {
		return nil, err
	}
	paths := s.paths(data)
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
	controls, err := configschema.ProjectSubagentControls(data)
	if err != nil {
		return nil, err
	}
	if !validateOnly && controls.Legacy && document["config_migration_ack"] != configRevision {
		return nil, errors.New("旧子代理配置需显式迁移：先查看转换结果及 CLI 优先级变化并确认迁移，再保存；原文件尚未修改")
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
	memory, err := validatedMemory(memoryDoc, readObject(data, "memory"), runtime)
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
	toolItems, ok := document["tools"].([]any)
	if !ok {
		return nil, errors.New("tools 必须是数组")
	}
	tools, err := validatedTools(toolItems, list(data, "tools"))
	if err != nil {
		return nil, fmt.Errorf("tools: %w", err)
	}

	subagentsDoc, err := requireObject(document, "subagents")
	if err != nil {
		return nil, err
	}
	providers := make(map[string]string, len(models))
	for _, item := range models {
		model := item.(map[string]any)
		providers[str(model, "name", "")] = str(model, "provider", "")
	}
	subagents, err := validatedSubagents(subagentsDoc, names, providers)
	if err != nil {
		return nil, err
	}
	evolutionDoc, err := requireObject(document, "skill_evolution")
	if err != nil {
		return nil, err
	}
	evolution, err := validatedEvolution(evolutionDoc, readObject(data, "skill_evolution"), names, defaultModel, configRevision, document["skill_evolution_migration_ack"])
	if err != nil {
		return nil, err
	}
	candidate := cloneMap(data)
	candidate["config_version"] = configschema.NativeConfigVersion
	candidate["models"] = models
	candidate["default_model"] = defaultModel
	candidate["memory"] = memory
	candidate["sandbox"] = sandbox
	candidate["tools"] = tools
	candidate["subagents"] = subagents
	candidate["skill_evolution"] = evolution
	local := object(candidate, "local_acp")
	delete(local, "subagent_enabled")
	delete(local, "max_concurrent_subagents")
	for key := range runtimeDefaults {
		if value, ok := runtime[key]; ok {
			local[key] = value
		}
	}
	// snapshot exposes this consumed legacy alias through runtime.model_name.
	// Remove its shadow so clearing/renaming the visible setting actually works.
	if api, ok := candidate["api"].(map[string]any); ok {
		api = cloneMap(api)
		delete(api, "model_name")
		candidate["api"] = api
	}
	if _, err := bridgePolicy(candidate); err != nil {
		return nil, err
	}
	sourceDoc, err := requireObject(document, "skill_sources")
	if err != nil {
		return nil, err
	}
	previousSkills := readObject(data, "skills")
	skillsConfig, err := configschema.MergeSkillSources(sourceDoc, previousSkills)
	if err != nil {
		return nil, err
	}
	candidate["skills"] = skillsConfig
	skillsEnabled, ok := document["skills_enabled"].(bool)
	if !ok {
		return nil, errors.New("skills_enabled 必须是布尔值")
	}
	skillsConfig["enabled"] = skillsEnabled
	if skillsEnabled {
		if err := configschema.ValidateSkillSourcesDraft(sourceDoc); err != nil {
			return nil, err
		}
	}
	if !skillsEnabled {
		originalSources, _, err := configschema.ProjectSkillSources(previousSkills)
		if err != nil {
			return nil, err
		}
		if !configschema.SameValue(originalSources, sourceDoc) && (sourceDoc["import_path"] != "" || sourceDoc["export_root"] != "" || len(sourceDoc["managed"].([]any)) > 0) {
			validation := cloneMap(candidate)
			validationSkills := cloneMap(skillsConfig)
			validationSkills["enabled"] = true
			validation["skills"] = validationSkills
			if _, err := launch.InspectConfiguredSkills(s.config, validation); err != nil {
				return nil, err
			}
		}
	}
	if boolValue(evolution, "enabled", false) {
		if !skillsEnabled {
			return nil, errors.New("skill_evolution.enabled requires skills.enabled")
		}
	}
	resolvedSkills, err := launch.InspectConfiguredSkills(s.config, candidate)
	if err != nil {
		return nil, err
	}
	if boolValue(evolution, "enabled", false) {
		destinations, err := launch.SkillProposalDestinations(resolvedSkills, boolValue(evolution, "allow_builtin_overlays", false))
		if err != nil {
			return nil, err
		}
		if err := launch.ValidateGenerationDestinations(boolValue(readObject(evolution, "generation"), "enabled", false), destinations); err != nil {
			return nil, err
		}
		behavior := readObject(evolution, "behavior")
		if boolValue(behavior, "enabled", false) {
			if _, err := launch.LoadBehaviorSuite(s.config, str(behavior, "suite_path", ""), destinations); err != nil {
				return nil, err
			}
		}
	}
	if validateOnly {
		return map[string]any{"valid": true, "migration_required": controls.Legacy}, nil
	}
	backup, err := s.backup(paths)
	if err != nil {
		return nil, err
	}
	if err := writeYAMLAtomic(s.config, candidate); err != nil {
		return nil, fmt.Errorf("保存主配置失败: %w", err)
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
		if err := configschema.ValidateModelDraft(model); err != nil {
			return nil, nil, err
		}
		name := strings.TrimSpace(str(model, "name", ""))
		if name == "" || names[name] {
			return nil, nil, errors.New("模型名称必须非空且唯一")
		}
		use := strings.TrimSpace(str(model, "provider", ""))
		id := strings.TrimSpace(str(model, "model", ""))
		if id == "" {
			return nil, nil, fmt.Errorf("模型 %s 缺少模型 ID", name)
		}
		provider, providerErr := harness.NativeModelProvider(use)
		if providerErr != nil {
			return nil, nil, fmt.Errorf("模型 %s 的提供商不受 Go ACP 支持", name)
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
		raw := map[string]any{}
		if sameBackend {
			// Preserve migration data only on its original backend. An endpoint
			// change must never transfer hidden provider credentials/extensions.
			raw = cloneMap(old)
		}
		raw["name"] = name
		delete(raw, "use")
		raw["provider"] = provider
		raw["model"] = id
		raw["supports_vision"] = model["supports_vision"]
		raw["context_window"] = model["context_window"]
		if parameter, present := model["output_token_parameter"]; present {
			raw["output_token_parameter"] = parameter
		}
		for _, key := range []string{"thinking_parameter", "thinking_budget_tokens"} {
			if value, present := model[key]; present {
				raw[key] = value
			}
		}
		if efforts, present := model["reasoning_efforts"]; present {
			if efforts == nil {
				delete(raw, "reasoning_efforts")
			} else {
				raw["reasoning_efforts"] = efforts
			}
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
	if err := configschema.ValidateDraft("runtime", runtime); err != nil {
		return err
	}
	if selected := str(runtime, "model_name", ""); selected != "" && !models[selected] {
		return errors.New("ACP 模型必须引用已配置模型")
	}
	return nil
}

func validatedMemory(document, previous, runtime map[string]any) (map[string]any, error) {
	if err := configschema.ValidateDraft("memory", document); err != nil {
		return nil, err
	}
	if err := configschema.ValidateMemoryPolicy(str(runtime, "memory_scope", ""), str(document, "mode", ""), boolValue(document, "enabled", true), boolValue(document, "retrieval_enabled", true), boolValue(document, "include_global", true)); err != nil {
		return nil, err
	}
	// Keep original unknown data until explicit migration. It is neither exposed
	// nor editable through this projection, and no legacy defaults are injected.
	raw := cloneMap(previous)
	for key := range memoryDefaults {
		raw[key] = document[key]
	}
	// Canonicalize the one consumed legacy alias; otherwise it would shadow the
	// newly saved native field at launch.
	if backend, ok := raw["backend_config"].(map[string]any); ok {
		backend = cloneMap(backend)
		raw["backend_config"] = backend
		delete(backend, "retrieval_enabled")
		if len(backend) == 0 {
			delete(raw, "backend_config")
		}
	}
	return raw, nil
}

func validatedSandbox(document, previous, runtime map[string]any) (map[string]any, error) {
	if err := configschema.ValidateDraft("sandbox", document); err != nil {
		return nil, err
	}
	raw := cloneMap(previous)
	delete(raw, "use")
	for key := range configschema.Defaults("sandbox") {
		raw[key] = document[key]
	}
	if boolValue(runtime, "enable_bash", false) && !boolValue(raw, "allow_host_bash", false) {
		return nil, errors.New("启用 bash 时需要允许宿主命令")
	}
	return raw, nil
}

func validatedSubagents(document map[string]any, models map[string]bool, providers map[string]string) (map[string]any, error) {
	if err := configschema.ValidateDraft("subagents", document); err != nil {
		return nil, err
	}
	encoded, err := yaml.Marshal(document)
	if err != nil {
		return nil, errors.New("subagents 配置无法编码")
	}
	config, err := agents.DecodeConfig(encoded, models)
	if err != nil {
		return nil, err
	}
	if err := config.ValidateModelOptions(providers, harness.DefaultBudgetLimits().MaxOutputTokens); err != nil {
		return nil, err
	}
	return cloneMap(document), nil
}
func validatedEvolution(document, previous map[string]any, models map[string]bool, defaultModel, revision string, acknowledgment any) (map[string]any, error) {
	original, ignored, err := configschema.ProjectEvolution(previous)
	if err != nil {
		return nil, err
	}
	ack, ok := acknowledgment.(string)
	if acknowledgment != nil && (!ok || ack != "" && ack != revision) {
		return nil, errors.New("skill_evolution 旧设置转换确认已过期，请重新加载并查看转换说明")
	}
	convert := ack != "" && ack == revision
	dormant := original
	if convert {
		dormant = nil
	}
	if err := configschema.ValidateEvolutionDraft(document, dormant); err != nil {
		return nil, err
	}
	enabled := document["enabled"] == true
	if enabled && len(ignored) > 0 && !convert {
		return nil, errors.New("skill_evolution 含当前运行时不读取的旧设置；请先确认转换为人工审核配置")
	}
	for _, key := range []string{"moderation_model_name", "evaluation_model_name", "generation_model_name"} {
		selected := str(document, key, "")
		active := enabled && (key != "generation_model_name" || boolValue(readObject(document, "generation"), "enabled", false))
		if selected != "" && !models[selected] && (active || convert || !configschema.SameValue(document[key], original[key])) {
			return nil, fmt.Errorf("skill_evolution.%s 引用了未知模型", key)
		}
	}
	if convert && document["security_fail_closed"] != true {
		return nil, errors.New("skill_evolution.security_fail_closed 必须为 true")
	}
	learning, oldLearning := readObject(document, "learning"), readObject(original, "learning")
	selected := str(learning, "extraction_model_name", "")
	if selected != "" && !models[selected] && (enabled && boolValue(learning, "enabled", false) || convert || !configschema.SameValue(learning["extraction_model_name"], oldLearning["extraction_model_name"])) {
		return nil, errors.New("skill_evolution.learning.extraction_model_name references an unknown model")
	}
	selected = str(learning, "consolidation_model_name", "")
	if selected != "" && !models[selected] && (enabled && boolValue(learning, "enabled", false) && boolValue(learning, "consolidation_enabled", false) || convert || !configschema.SameValue(learning["consolidation_model_name"], oldLearning["consolidation_model_name"])) {
		return nil, errors.New("skill_evolution.learning.consolidation_model_name references an unknown model")
	}
	selected = str(learning, "matching_model_name", "")
	if selected != "" && !models[selected] && (enabled && boolValue(learning, "enabled", false) && boolValue(learning, "matching_enabled", false) || convert || !configschema.SameValue(learning["matching_model_name"], oldLearning["matching_model_name"])) {
		return nil, errors.New("skill_evolution.learning.matching_model_name references an unknown model")
	}
	raw := cloneMap(previous)
	if convert {
		raw = map[string]any{}
	}
	for key, value := range document {
		if key == "validation" || key == "generation" || key == "learning" || key == "discovery" || key == "behavior" || key == "auto_patch" || key == "budget" {
			limits := cloneMap(readObject(raw, key))
			for name, limit := range value.(map[string]any) {
				limits[name] = limit
			}
			raw[key] = limits
		} else {
			raw[key] = value
		}
	}
	encoded, err := yaml.Marshal(raw)
	if err != nil {
		return nil, errors.New("skill_evolution 配置无法编码")
	}
	if _, err = evolution.DecodeReviewConfig(encoded, models, defaultModel); err != nil {
		return nil, err
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

	return backup, nil
}

func (s service) rollback(paths configPaths, backup string, cause error) error {
	for _, path := range []string{s.config, paths.extensions} {
		body, err := os.ReadFile(filepath.Join(backup, filepath.Base(path)))
		if err == nil {
			_ = writeAtomic(path, body)
		}
	}
	return fmt.Errorf("保存失败，已尝试恢复保存前配置；备份位于 %s: %w", backup, cause)
}
