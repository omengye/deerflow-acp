package harness

import "fmt"

// PermissionMode is the deployment's default ACP approval policy. The empty
// value preserves the Go SDK's original behavior (approve configured tools).
type PermissionMode string

const (
	PermissionModeOff       PermissionMode = "off"
	PermissionModeDangerous PermissionMode = "dangerous"
	PermissionModeAll       PermissionMode = "all"
)

func (m PermissionMode) Validate() error {
	switch m {
	case "", PermissionModeOff, PermissionModeDangerous, PermissionModeAll:
		return nil
	default:
		return fmt.Errorf("invalid permission mode %q", m)
	}
}

// PermissionPolicyVersion identifies the latest built-in approval rules.
// Execution contracts bind the revision applicable to their tools and mode.
const PermissionPolicyVersion = "trusted-builtins-v4-direct-web-search"

// RequiresPermission classifies an exact host-owned built-in identity. Engine
// callers must use RequiresToolPermission for tools from external sources.
func (m PermissionMode) RequiresPermission(name string) bool {
	// Candidate submission only queues independent host validation. External
	// tools cannot borrow this exemption, and command permissions are unchanged.
	if name == "skill_manage" {
		return false
	}
	switch m {
	case PermissionModeOff:
		return false
	case PermissionModeDangerous:
		switch name {
		case "skill", "skill_inspect", "read_skill_file", "schedule_read",
			"ls", "list_directory", "read_file", "read_tool_output", "view_image",
			"search_files", "glob", "grep", "list_attachments", "read_attachment", "analyze_attachment",
			"search_memory", "get_memory_state", "history_search", "history_read", "task_note", "task", "task_status", "task_wait", "web_search", "set_search_provider",
			"secret_read", "tool_search":
			return false
		}
		return true
	default:
		return true
	}
}

func (m PermissionMode) RequiresToolPermission(name string, trustedBuiltin bool) bool {
	if m == PermissionModeOff {
		return false
	}
	return !trustedBuiltin || m.RequiresPermission(name)
}
