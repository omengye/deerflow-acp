package skills

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
)

func (s *Snapshot) ReadFileTool() tool.InvokableTool { return &readFileTool{snapshot: s} }

type readFileTool struct{ snapshot *Snapshot }

func (t *readFileTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{
		Name: "read_skill_file", Desc: "Read a UTF-8 support file from an authorized immutable skill snapshot. Use the skill name and a relative path such as references/guide.md. skill:// references are virtual; they cannot be opened with workspace or shell tools. This tool never writes or executes files.",
		ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{"skill": {Type: schema.String, Required: true, Desc: "Enabled skill name in this run's snapshot"}, "path": {Type: schema.String, Required: true, Desc: "File path relative to that skill; no absolute paths, parent traversal or URLs"}}),
	}, nil
}
func (t *readFileTool) InvokableRun(ctx context.Context, args string, _ ...tool.Option) (string, error) {
	if len(args) > 4096 {
		return "", invalid("read_skill_file arguments exceed limit")
	}
	var in struct {
		Skill string `json:"skill"`
		Path  string `json:"path"`
	}
	d := json.NewDecoder(strings.NewReader(args))
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil {
		return "", invalid("read_skill_file arguments are invalid")
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return "", invalid("read_skill_file requires one JSON object")
	}
	data, err := t.snapshot.ReadFile(ctx, in.Skill, in.Path)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return "", invalid("binary skill files cannot be rendered as text")
	}
	return string(data), nil
}
