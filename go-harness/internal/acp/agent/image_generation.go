package agent

import (
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
)

func imageGenerationError(failure *harness.ImageGenerationError) *protocol.Error {
	kind, message := failure.Kind, "服务商未完成图片生成。请检查本轮结果后再重试；图片生成设置未被修改。"
	switch kind {
	case "unsupported_complexity":
		message = "服务商的图片模型不支持请求中的复杂度参数。请核对服务商的图片模型配置或切换服务商；本轮未自动重试，生图开关和对话推理强度未被修改。"
	case "unsupported":
		message = "服务商拒绝了图片生成工具：当前端点或模型不支持此请求。请核对服务商的 Responses 生图支持，或切换模型。"
	case "access_denied":
		message = "服务商拒绝了图片生成权限。请检查当前账号和模型的访问权限；图片生成设置未被修改。"
	case "quota_exceeded":
		message = "服务商报告图片生成额度不足。请检查账号额度后再重试；图片生成设置未被修改。"
	default:
		kind = "failed"
	}
	return &protocol.Error{Code: -32032, Message: message, Data: map[string]any{"kind": "image_generation_failed", "reason": kind, "automaticReplay": false}}
}
