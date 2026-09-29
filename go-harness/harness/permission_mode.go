package harness

import (
	"fmt"
	"strings"
)

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

// RequiresPermission follows the Python local ACP tool classifier. Session
// approval choices apply only after this deployment-level classification.
func (m PermissionMode) RequiresPermission(name string) bool {
	name = strings.ToLower(name)
	switch m {
	case PermissionModeOff:
		return false
	case PermissionModeDangerous:
		switch name {
		case "ask_clarification", "list_uploaded_files", "memory_search", "task", "tool_search":
			return false
		}
		if name == "ls" || containsToolKind(name, "read", "view", "list_file") {
			return false
		}
		if containsToolKind(name, "write", "edit", "patch", "create_file", "replace", "delete", "remove", "move", "rename", "fetch", "web", "http", "browser") {
			return true
		}
		if name == "glob" || containsToolKind(name, "search", "query", "grep", "find") {
			return false
		}
		if containsToolKind(name, "think", "task", "subagent") {
			return false
		}
		return true
	default:
		return true
	}
}

func containsToolKind(name string, parts ...string) bool {
	name = strings.ToLower(name)
	for _, part := range parts {
		if strings.Contains(name, part) {
			return true
		}
	}
	return false
}
