package desktopconfig

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/configschema"
	"github.com/omengye/deerflow-acp/go-harness/internal/launch"
	"github.com/omengye/deerflow-acp/go-harness/internal/opencli"
	"github.com/omengye/deerflow-acp/go-harness/internal/websearch"
	"go.yaml.in/yaml/v3"
)

// Checks only saved configuration and local dependencies. No commands, model
// calls, browser connections or network requests run as a side effect.
func (s service) readiness() (map[string]any, error) {
	file, err := os.Open(s.config)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, 2<<20+1))
	if err != nil {
		return nil, err
	}
	if len(body) > 2<<20 {
		return nil, fmt.Errorf("configuration exceeds 2 MiB")
	}
	var data map[string]any
	if err = yaml.Unmarshal(body, &data); err != nil || data == nil {
		return nil, fmt.Errorf("invalid configuration YAML")
	}
	// Hash the same bytes we inspected, not a second possibly newer file read.
	return map[string]any{"config_revision": fmt.Sprintf("%x", sha256.Sum256(body)), "network_checked": false, "items": capabilityReadinessForConfig(data, s.config, s.configValues(data))}, nil
}

func hasCredential(values launch.ConfigValues, value string) bool {
	resolved, err := values.Resolve(value)
	return err == nil && strings.TrimSpace(resolved) != "" && resolved != "not-configured" && resolved != redacted
}

func capabilityReadiness(data map[string]any, configDir string, values launch.ConfigValues) []map[string]string {
	return capabilityReadinessWithSearch(data, configDir, filepath.Join(configDir, "config.yaml"), values)
}

func capabilityReadinessForConfig(data map[string]any, configPath string, values launch.ConfigValues) []map[string]string {
	return capabilityReadinessWithSearch(data, filepath.Dir(configPath), configPath, values)
}

func capabilityReadinessWithSearch(data map[string]any, configDir, configPath string, values launch.ConfigValues) []map[string]string {
	items := []map[string]string{}
	add := func(id, state, reason, section string) {
		items = append(items, map[string]string{"id": id, "state": state, "reason": reason, "section": section})
	}
	local := readObject(data, "local_acp")
	selected := str(local, "model_name", "")
	if selected == "" {
		selected = str(readObject(data, "api"), "model_name", "")
	}
	if selected == "" {
		selected = str(data, "default_model", "")
	}
	var model map[string]any
	for _, item := range list(data, "models") {
		raw, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if selected == "" || str(raw, "name", "") == selected {
			model = raw
			break
		}
	}
	if model == nil || str(model, "model", "") == "" {
		add("model", "not_configured", "请选择默认模型并填写模型 ID。", "models")
	} else if _, err := harness.NativeModelProvider(nativeProvider(model)); err != nil {
		add("model", "not_configured", "模型 Provider 不受支持。", "models")
	} else if !hasCredential(values, str(model, "api_key", "")) {
		add("model", "missing_credential", "已保存模型的密钥为空、占位值或环境变量未设置；无鉴权端点可主动测试确认。", "models")
	} else {
		add("model", "unverified", "本地配置具备凭据；请主动运行模型连接或 Agent 能力测试，可能产生费用。", "models")
	}
	policy := harness.ToolPolicy{}
	if raw, ok := local["tool_allowlist"].([]any); ok {
		policy.Allowlist = []string{}
		for _, v := range raw {
			if name, ok := v.(string); ok {
				policy.Allowlist = append(policy.Allowlist, name)
			}
		}
		policy.Allowlist = configschema.NormalizeToolNames(policy.Allowlist)
	}
	for _, v := range list(local, "tool_denylist") {
		if name, ok := v.(string); ok {
			policy.Denylist = append(policy.Denylist, name)
		}
	}
	policy.Denylist = configschema.NormalizeToolNames(policy.Denylist)
	configured := map[string]map[string]any{}
	for _, v := range list(data, "tools") {
		if tool, ok := v.(map[string]any); ok {
			configured[str(tool, "name", "")] = tool
		}
	}
	sandbox := readObject(data, "sandbox")
	for _, name := range []string{"web_search", "host_opencli"} {
		tool := configured[name]
		if !policy.Allows(name) || name == "host_opencli" && !boolValue(sandbox, "allow_host_tools", false) {
			add(name, "not_authorized", "当前宿主工具策略未授权；不会自动开启。", "tools")
			continue
		}
		if tool == nil {
			add(name, "not_configured", "尚未添加工具配置。", "tools")
			continue
		}
		if name == "web_search" {
			body, _ := yaml.Marshal(data)
			seed, seedErr := websearch.SeedYAML(body)
			manager, managerErr := websearch.New(filepath.Join(configDir, "search-providers.json"), seed)
			if seedErr != nil || managerErr != nil {
				add(name, "not_configured", "搜索服务配置无效，请在设置 → 工具 → web_search 中重新配置。", "tools")
				continue
			}
			attachSearchSecretCheck(configPath, manager)
			status, statusErr := manager.Status()
			if statusErr != nil {
				add(name, "not_configured", "无法读取搜索服务配置，请在设置 → 工具 → web_search 中检查。", "tools")
				continue
			}
			for _, provider := range status.Providers {
				if provider.Name != status.DefaultProvider {
					continue
				}
				if !provider.Configured {
					state := "missing_credential"
					reason := "请在设置 → 工具 → web_search 中为 " + provider.Label + " 配置 API Key。"
					if provider.Name == "searxng" {
						state = "not_configured"
						reason = "请填写 SearXNG 实例地址；Key 选填。"
					}
					add(name, state, reason, "tools")
				} else {
					add(name, "unverified", provider.Label+" 已配置；本地检查不验证网络、额度或实际搜索。", "tools")
				}
			}
		} else {
			executable, err := values.Resolve(str(tool, "executable", ""))
			if err == nil {
				if strings.ContainsAny(executable, "/\\") && !filepath.IsAbs(executable) {
					executable = filepath.Join(configDir, executable)
				}
				_, err = opencli.Resolve(executable)
			}
			if err != nil {
				add(name, "missing_dependency", "未找到可用 OpenCLI 启动器或 Node.js 依赖；请检查安装与路径。", "tools")
			} else {
				add(name, "unverified", "已找到本地启动器；未执行 OpenCLI，也未验证版本、浏览器桥接或登录态。", "tools")
			}
		}
	}
	if !boolValue(local, "enable_bash", false) || !boolValue(sandbox, "allow_host_bash", false) || !policy.Allows("execute") {
		add("commands", "not_authorized", "命令执行未获全部授权；开启后仍受会话审批约束。", "tools")
	} else {
		if runtime.GOOS != "windows" || str(sandbox, "provider", "") != "local" && str(sandbox, "use", "") != "local" && str(sandbox, "use", "") != "deerflow.sandbox.local:LocalSandboxProvider" {
			add("commands", "not_configured", "桌面自动命令映射需要 Windows 和 local provider；自定义命令行覆盖不在此检查范围。", "tools")
		} else if !desktopShellAvailable() {
			add("commands", "missing_dependency", "未找到 PowerShell 7 或系统 Windows PowerShell。", "tools")
		} else {
			add("commands", "ready", "本机 shell 与授权检查通过；未执行命令，不代表安全隔离。", "tools")
		}
	}
	return items
}

func desktopShellAvailable() bool {
	if _, err := exec.LookPath("pwsh"); err == nil {
		return true
	}
	info, err := os.Stat(filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe"))
	return err == nil && info.Mode().IsRegular()
}
