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
	} {
		s := &Service{PermissionMode: tc.mode}
		got, handled := s.configuredPermission(harness.Session{ApprovalMode: tc.approval}, request)
		if got != tc.want || handled != tc.handled {
			t.Errorf("mode=%s approval=%s got=%s handled=%t", tc.mode, tc.approval, got, handled)
		}
	}
	readOnly := &Service{PermissionMode: harness.PermissionModeOff}
	got, handled := readOnly.configuredPermission(harness.Session{ApprovalMode: harness.ApprovalReadOnly}, harness.PermissionRequest{ToolName: "write_file"})
	if !handled || got != harness.RejectOnce {
		t.Fatalf("deployment off bypassed Go read-only mode: %s %t", got, handled)
	}
}
