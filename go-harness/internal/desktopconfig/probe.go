package desktopconfig

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/engine/eino"
)

func (s service) testModel(request map[string]any) (map[string]any, error) {
	incoming, ok := request["model"].(map[string]any)
	if !ok {
		return nil, errors.New("Missing model configuration")
	}
	data, err := readYAML(s.config)
	if err != nil {
		return nil, err
	}
	models, _, err := validatedModels([]any{incoming}, list(data, "models"))
	if err != nil {
		return nil, err
	}
	model := models[0].(map[string]any)
	provider := nativeProvider(model)
	parameter := harness.OutputTokenParameter(str(model, "output_token_parameter", "auto"))
	apiKey, err := resolveSecret(str(model, "api_key", ""))
	if err != nil {
		return map[string]any{"ok": false, "error_type": "MissingCredential", "note": "请求失败，请检查凭据、地址、模型 ID 和网络；测试最长 30 秒"}, nil
	}
	baseURL, err := resolveSecret(str(model, "base_url", ""))
	if err != nil {
		return map[string]any{"ok": false, "error_type": "InvalidEndpoint", "note": "请求失败，请检查凭据、地址、模型 ID 和网络；测试最长 30 秒"}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	started := time.Now()
	profile, _ := request["profile"].(string)
	if profile != "" && profile != "text" && profile != "agent" {
		return nil, errors.New("invalid model probe profile")
	}
	if profile == "agent" {
		capabilities, err := eino.ProbeAgentCapabilities(ctx, provider, str(model, "model", ""), baseURL, apiKey, parameter)
		if err != nil {
			return map[string]any{"ok": false, "error_type": "ModelRequestError", "note": "无法构建模型能力测试，请检查配置"}, nil
		}
		return map[string]any{"ok": capabilities.Streaming.OK && capabilities.Tools.OK, "latency_ms": time.Since(started).Milliseconds(), "capabilities": capabilities, "note": "最多两次请求；工具不执行；usage 缺失仍按估算记账；未测试图片和 thinking"}, nil
	}
	err = eino.ProbeModel(ctx, provider, str(model, "model", ""), baseURL, apiKey, parameter)
	if err != nil {
		errorType := "ModelRequestError"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			errorType = "TimeoutError"
		}
		return map[string]any{"ok": false, "error_type": errorType, "note": "请求失败，请检查凭据、地址、模型 ID 和网络；测试最长 30 秒"}, nil
	}
	return map[string]any{"ok": true, "latency_ms": time.Since(started).Milliseconds(), "note": "模型文本请求成功；能力声明仍需按服务商说明设置"}, nil
}

func resolveSecret(value string) (string, error) {
	if strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}") {
		body := value[2 : len(value)-1]
		if key, fallback, ok := strings.Cut(body, ":-"); ok {
			if current := os.Getenv(key); current != "" {
				return current, nil
			}
			return fallback, nil
		}
		value = "$" + body
	}
	if strings.HasPrefix(value, "$") {
		key := strings.TrimPrefix(value, "$")
		if key == "" || strings.ContainsAny(key, "${} ") {
			return "", errors.New("invalid environment reference")
		}
		current, ok := os.LookupEnv(key)
		if !ok {
			return "", errors.New("missing environment variable")
		}
		return current, nil
	}
	return value, nil
}
