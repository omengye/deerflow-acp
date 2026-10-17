package harness

import (
	"context"
	"encoding/json"
)

// ToolArgumentPreparer resolves a native tool's arguments before its receipt and
// permission are recorded. Only host implementations may supply this interface.
// The returned arguments are used unchanged for approval and execution.
type ToolArgumentPreparer interface {
	PrepareToolArguments(context.Context, string) (string, error)
}

// ExplicitSkillEditApproval cannot be disabled by broad session/deployment
// grants. Invalid arguments fail closed as requiring explicit confirmation.
func ExplicitSkillEditApproval(p PermissionRequest) bool {
	if p.ToolName != "skill_edit" {
		return false
	}
	var in struct {
		Action string `json:"action"`
	}
	return json.Unmarshal(p.Arguments, &in) != nil || in.Action == "apply"
}

type SkillFileEdit struct {
	Path      string          `json:"path" jsonschema:"required,description=Existing file path relative to the installed skill"`
	Operation string          `json:"operation" jsonschema:"required,enum=replace,enum=write,enum=json_set"`
	Find      *string         `json:"find,omitempty"`
	Replace   *string         `json:"replace,omitempty"`
	Content   *string         `json:"content,omitempty"`
	Pointer   string          `json:"pointer,omitempty" jsonschema:"description=JSON pointer for json_set; credential fields cannot be changed"`
	Value     json.RawMessage `json:"value,omitempty"`
}

type SkillEditChange struct {
	Path         string `json:"path"`
	BeforeSHA256 string `json:"before_sha256"`
	AfterSHA256  string `json:"after_sha256"`
	Preview      string `json:"preview"`
	Script       bool   `json:"script"`
}

// SkillEditReview contains no file bodies or credentials outside the redacted
// previews. It is immutable after preparation and bound to the approval digest.
type SkillEditReview struct {
	ID              string            `json:"edit_id"`
	Target          SkillTarget       `json:"target"`
	Base            SkillHead         `json:"base"`
	Candidate       string            `json:"candidate_sha256"`
	Storage         string            `json:"storage"`
	SourceDirectory string            `json:"source_directory,omitempty"`
	Changes         []SkillEditChange `json:"changes"`
	Behavior        string            `json:"behavior"`
	Effective       string            `json:"effective"`
}

type SkillEditResult struct {
	Review         SkillEditReview `json:"review"`
	Status         string          `json:"status"`
	AppliedHead    SkillHead       `json:"applied_head"`
	CurrentHead    SkillHead       `json:"current_head"`
	LiveMatches    bool            `json:"live_matches"`
	SourceMatches  bool            `json:"source_matches"`
	ErrorCode      string          `json:"error_code,omitempty"`
	BackupRelative string          `json:"backup_relative,omitempty"`
}

type SkillInspection struct {
	Target          SkillTarget     `json:"target"`
	PinnedVersion   string          `json:"pinned_version"`
	Head            SkillHead       `json:"head"`
	Storage         string          `json:"storage"`
	SourceDirectory string          `json:"source_directory,omitempty"`
	Files           []SkillFileInfo `json:"files"`
	Editable        bool            `json:"editable"`
	BlockedReason   string          `json:"blocked_reason,omitempty"`
	NextAction      string          `json:"next_action"`
}
