package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type imageInput struct {
	Path string `json:"path" jsonschema:"required,description=Image file within this session's workspace"`
}
type imageTool struct {
	info *schema.ToolInfo
	load func(context.Context, string, string) (*schema.ToolResult, error)
}

// ViewImageTool returns only a staged canonical asset reference. Its caller
// commits that snapshot with the terminal receipt, then hydrates it immediately
// before a vision model invocation. The tool never downloads a URL.
func ViewImageTool(load func(context.Context, string, string) (*schema.ToolResult, error)) (tool.EnhancedInvokableTool, error) {
	if load == nil {
		return nil, errors.New("image backend is required")
	}
	info, err := utils.GoStruct2ToolInfo[imageInput]("view_image", "Inspect a PNG, JPEG, GIF, or WebP file from the selected workspace with the current vision model. The file must be at most 20 MiB. Remote URLs are not downloaded. This is a read-only operation; it does not publish an output artifact.")
	if err != nil {
		return nil, err
	}
	return &imageTool{info: info, load: load}, nil
}
func (t *imageTool) Info(context.Context) (*schema.ToolInfo, error) { return t.info, nil }
func (t *imageTool) InvokableRun(ctx context.Context, args *schema.ToolArgument, _ ...tool.Option) (*schema.ToolResult, error) {
	if args == nil || len(args.Text) > 8192 {
		return nil, harness.ErrInvalidInput
	}
	var in imageInput
	dec := json.NewDecoder(strings.NewReader(args.Text))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return nil, harness.ErrInvalidInput
	}
	var extra any
	if dec.Decode(&extra) != io.EOF || strings.TrimSpace(in.Path) == "" || len(in.Path) > 4096 || strings.ContainsRune(in.Path, 0) {
		return nil, harness.ErrInvalidInput
	}
	callID := compose.GetToolCallID(ctx)
	if callID == "" {
		return nil, errors.New("image inspection requires an active tool call")
	}
	return t.load(ctx, callID, in.Path)
}
