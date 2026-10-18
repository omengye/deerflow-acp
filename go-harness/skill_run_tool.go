package deerflow

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	einoengine "github.com/omengye/deerflow-acp/go-harness/internal/engine/eino"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillrun"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

type skillRunTool struct {
	info     *schema.ToolInfo
	snapshot *skills.Snapshot
	backend  harness.CommandBackend
	policy   harness.SandboxConfig
	req      harness.RunRequest
}

type preparedSkillRun struct {
	skillrun.Input
	PinnedVersion string `json:"pinned_version"`
}

func newSkillRunTool(snapshot *skills.Snapshot, backend harness.CommandBackend, policy harness.SandboxConfig, req harness.RunRequest) (*skillRunTool, error) {
	info, err := utils.GoStruct2ToolInfo[skillrun.Input]("skill_run", "Check Python readiness or execute a packaged .py entrypoint from this session's immutable skill version. check runs only a host-owned standard-library metadata probe, never skill code or installs. run checks dependencies then executes pinned files through the configured command backend and existing permission policy. Missing dependencies do not unpublish a skill. No automatic pip installs. Local/PowerShell/WSL are not security isolation. Results distinguish readiness, command exit and termination. Use skill-runtime.json for min_version, distribution packages and entrypoints. Arguments and file outputs are relative to the session workspace; never execute skill:// or a mutable installation path.")
	if err != nil {
		return nil, err
	}
	return &skillRunTool{info, snapshot, backend, policy, req}, nil
}

func (t *skillRunTool) Info(context.Context) (*schema.ToolInfo, error) { return t.info, nil }

func (t *skillRunTool) authorized(ctx context.Context, name string) (string, error) {
	if t.req.Session.Mode == "plan" || t.req.Session.ApprovalMode == harness.ApprovalReadOnly || compose.GetToolCallID(ctx) == "" {
		return "", harness.ErrPermissionDenied
	}
	scope, ok := budget.ScopeFromContext(ctx)
	root := t.req.RootBudgetID
	if root == "" {
		root = t.req.RunID
	}
	if !ok || scope.RootBudgetID != root || scope.MemberID != t.req.RunID || scope.SessionID != t.req.Session.ID || scope.AttemptID == "" {
		return "", harness.ErrPermissionDenied
	}
	role, active, err := einoengine.CurrentRole(ctx)
	if err != nil || active && (role.RootID != root || !role.Spec.Tools.Allows("skill_run") || !role.Spec.Tools.Allows("execute") || !slices.Contains(role.Spec.Skills, name)) {
		return "", harness.ErrPermissionDenied
	}
	for _, ref := range t.snapshot.Refs() {
		if ref.Name == name {
			return ref.Version, nil
		}
	}
	return "", harness.ErrPermissionDenied
}

func (t *skillRunTool) PrepareToolArguments(ctx context.Context, args string) (string, error) {
	var in skillrun.Input
	if len(args) > 65536 || decodeBackgroundArguments(args, &in) != nil || skillrun.ValidateInput(in) != nil {
		return "", harness.ErrInvalidInput
	}
	v, err := t.authorized(ctx, in.Name)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(preparedSkillRun{in, v})
	return string(data), err
}

func (t *skillRunTool) InvokableRun(ctx context.Context, args string, _ ...tool.Option) (string, error) {
	var in preparedSkillRun
	if len(args) > 65536 || decodeBackgroundArguments(args, &in) != nil {
		return "", harness.MarkToolNotExecuted(harness.ErrInvalidInput)
	}
	v, err := t.authorized(ctx, in.Name)
	if err != nil || v != in.PinnedVersion {
		return "", harness.MarkToolNotExecuted(harness.ErrPermissionDenied)
	}
	r, err := skillrun.Run(ctx, t.snapshot, t.backend, t.policy, t.req.Session.CWD, in.Input)
	b, _ := json.Marshal(r)
	if err != nil {
		return string(b), &skillRunError{err, string(b)}
	}
	return string(b), nil
}

type skillRunError struct {
	error
	result string
}

func (e *skillRunError) Unwrap() error      { return e.error }
func (e *skillRunError) ToolResult() string { return e.result }
