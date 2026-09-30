package desktopconfig

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

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
	provider := "openai"
	if str(model, "use", "") == "langchain_anthropic:ChatAnthropic" {
		provider = "claude"
	}
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
	err = eino.ProbeModel(ctx, provider, str(model, "model", ""), baseURL, apiKey)
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
