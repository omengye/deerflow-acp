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

type artifactInput struct {
	Paths []string `json:"paths" jsonschema:"required,description=One to sixteen paths inside .deerflow/outputs in the selected workspace"`
}

type artifactTool struct {
	info  *schema.ToolInfo
	stage func(context.Context, string, []string) ([]harness.Content, error)
}

// PresentFilesTool stages immutable output snapshots. The runtime commits the
// staged assets together with the terminal tool event and receipt before any
// reference is delivered to ACP. Stage itself must not publish client events.
func PresentFilesTool(stage func(context.Context, string, []string) ([]harness.Content, error)) (tool.InvokableTool, error) {
	if stage == nil {
		return nil, errors.New("artifact staging backend is required")
	}
	info, err := utils.GoStruct2ToolInfo[artifactInput]("present_files", "Present completed output files to the user. First create files inside .deerflow/outputs in the selected workspace, then provide their paths. This snapshots existing files and records stable artifact references; it does not execute, upload, or fetch remote files. /mnt/user-data/outputs/... is an alias for that directory.")
	if err != nil {
		return nil, err
	}
	return &artifactTool{info: info, stage: stage}, nil
}

func (t *artifactTool) Info(context.Context) (*schema.ToolInfo, error) { return t.info, nil }
func (t *artifactTool) InvokableRun(ctx context.Context, args string, _ ...tool.Option) (string, error) {
	if len(args) > 64<<10 {
		return "", harness.ErrInvalidInput
	}
	var in artifactInput
	dec := json.NewDecoder(strings.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return "", harness.ErrInvalidInput
	}
	var extra any
	if dec.Decode(&extra) != io.EOF || len(in.Paths) == 0 || len(in.Paths) > 16 {
		return "", harness.ErrInvalidInput
	}
	for _, p := range in.Paths {
		if strings.TrimSpace(p) == "" || len(p) > 4096 || strings.ContainsRune(p, 0) {
			return "", harness.ErrInvalidInput
		}
	}
	callID := compose.GetToolCallID(ctx)
	if callID == "" {
		return "", errors.New("artifact publication requires an active tool call")
	}
	content, err := t.stage(ctx, callID, in.Paths)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(struct {
		Artifacts []harness.Content `json:"artifacts"`
	}{content})
	return string(data), err
}
