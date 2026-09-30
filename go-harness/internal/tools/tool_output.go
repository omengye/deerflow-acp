package tools

import (
	"context"
	"errors"

	"github.com/cloudwego/eino/components/tool"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type toolOutputReadInput struct {
	ID       string `json:"id" jsonschema:"required,description=Opaque snapshot ID returned with an oversized tool result"`
	Offset   int64  `json:"offset" jsonschema:"description=Byte offset from a prior nextOffset; defaults to 0"`
	MaxBytes int    `json:"max_bytes" jsonschema:"description=Maximum UTF-8 bytes to read; capped at 4096"`
}

// ReadToolOutputTool gives the model bounded access to a session-owned result.
// The store validates the ID against the current session and workspace on each
// invocation; this tool never accepts a filesystem path.
func ReadToolOutputTool(store harness.ToolOutputStore, x harness.Session) (tool.InvokableTool, error) {
	if store == nil {
		return nil, errors.New("tool output store is required")
	}
	return inferTool("read_tool_output", "Read one bounded chunk of an oversized tool result snapshot. Supply the returned opaque ID and use nextOffset to continue. This reads only snapshots from the current session, not arbitrary files.", func(ctx context.Context, in toolOutputReadInput) (harness.ToolOutputChunk, error) {
		return store.ReadToolOutput(ctx, x, in.ID, in.Offset, in.MaxBytes)
	})
}
