package runtime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestInstalledEditRequiresExplicitApproval(t *testing.T) {
	p := harness.PermissionRequest{ToolName: "skill_edit", Arguments: json.RawMessage(`{"action":"apply","edit_id":"fixed"}`)}
	for _, mode := range []harness.PermissionMode{harness.PermissionModeOff, harness.PermissionModeAll, harness.PermissionModeDangerous} {
		s := &Service{PermissionMode: mode, decisions: map[string]harness.PermissionDecision{}}
		x := harness.Session{ID: "session", ApprovalMode: harness.ApprovalAllowAlways}
		if d, handled := s.configuredPermission(x, p); handled {
			t.Fatalf("auto-approved %s: %s", mode, d)
		}
		calls := 0
		for i := 0; i < 2; i++ {
			_, err := s.executionPermission(context.Background(), "owner", x, p, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
				calls++
				return harness.AllowAlways, nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		if calls != 2 || len(s.decisions) != 0 {
			t.Fatal("cached explicit edit approval")
		}
		x.Mode = "plan"
		if d, handled := s.configuredPermission(x, p); !handled || d != harness.RejectOnce {
			t.Fatal("plan edit allowed")
		}
	}
}
