package deerflow

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	durablebudget "github.com/omengye/deerflow-acp/go-harness/internal/budget"
	einoengine "github.com/omengye/deerflow-acp/go-harness/internal/engine/eino"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

type skillInspectInput struct {
	Name string `json:"name" jsonschema:"required"`
}
type skillEditInput struct {
	Action string                  `json:"action" jsonschema:"required,enum=prepare,enum=apply,enum=status"`
	Name   string                  `json:"name,omitempty" jsonschema:"description=Visible installed skill name for prepare; source is resolved by the host"`
	Edits  []harness.SkillFileEdit `json:"edits,omitempty" jsonschema:"description=Existing files only; prepare all related file changes together. Never include credentials. Use json_set to change a non-secret JSON field while preserving tokens."`
	ID     string                  `json:"edit_id,omitempty" jsonschema:"description=Prepared edit ID for apply or status"`
}
type preparedSkillEdit struct {
	skillEditInput
	Review *harness.SkillEditReview `json:"host_review,omitempty"`
}
type installedSkillTool struct {
	info     *schema.ToolInfo
	registry *skills.Registry
	run      harness.RunRequest
	refs     []harness.SkillRef
}

func newInstalledSkillTools(registry *skills.Registry, req harness.RunRequest, refs []harness.SkillRef, editable bool) ([]tool.BaseTool, error) {
	inspect, err := utils.GoStruct2ToolInfo[skillInspectInput]("skill_inspect", "Inspect the authoritative installed skill identity, source directory, immutable pinned version, current head and file manifest. A workspace copy is NOT the installation. Read file bodies using read_skill_file.")
	if err != nil {
		return nil, err
	}
	tools := []tool.BaseTool{&installedSkillTool{inspect, registry, req, refs}}
	if editable {
		info, err := utils.GoStruct2ToolInfo[skillEditInput]("skill_edit", "Edit the authoritative installed directory, never a workspace copy or database-only overlay. First inspect, then prepare a bounded multi-file edit, then apply its edit_id. Apply ALWAYS requires explicit host confirmation of the exact redacted diff and target; general session permission is insufficient. Existing Python/scripts are structurally validated, never executed. Prepared is NOT applied. Only status=applied AND live_matches=true AND source_matches=true confirms success. Each new ordinary turn re-reads the source directory; this execution/resume stays pinned. skill_manage proposes autonomous evolution candidates; a proposal alone does not change files.")
		if err != nil {
			return nil, err
		}
		tools = append(tools, &installedSkillTool{info, registry, req, refs})
	}
	return tools, nil
}

func (t *installedSkillTool) Info(context.Context) (*schema.ToolInfo, error) { return t.info, nil }
func (t *installedSkillTool) ref(name string) (harness.SkillRef, error) {
	for _, ref := range t.refs {
		if ref.Name == name {
			return ref, nil
		}
	}
	return harness.SkillRef{}, harness.ErrPermissionDenied
}
func (t *installedSkillTool) owner(ctx context.Context) (string, error) {
	if _, active, err := einoengine.CurrentRole(ctx); active || err != nil {
		return "", harness.ErrPermissionDenied
	}
	if t.run.RootBudgetID != "" && t.run.RootBudgetID != t.run.RunID {
		return "", harness.ErrPermissionDenied
	}
	scope, ok := durablebudget.ScopeFromContext(ctx)
	if !ok || scope.MemberID != t.run.RunID || scope.SessionID != t.run.Session.ID || scope.AttemptID == "" || compose.GetToolCallID(ctx) == "" {
		return "", harness.ErrPermissionDenied
	}
	return fmt.Sprintf("installed/%x", sha256.Sum256([]byte(t.run.Session.ID))), nil
}
func (t *installedSkillTool) review(ctx context.Context, id, owner string) (harness.SkillEditReview, error) {
	review, err := t.registry.EditReview(ctx, id, owner)
	if err != nil {
		return review, err
	}
	ref, err := t.ref(review.Target.Name)
	if err != nil || ref.SourceID != review.Target.SourceID || ref.SourceIdentity != review.Target.SourceIdentity {
		return harness.SkillEditReview{}, harness.ErrPermissionDenied
	}
	return review, nil
}

func (t *installedSkillTool) PrepareToolArguments(ctx context.Context, args string) (string, error) {
	owner, err := t.owner(ctx)
	if err != nil {
		return "", err
	}
	if len(args) > 256<<10 {
		return "", harness.ErrInvalidInput
	}
	if t.info.Name == "skill_inspect" {
		var in skillInspectInput
		if err := decodeBackgroundArguments(args, &in); err != nil {
			return "", harness.ErrInvalidInput
		}
		if _, err := t.ref(in.Name); err != nil {
			return "", err
		}
		return args, nil
	}
	if t.run.Session.Mode == "plan" || t.run.Session.ApprovalMode == harness.ApprovalReadOnly {
		return "", harness.ErrPermissionDenied
	}
	var in skillEditInput
	if err := decodeBackgroundArguments(args, &in); err != nil {
		return "", harness.ErrInvalidInput
	}
	out := preparedSkillEdit{skillEditInput: in}
	switch in.Action {
	case "prepare":
		if in.ID != "" {
			return "", harness.ErrInvalidInput
		}
		ref, err := t.ref(in.Name)
		if err != nil {
			return "", err
		}
		// Reject secret-bearing or invalid operations before tool events and
		// permission records. This check stages nothing and changes no head.
		if err = t.registry.CheckEdit(ctx, ref, in.Edits); err != nil {
			return "", err
		}
	case "apply", "status":
		if in.Name != "" || len(in.Edits) != 0 || in.ID == "" {
			return "", harness.ErrInvalidInput
		}
		review, err := t.review(ctx, in.ID, owner)
		if err != nil {
			return "", err
		}
		if in.Action == "apply" {
			out.Review = &review
		}
	default:
		return "", harness.ErrInvalidInput
	}
	data, err := json.Marshal(out)
	return string(data), err
}

func (t *installedSkillTool) InvokableRun(ctx context.Context, args string, _ ...tool.Option) (string, error) {
	owner, err := t.owner(ctx)
	if err != nil {
		return "", harness.MarkToolNotExecuted(err)
	}
	var result any
	if t.info.Name == "skill_inspect" {
		var in skillInspectInput
		if err := decodeBackgroundArguments(args, &in); err != nil {
			return "", harness.MarkToolNotExecuted(harness.ErrInvalidInput)
		}
		ref, e := t.ref(in.Name)
		if e != nil {
			return "", harness.MarkToolNotExecuted(e)
		}
		result, err = t.registry.InspectSkill(ctx, ref)
	} else {
		if !einoengine.ConfirmedToolArguments(ctx, "skill_edit", args) {
			return "", harness.MarkToolNotExecuted(harness.ErrPermissionDenied)
		}
		var in preparedSkillEdit
		if err := decodeBackgroundArguments(args, &in); err != nil {
			return "", harness.MarkToolNotExecuted(harness.ErrInvalidInput)
		}
		switch in.Action {
		case "prepare":
			ref, e := t.ref(in.Name)
			if e != nil {
				return "", harness.MarkToolNotExecuted(e)
			}
			key := fmt.Sprintf("%x", sha256.Sum256([]byte(t.run.RunID+"/"+compose.GetToolCallID(ctx))))
			var review harness.SkillEditReview
			review, err = t.registry.PrepareEdit(ctx, ref, owner, key, in.Edits)
			result = map[string]any{"status": "prepared", "live_skill_changed": false, "review": review, "next_action": "Apply edit_id with explicit host confirmation; no installed files have changed."}
		case "apply":
			if in.Review == nil {
				return "", harness.MarkToolNotExecuted(harness.ErrPermissionDenied)
			}
			var state harness.SkillEditResult
			state, err = t.registry.ApplyEdit(ctx, in.ID, owner, *in.Review)
			result = state
			if err == nil && (state.Status != "applied" || !state.LiveMatches || state.Review.Storage == "source" && !state.SourceMatches) {
				body, _ := json.Marshal(state)
				return string(body), harness.MarkToolExecutionFailed(&skillToolRejection{"Installed edit not verified; inspect status and reconcile on the host. Do not claim success.", string(body)})
			}
		case "status":
			if _, e := t.review(ctx, in.ID, owner); e != nil {
				return "", harness.MarkToolNotExecuted(e)
			}
			result, err = t.registry.EditStatus(ctx, in.ID, owner)
		default:
			return "", harness.MarkToolNotExecuted(harness.ErrInvalidInput)
		}
	}
	if err != nil {
		if errors.Is(err, harness.ErrSkillConflict) || errors.Is(err, skills.ErrPublicationConflict) || errors.Is(err, harness.ErrPermissionDenied) || errors.Is(err, harness.ErrInvalidInput) || errors.Is(err, harness.ErrNotFound) || errors.Is(err, harness.ErrReconciliationRequired) {
			return "", harness.MarkToolNoEffect(err)
		}
		return "", err
	}
	data, err := json.Marshal(result)
	return string(data), err
}
