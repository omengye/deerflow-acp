package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// inferTool keeps Eino's generated schema while making argument rejection a
// verified pre-execution failure. The decoder runs before the native function.
func inferTool[T, D any](name, description string, invoke utils.InvokeFunc[T, D]) (tool.InvokableTool, error) {
	// These native functions only read workspace data or issue HTTP GETs. Their
	// live terminal errors prove no mutation; an unrelated SDK tool with the
	// same name does not acquire this contract.
	wrapped := func(ctx context.Context, input T) (D, error) {
		result, err := invoke(ctx, input)
		switch name {
		case "ls", "read_file", "glob", "grep", "read_tool_output", "search_memory", "web_search", "web_fetch", "image_search":
			err = harness.MarkToolNoEffect(err)
		}
		return result, err
	}
	return utils.InferTool(name, description, wrapped, utils.WithUnmarshalArguments(func(_ context.Context, raw string) (any, error) {
		var input T
		if !strings.HasPrefix(strings.TrimSpace(raw), "{") {
			return nil, harness.MarkToolNotExecuted(errors.New("tool input must be exactly one JSON object"))
		}
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			return nil, harness.MarkToolNotExecuted(err)
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return nil, harness.MarkToolNotExecuted(errors.New("tool input must be exactly one JSON object"))
		}
		return input, nil
	}))
}
