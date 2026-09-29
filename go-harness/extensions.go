package deerflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"

	"github.com/cloudwego/eino/adk"
	einoskill "github.com/cloudwego/eino/adk/middlewares/skill"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/assets"
	einoengine "github.com/omengye/deerflow-acp/go-harness/internal/engine/eino"
	"github.com/omengye/deerflow-acp/go-harness/internal/mcp"
	"github.com/omengye/deerflow-acp/go-harness/internal/sandbox"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
	"github.com/omengye/deerflow-acp/go-harness/internal/tools"
)

// No credentials or mutable source text enter the persisted execution state.
type extensionState struct {
	Version       int                `json:"version"`
	Skills        []harness.SkillRef `json:"skills"`
	MCPGeneration string             `json:"mcpGeneration,omitempty"`
	SandboxPolicy string             `json:"sandboxPolicy"`
}

func extensionFactory(cfg Config, manager *mcp.Manager, registry *skills.Registry, assetStore *assets.Store) func(context.Context, harness.RunRequest, json.RawMessage) (einoengine.RunExtensions, error) {
	selectionPolicy := cfg.SkillSelection
	selectionPolicy.Names = slices.Clone(selectionPolicy.Names)
	commandPolicy := cfg.Sandbox
	commandPolicy.AllowedExecutables = slices.Clone(commandPolicy.AllowedExecutables)
	commandPolicy.Docker.AllowedImages = slices.Clone(commandPolicy.Docker.AllowedImages)
	commandPolicy.Environment = maps.Clone(commandPolicy.Environment)
	encoded, _ := json.Marshal(commandPolicy)
	policyHash := fmt.Sprintf("%x", sha256.Sum256(encoded))
	return func(ctx context.Context, req harness.RunRequest, pinned json.RawMessage) (out einoengine.RunExtensions, err error) {
		selection := selectionPolicy
		selection.Workspace = req.Session.CWD
		readOnly := req.Session.Mode == "plan" || req.Session.ApprovalMode == harness.ApprovalReadOnly
		state := extensionState{Version: 1, SandboxPolicy: policyHash}
		if !readOnly {
			state.MCPGeneration, err = manager.Generation(ctx, req.Session.ID)
			if err != nil {
				return out, err
			}
		}
		var snapshot *skills.Snapshot
		if pinned != nil {
			var previous extensionState
			dec := json.NewDecoder(bytes.NewReader(pinned))
			dec.DisallowUnknownFields()
			if err = dec.Decode(&previous); err != nil {
				return out, fmt.Errorf("%w: invalid extension checkpoint", harness.ErrInvalidInput)
			}
			var extra any
			if dec.Decode(&extra) != io.EOF || previous.Version != state.Version || previous.MCPGeneration != state.MCPGeneration || previous.SandboxPolicy != state.SandboxPolicy {
				return out, fmt.Errorf("%w: execution resources changed since checkpoint", harness.ErrInvalidInput)
			}
			snapshot, err = registry.Restore(ctx, selection, previous.Skills)
		} else {
			snapshot, err = registry.Snapshot(ctx, selection)
		}
		if err != nil {
			return out, err
		}
		state.Skills = snapshot.Refs()
		out.State, err = json.Marshal(state)
		if err != nil {
			return out, err
		}
		if readOnly {
			req.Session.Mode = "plan"
		}
		out.Tools, out.Cleanup, err = tools.WorkspaceFactory(ctx, req)
		if err != nil {
			return out, err
		}
		if len(state.Skills) > 0 {
			handler, err := einoskill.NewMiddleware(ctx, &einoskill.Config{Backend: snapshot, BuildContent: snapshot.BuildContent})
			if err != nil {
				return out, err
			}
			out.Tools = append(out.Tools, snapshot.ReadFileTool())
			out.Handlers = []adk.ChatModelAgentMiddleware{handler}
		}
		if assetStore != nil && cfg.Media.SupportsVision(req.Session.Model) {
			view, err := tools.ViewImageTool(func(ctx context.Context, callID, path string) (*schema.ToolResult, error) {
				content, err := assetStore.StageImage(ctx, req.Session, req.RunID, callID, path)
				if err != nil {
					return nil, err
				}
				return einoengine.ProjectToolContent([]harness.Content{content})
			})
			if err != nil {
				return out, err
			}
			out.Tools = append(out.Tools, view)
		}
		if readOnly {
			return out, nil
		}
		if assetStore != nil {
			present, err := tools.PresentFilesTool(func(ctx context.Context, callID string, paths []string) ([]harness.Content, error) {
				return assetStore.StageArtifacts(ctx, req.Session, req.RunID, callID, paths)
			})
			if err != nil {
				return out, err
			}
			out.Tools = append(out.Tools, present)
		}
		remote, err := manager.Tools(ctx, req.Session.ID)
		if err != nil {
			return out, err
		}
		out.Tools = append(out.Tools, remote...)
		if !commandPolicy.Enabled {
			return out, nil
		}
		commands, err := sandbox.New(ctx, commandPolicy, req.Session.CWD)
		if err != nil {
			return out, err
		}
		workspaceCleanup := out.Cleanup
		out.Cleanup = func() error { return errors.Join(commands.Close(), workspaceCleanup()) }
		command, err := tools.CommandTool(commands, commandPolicy.Provider)
		if err != nil {
			return out, err
		}
		out.Tools = append(out.Tools, command)
		return out, nil
	}
}
