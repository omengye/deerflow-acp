package tools

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/cloudwego/eino/schema"
)

const workspaceReadDescription = "Read a UTF-8 text file inside the workspace. Without offset/limit the whole file is returned (at most 512 KiB). With offset (1-based line) and/or limit (lines) a line range is returned from files up to 8 MiB, with startLine, endLine and totalLines. Returns version; pass it as expected_version when writing or editing."
const legacyWorkspaceReadDescription = "Read a UTF-8 text file inside the workspace, at most 512 KiB. Returns content and version; pass version as expected_version when writing or editing."

// LegacyWorkspaceReadInfo recognizes only the exact host read_file schema whose
// change added optional line ranges. Old path-only calls retain the same root,
// size bound, read-only behavior and version result. This is an explicit schema
// migration, not permission to ignore arbitrary tool/schema/policy changes.
// The caller must separately verify that this is a trusted builtin.
func LegacyWorkspaceReadInfo(ctx context.Context, info *schema.ToolInfo) (*schema.ToolInfo, error) {
	if info == nil || info.Name != "read_file" {
		return nil, nil
	}
	current, err := inferTool("read_file", workspaceReadDescription, func(context.Context, readInput) (string, error) { return "", nil })
	if err != nil {
		return nil, err
	}
	expected, err := current.Info(ctx)
	if err != nil {
		return nil, err
	}
	actualJSON, err := json.Marshal(info)
	if err != nil {
		return nil, err
	}
	expectedJSON, err := json.Marshal(expected)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(actualJSON, expectedJSON) {
		return nil, nil
	}
	legacy, err := inferTool("read_file", legacyWorkspaceReadDescription, func(context.Context, pathInput) (string, error) { return "", nil })
	if err != nil {
		return nil, err
	}
	return legacy.Info(ctx)
}
