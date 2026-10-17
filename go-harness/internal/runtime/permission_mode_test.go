package runtime

import (
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestDeploymentPermissionPrecedesSessionApprovalChoice(t *testing.T) {
	request := harness.PermissionRequest{ToolName: "read_file"}
	for _, tc := range []struct {
		mode     harness.PermissionMode
		approval string
		want     harness.PermissionDecision
		handled  bool
	}{
		{harness.PermissionModeOff, harness.ApprovalRejectAlways, harness.AllowOnce, true},
		{harness.PermissionModeDangerous, harness.ApprovalRejectAlways, harness.AllowOnce, true},
		{harness.PermissionModeAll, harness.ApprovalRejectAlways, harness.RejectOnce, true},
		{harness.PermissionModeAll, harness.ApprovalAllowAlways, harness.AllowOnce, true},
		{harness.PermissionModeAll, harness.ApprovalAsk, "", false},
		{harness.PermissionModeOff, harness.ApprovalGlobal, harness.AllowOnce, true},
		{harness.PermissionModeDangerous, harness.ApprovalGlobal, harness.AllowOnce, true},
		{harness.PermissionModeAll, harness.ApprovalGlobal, "", false},
	} {
		s := &Service{PermissionMode: tc.mode}
		got, handled := s.configuredPermission(harness.Session{ApprovalMode: tc.approval}, request)
		if got != tc.want || handled != tc.handled {
			t.Errorf("mode=%s approval=%s got=%s handled=%t", tc.mode, tc.approval, got, handled)
		}
	}
	readOnly := &Service{PermissionMode: harness.PermissionModeOff}
	for _, session := range []harness.Session{{Mode: "plan"}, {ApprovalMode: harness.ApprovalReadOnly}} {
		got, handled := readOnly.configuredPermission(session, harness.PermissionRequest{ToolName: "read_tool_output"})
		if !handled || got != harness.AllowOnce {
			t.Fatalf("read_tool_output was unavailable in %q/%q: %s %t", session.Mode, session.ApprovalMode, got, handled)
		}
	}
	got, handled := readOnly.configuredPermission(harness.Session{ApprovalMode: harness.ApprovalReadOnly}, harness.PermissionRequest{ToolName: "write_file"})
	if !handled || got != harness.RejectOnce {
		t.Fatalf("deployment off bypassed Go read-only mode: %s %t", got, handled)
	}
}

func TestGlobalApprovalPreservesRestrictedToolsAndExplicitEdits(t *testing.T) {
	for _, mode := range []harness.PermissionMode{harness.PermissionModeOff, harness.PermissionModeDangerous, harness.PermissionModeAll} {
		s := &Service{PermissionMode: mode}
		x := harness.Session{ApprovalMode: harness.ApprovalGlobal}
		for _, name := range []string{"skill", "skill_inspect", "list_directory", "bash", "web_fetch", "write_file", "skill_edit"} {
			got, handled := s.configuredPermission(x, harness.PermissionRequest{ToolName: name})
			wantAllowed := name != "skill_edit" && !mode.RequiresPermission(name)
			if handled != wantAllowed || (handled && got != harness.AllowOnce) {
				t.Errorf("mode=%s tool=%s decision=%s handled=%t", mode, name, got, handled)
			}
		}
		x.Mode = "plan"
		if got, handled := s.configuredPermission(x, harness.PermissionRequest{ToolName: "bash"}); !handled || got != harness.RejectOnce {
			t.Errorf("global policy bypassed plan mode: %s %s %t", mode, got, handled)
		}
	}
}
