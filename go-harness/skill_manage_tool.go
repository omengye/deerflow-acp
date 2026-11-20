package deerflow

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
	durablebudget "github.com/omengye/deerflow-acp/go-harness/internal/budget"
	einoengine "github.com/omengye/deerflow-acp/go-harness/internal/engine/eino"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills/evolution"
	"github.com/omengye/deerflow-acp/go-harness/internal/toolidentity"
)

func validateSkillProposalSources(cfg Config, registry *skills.Registry) error {
	if len(cfg.SkillProposalSources) == 0 {
		return nil
	}
	if cfg.SkillValidator == nil {
		return fmt.Errorf("%w: skill proposal sources require a configured validator", harness.ErrInvalidInput)
	}
	sources, err := registry.PublicationSources()
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, id := range cfg.SkillProposalSources {
		found := false
		for _, source := range sources {
			if source.SourceID == id {
				found = true
				break
			}
		}
		if !found || seen[id] {
			return fmt.Errorf("%w: skill proposal sources must be unique configured source IDs", harness.ErrInvalidInput)
		}
		seen[id] = true
	}
	slices.Sort(cfg.SkillProposalSources)
	return nil
}

type skillManageInput struct {
	Action        string            `json:"action" jsonschema:"required,enum=create,enum=edit,enum=patch,enum=write_file,enum=remove_file,enum=get,enum=publish,description=Submit or inspect your own candidate. The host publishes after safety review. publish checks or resumes an existing file installation."`
	Source        string            `json:"source,omitempty" jsonschema:"description=For an existing skill, omit to use its pinned source. An explicit source must match that skill. For create, choose an authorized destination when several are available."`
	Name          string            `json:"name,omitempty" jsonschema:"description=Skill name for a candidate mutation"`
	ProposalID    string            `json:"proposal_id,omitempty" jsonschema:"description=Your own candidate ID for get or publish"`
	CandidateSHA  string            `json:"candidate_sha256,omitempty" jsonschema:"description=Required for publish: exact candidate_sha256 returned by get; not accepted for other actions"`
	StateVersion  int64             `json:"state_version,omitempty" jsonschema:"description=Required for publish: exact state_version returned by get; refresh get on conflict"`
	Reason        string            `json:"reason,omitempty" jsonschema:"description=The reusable problem and requirements this change must solve. Review models evaluate this together with the candidate; preserve unmet requirements when revising."`
	Content       *string           `json:"content,omitempty" jsonschema:"description=Complete SKILL.md content for create/edit or support file text for write_file"`
	Files         map[string]string `json:"files,omitempty" jsonschema:"description=Complete UTF-8 file package for create/edit, including SKILL.md and all support files. Cannot be mixed with content/path/patch fields. Never write a draft into the final installation directory."`
	Path          string            `json:"path,omitempty" jsonschema:"description=Canonical relative support-file path for write_file/remove_file; never SKILL.md"`
	Find          *string           `json:"find,omitempty" jsonschema:"description=Exact nonempty SKILL.md text for patch"`
	Replace       *string           `json:"replace,omitempty" jsonschema:"description=Replacement text for patch; can be empty"`
	ExpectedCount *int              `json:"expected_count,omitempty" jsonschema:"description=Optional exact total occurrence count for patch; without it replace the first match"`
}

type skillManageTool struct {
	toolidentity.Native
	info        *schema.ToolInfo
	service     *evolution.Service
	run         harness.RunRequest
	targets     []harness.SkillTarget
	refs        []harness.SkillRef
	builtin     []string
	registry    *skills.Registry
	selection   harness.SkillSelection
	dataDir     string
	readAllowed bool
}

func newSkillManageTool(ctx context.Context, cfg Config, registry *skills.Registry, req harness.RunRequest, refs []harness.SkillRef) (tool.BaseTool, error) {
	if cfg.skillEvolution == nil {
		return nil, fmt.Errorf("%w: skill proposal service is missing", harness.ErrInvalidInput)
	}
	sources, err := registry.PublicationSources()
	if err != nil {
		return nil, err
	}
	builtin, err := registry.BuiltinSources()
	if err != nil {
		return nil, err
	}
	managed, err := registry.ManagedSources()
	if err != nil {
		return nil, err
	}
	var targets []harness.SkillTarget
	var descriptions []string
	for _, source := range sources {
		if !slices.Contains(cfg.SkillProposalSources, source.SourceID) {
			continue
		}
		if source.Scope == harness.SkillScopeWorkspace && !session.SameWorkspace(source.Workspace, req.Session.CWD) || source.Scope == harness.SkillScopeGlobal && !cfg.SkillSelection.IncludeGlobal {
			continue
		}
		targets = append(targets, source)
		description := source.SourceID + " (" + string(source.Scope) + ")"
		if slices.Contains(builtin, source.SourceID) {
			description += " [builtin: in-place automatic changes unavailable]"
		}
		if slices.Contains(managed, source.SourceID) {
			description += " [managed source: preferred for new skills]"
		}
		descriptions = append(descriptions, description)
	}
	if len(targets) == 0 {
		return nil, nil
	}
	info, err := utils.GoStruct2ToolInfo[skillManageInput]("skill_manage", "Submit complete skill packages for host safety review. The host automatically installs authorized creations and updates after safety passes. No usage evidence, behavioral tests, observation period or extra user confirmation is required. Use get with the returned proposal_id to check progress; publish only resumes a previously admitted file installation. Report publication and next-turn availability from host receipts. Scripts, credentials and external effects require their normal runtime permissions. Read skill-creator before authoring and keep drafts outside installed roots. Existing skills must be pinned and visible. Authorized candidate sources: "+strings.Join(descriptions, ", "))
	if err != nil {
		return nil, err
	}
	return &skillManageTool{Native: toolidentity.Builtin("skill_manage"), info: info, service: cfg.skillEvolution, run: req, targets: targets, refs: slices.Clone(refs), builtin: builtin, registry: registry, selection: cfg.SkillSelection, dataDir: cfg.DataDir, readAllowed: cfg.ToolPolicy.Allows("skill") || cfg.ToolPolicy.Allows("read_skill_file")}, nil
}

func (t *skillManageTool) Info(context.Context) (*schema.ToolInfo, error) { return t.info, nil }

func (t *skillManageTool) actor(ctx context.Context) (string, *agents.Invocation, error) {
	if t.run.Session.Mode == "plan" || t.run.Session.ApprovalMode == harness.ApprovalReadOnly || t.run.Session.ApprovalMode == harness.ApprovalRejectAlways || compose.GetToolCallID(ctx) == "" {
		return "", nil, harness.ErrPermissionDenied
	}
	scope, ok := durablebudget.ScopeFromContext(ctx)
	root := t.run.RootBudgetID
	if root == "" {
		root = t.run.RunID
	}
	if !ok || scope.RootBudgetID != root || scope.MemberID != t.run.RunID || scope.SessionID != t.run.Session.ID || scope.AttemptID == "" {
		return "", nil, harness.ErrPermissionDenied
	}
	role, active, err := einoengine.CurrentRole(ctx)
	if err != nil {
		return "", nil, err
	}
	identity := []string{"session", t.run.Session.ID}
	var invocation *agents.Invocation
	if active {
		if role.RootID != root || !role.Spec.Tools.Allows("skill_manage") {
			return "", nil, harness.ErrPermissionDenied
		}
		identity = []string{"role", root, role.ID}
		invocation = &role
	}
	data, _ := json.Marshal(identity)
	return fmt.Sprintf("model/%x", sha256.Sum256(data)), invocation, nil
}

func (t *skillManageTool) target(id string) (harness.SkillTarget, error) {
	if id == "" && len(t.targets) == 1 {
		return t.targets[0], nil
	}
	for _, target := range t.targets {
		if target.SourceID == id {
			return target, nil
		}
	}
	return harness.SkillTarget{}, harness.ErrPermissionDenied
}

func (t *skillManageTool) visible(role *agents.Invocation, target harness.SkillTarget, name string) (harness.SkillRef, bool) {
	if role != nil && !slices.Contains(role.Spec.Skills, name) {
		return harness.SkillRef{}, false
	}
	for _, ref := range t.refs {
		if ref.SourceID == target.SourceID && ref.SourceIdentity == target.SourceIdentity && ref.Name == name {
			return ref, true
		}
	}
	return harness.SkillRef{}, false
}

func skillManageResult(p harness.SkillProposal) string {
	feedback := evolution.ProposalFeedback(p)
	message, code, stage, retryable := p.Error, "", "", false
	if feedback.Failure != nil {
		message, code, stage, retryable = feedback.Message, feedback.Failure.Code, feedback.Failure.Stage, feedback.Failure.Retryable
	}
	feedback.NextAction = "Inspect this candidate with get for its current review and publication status."
	if p.PublicationError != "" {
		code, stage, retryable = p.PublicationError, "publication", false
		feedback.NextAction = "The target changed or publication could not complete. Preserve its files and inspect the current candidate state; do not overwrite a newer version."
	}
	if p.Origin != "automatic" {
		feedback.NextAction = "This is a historical candidate. It cannot be manually approved or resumed through skill_manage; new task evidence uses the automatic pipeline."
	}
	mode := "automatic"
	if p.PublicationPolicy == harness.SkillPublicationSecurityOnly {
		mode = p.PublicationPolicy
		switch p.Status {
		case "validating", "pending_review", "publishing":
			feedback.NextAction = "The host reviews safety and installs this exact package automatically. Use get to check progress."
		case "published":
			feedback.NextAction = "Report the publication receipt and the verified availability for the next turn."
		case "failed":
			feedback.NextAction = "Inspect the safety review result. Address a rejected finding before submitting a revised package."
		case "rejected":
			feedback.NextAction = "This candidate was cancelled."
		case "stale":
			feedback.NextAction = "Read the current target and source before preparing another update."
		}
	}
	data, _ := json.Marshal(map[string]any{
		"ok": true, "proposal_id": p.ID, "status": evolution.CandidateStatus(p), "action": p.Action, "mode": mode,
		"name": p.SkillName, "source": p.Target.SourceID, "state_version": p.StateVersion,
		"base_sha256": p.Base.Version, "candidate_sha256": p.CandidateVersion,
		"security": p.Evaluation.Security, "quality": feedback.Quality, "behavior": p.Evaluation.Behavior,
		"model_checks": feedback.Checks, "error": message, "error_code": code, "failure_stage": stage,
		"validation_failure": feedback.Failure,
		"retryable":          retryable, "next_action": feedback.NextAction, "evaluation_note": p.ReviewNote,
		"live_skill_changed":         false,
		"scripts_execution_verified": p.Evaluation.IsolatedScripts,
		"published":                  p.Status == "published",
		"publication":                map[string]any{"state": evolution.CandidateStatus(p), "error_code": p.PublicationError},
		"file_publication_pending":   p.FilePublicationPending,
		"source_identity":            p.Target.SourceIdentity, "availability_verified": false,
	})
	if p.Approval != nil && p.Approval.UserConfirmation != nil {
		var result map[string]any
		_ = json.Unmarshal(data, &result)
		result["mode"], result["evidence_gate_bypassed"] = "user_confirmed", true
		result["confirmation"] = p.Approval.UserConfirmation
		result["validation_error"], result["validation_error_code"] = message, code
		result["error"], result["error_code"], result["failure_stage"], result["retryable"] = "", "", "", false
		result["next_action"] = "Publication follows the user's explicit confirmation. Report the host publication and availability receipts; unrun or failed checks remain unchanged and execution permissions are separate."
		data, _ = json.Marshal(result)
	}
	return string(data)
}

func (t *skillManageTool) result(ctx context.Context, p harness.SkillProposal, role *agents.Invocation) string {
	var progress evolution.CandidateProgress
	var snapshotErr error
	if t.service != nil {
		var fresh harness.SkillProposal
		fresh, progress, snapshotErr = t.service.CandidateSnapshot(ctx, p.ID)
		if snapshotErr == nil {
			p = fresh
		}
	}
	var result map[string]any
	_ = json.Unmarshal([]byte(skillManageResult(p)), &result)
	if t.service != nil {
		if snapshotErr == nil {
			result["status"], result["reason"] = progress.Status, progress.Reason
			result["job_id"], result["job_state"] = progress.JobID, progress.JobState
			result["automatic_retry"] = progress.AutomaticRetry
			if progress.Evidence != nil {
				result["evidence"] = progress.Evidence
				if !progress.Evidence.SuiteBound {
					result["next_action"] = progress.Evidence.NextAction
				}
			}
			result["publication"] = map[string]any{"state": progress.Status, "error_code": p.PublicationError}
		} else {
			result["status"], result["error_code"] = "unavailable", "progress_unavailable"
		}
	}
	result["data_dir"], result["session_id"] = t.dataDir, t.run.Session.ID
	result["availability_scope"], result["execution_checked"] = "next_turn", false
	roleCanRead := role == nil || slices.Contains(role.Spec.Skills, p.SkillName) && (role.Spec.Tools.Allows("skill") || role.Spec.Tools.Allows("read_skill_file"))
	if t.readAllowed && roleCanRead && p.Status == "published" && !p.FilePublicationPending && p.Action != "delete" {
		selection := t.selection
		selection.Workspace = t.run.Session.CWD
		if snapshot, err := t.registry.Snapshot(ctx, selection); err == nil {
			for _, ref := range snapshot.Refs() {
				if ref.Name == p.SkillName && ref.SourceIdentity == p.Target.SourceIdentity && ref.Version == p.CandidateVersion {
					if _, err := snapshot.Get(ctx, ref.Name); err == nil {
						result["availability_verified"] = true
					}
				}
			}
		}
	}
	data, _ := json.Marshal(result)
	return string(data)
}

func skillManageFailure(code, message string) (string, error) {
	data, _ := json.Marshal(map[string]any{"ok": false, "code": code, "error": message, "live_skill_changed": false})
	return string(data), harness.MarkToolNotExecuted(&skillToolRejection{message: message, result: string(data)})
}

// Eino may discard the value accompanying an error. Keep the bounded rejection
// available to the middleware without turning a rejected mutation into success.
type skillToolRejection struct{ message, result string }

func (e *skillToolRejection) Error() string      { return e.message }
func (e *skillToolRejection) ToolResult() string { return e.result }

func (t *skillManageTool) mutationTarget(ctx context.Context, input skillManageInput, role *agents.Invocation) (harness.SkillTarget, string) {
	if input.Action == "create" {
		target, err := t.target(input.Source)
		if err != nil {
			return harness.SkillTarget{}, "Choose an explicitly authorized candidate source for the new skill."
		}
		return target, ""
	}
	for _, ref := range t.refs {
		if ref.Name != input.Name || role != nil && !slices.Contains(role.Spec.Skills, ref.Name) {
			continue
		}
		if input.Source != "" && input.Source != ref.SourceID {
			return harness.SkillTarget{}, "Source mismatch: this visible skill belongs to " + ref.SourceID + "; do not switch to a new-skill destination."
		}
		target, err := t.target(ref.SourceID)
		if err == nil && target.SourceIdentity == ref.SourceIdentity {
			return target, ""
		}
		return harness.SkillTarget{}, "The visible skill's source does not authorize candidate changes."
	}
	if t.registry != nil && (role == nil || slices.Contains(role.Spec.Skills, input.Name)) {
		selection := t.selection
		selection.Workspace = t.run.Session.CWD
		var unavailable *harness.SkillAvailabilityError
		if err := t.registry.VisibilityError(ctx, selection, input.Name); errors.As(err, &unavailable) {
			return harness.SkillTarget{}, unavailable.Error()
		}
	}
	return harness.SkillTarget{}, "Existing skill is outside this agent's pinned visible skill set."
}

func (t *skillManageTool) owns(p harness.SkillProposal, author string, role *agents.Invocation) bool {
	target, err := t.target(p.Target.SourceID)
	target.Name = p.SkillName
	if err != nil || target != p.Target || p.Author != author {
		return false
	}
	if p.Action == "create" {
		return true
	}
	_, visible := t.visible(role, p.Target, p.SkillName)
	return visible
}

func (t *skillManageTool) InvokableRun(ctx context.Context, args string, _ ...tool.Option) (string, error) {
	if len(args) > 512<<10 {
		return skillManageFailure("invalid_request", "Candidate request exceeds bounds.")
	}
	var input skillManageInput
	if err := decodeBackgroundArguments(args, &input); err != nil {
		return skillManageFailure("invalid_request", "Invalid candidate arguments.")
	}
	author, role, err := t.actor(ctx)
	if err != nil {
		return "", harness.MarkToolNotExecuted(err)
	}
	if input.Action == "get" {
		if strings.TrimSpace(input.ProposalID) == "" {
			return skillManageFailure("invalid_request", input.Action+" requires the proposal_id returned by create/edit. It cannot discover or publish a directory. Submit a complete candidate to automatic validation; never write registry tables.")
		}
		if input.Name != "" || input.Source != "" || input.Content != nil || input.Files != nil || input.Path != "" || input.Find != nil || input.Replace != nil || input.ExpectedCount != nil || input.Reason != "" || input.CandidateSHA != "" || input.StateVersion != 0 {
			return skillManageFailure("invalid_request", "get accepts only proposal_id. Candidate content cannot change during inspection.")
		}
		p, err := t.service.Get(ctx, input.ProposalID)
		if err != nil || !t.owns(p, author, role) {
			if err != nil && !errors.Is(err, harness.ErrNotFound) && !errors.Is(err, harness.ErrInvalidInput) && !errors.Is(err, harness.ErrPermissionDenied) {
				return "", harness.MarkToolNoEffect(err)
			}
			return skillManageFailure("not_found", "Proposal is not available to this agent.")
		}
		return t.result(ctx, p, role), nil
	}
	if input.Action == "publish" {
		return t.publicationStatus(ctx, input, author, role)
	}
	switch input.Action {
	case "create", "edit", "patch", "write_file", "remove_file":
	default:
		return skillManageFailure("invalid_request", "Use candidate creation, editing, get or publish. The host publishes after safety review. Deletion belongs to explicit skill administration.")
	}
	if input.ProposalID != "" || strings.TrimSpace(input.Reason) == "" || input.CandidateSHA != "" || input.StateVersion != 0 {
		return skillManageFailure("invalid_request", "A mutation requires a reason and cannot supply proposal_id.")
	}
	target, targetError := t.mutationTarget(ctx, input, role)
	if targetError != "" {
		code := "permission_denied"
		for _, reason := range []string{"installation_missing", "disabled", "source_unavailable", "not_in_snapshot"} {
			if strings.HasPrefix(targetError, reason+":") {
				code = reason
				break
			}
		}
		return skillManageFailure(code, targetError)
	}
	if slices.Contains(t.builtin, target.SourceID) {
		return skillManageFailure("permission_denied", "Builtin sources do not accept in-place automatic candidates. New reusable skills require an authorized managed source.")
	}
	canonical, _ := json.Marshal(input)
	identity, _ := json.Marshal([]any{t.run.RunID, compose.GetToolCallID(ctx), author, json.RawMessage(canonical)})
	key := fmt.Sprintf("model/%x", sha256.Sum256(identity))
	prior, err := t.service.LookupRequest(ctx, target.SourceID, input.Name, key)
	if err == nil {
		if !t.owns(prior, author, role) {
			return skillManageFailure("permission_denied", "Proposal is outside this agent's current scope.")
		}
		return t.result(ctx, prior, role), nil
	}
	if !errors.Is(err, harness.ErrNotFound) {
		if errors.Is(err, harness.ErrInvalidInput) {
			return skillManageFailure("invalid_request", "Invalid skill name.")
		}
		return "", harness.MarkToolNoEffect(err)
	}
	current, base, _, err := t.service.Head(ctx, target.SourceID, input.Name)
	if err != nil {
		return "", harness.MarkToolNoEffect(err)
	}
	target.Name = input.Name
	if current != target {
		return skillManageFailure("permission_denied", "Candidate source identity changed.")
	}
	if input.Action == "create" {
		if base.Exists {
			return skillManageFailure("conflict", "A skill already exists at that target.")
		}
	} else {
		ref, visible := t.visible(role, target, input.Name)
		if !visible {
			return skillManageFailure("permission_denied", "Existing skill is outside this agent's pinned visible skill set.")
		}
		if !base.Exists || base.Version != ref.Version {
			return skillManageFailure("conflict", "The live skill changed after this run began. Start a new run to review the current version.")
		}
	}
	var files map[string][]byte
	if input.Files != nil {
		files = make(map[string][]byte, len(input.Files))
		for path, content := range input.Files {
			files[path] = []byte(content)
		}
	}
	p, submitErr := t.service.SubmitAutomatic(ctx, harness.SkillProposalRequest{SourceID: target.SourceID, Name: input.Name, RequestKey: key, Action: input.Action, Base: base, Files: files, Content: input.Content, Path: input.Path, Find: input.Find, Replace: input.Replace, ExpectedCount: input.ExpectedCount, Reason: input.Reason, Author: author, Origin: "automatic", Trigger: harness.SkillTrigger{Type: "explicit_agent_proposal", ThreadID: t.run.Session.ID, RunID: t.run.RunID, Summary: input.Reason}}, t.run.Session.CWD)
	if submitErr == nil {
		return t.result(ctx, p, role), nil
	}
	if p.ID != "" {
		// A cancelled/competing validation may leave a known committed candidate.
		// Read its durable outcome instead of reporting an uncertain mutation.
		check, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		saved, err := t.service.LookupRequest(check, target.SourceID, input.Name, key)
		if err == nil && t.owns(saved, author, role) {
			return t.result(ctx, saved, role), nil
		}
		if err != nil && !errors.Is(err, harness.ErrNotFound) {
			return "", err
		}
	}
	var conflict *skills.PublicationConflict
	if errors.As(submitErr, &conflict) {
		return skillManageFailure(conflict.Code, "The destination cannot accept this candidate. Preserve its files; inspect and submit the complete current package for review, or use a separate draft directory.")
	}
	if errors.Is(submitErr, harness.ErrSkillConflict) || errors.Is(submitErr, skills.ErrPublicationConflict) {
		return skillManageFailure("conflict", "Candidate base changed; refresh before proposing another edit.")
	}
	if errors.Is(submitErr, harness.ErrInvalidInput) {
		return skillManageFailure("invalid_request", submitErr.Error())
	}
	if errors.Is(submitErr, harness.ErrPermissionDenied) {
		return skillManageFailure("permission_denied", "This source does not authorize the requested candidate mutation.")
	}
	// Storage failures are not converted into successful or no-effect receipts.
	return "", submitErr
}
