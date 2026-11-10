package agent

import (
	"errors"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
)

func checkpointError(err error) *protocol.Error {
	code, kind := -32030, "checkpoint_persist_failed"
	message := "执行进度保存失败，本轮已停止。请检查本机存储后重试；已保存的记录会保留。"
	switch {
	case errors.Is(err, harness.ErrCheckpointTooLarge):
		code, kind = -32028, "checkpoint_too_large"
		message = "执行状态过大，无法保存本轮进度。请减少并行子任务后发送新消息继续；已保存的工具结果会保留。"
	case errors.Is(err, harness.ErrCheckpointInvalid):
		code, kind = -32029, "checkpoint_invalid"
		message = "执行状态校验失败，暂时无法恢复。本轮已停止，已保存的记录会保留；请发送新消息继续。"
	}
	data := map[string]any{"kind": kind, "automaticReplay": false}
	var size *harness.CheckpointSizeError
	if errors.As(err, &size) {
		data["bytes"], data["limit"] = size.Bytes, size.Limit
		if size.Representation == "raw" || size.Representation == "stored" {
			data["representation"] = size.Representation
		}
	}
	return &protocol.Error{Code: code, Message: message, Data: data}
}
