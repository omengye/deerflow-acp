package deerflow

import (
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestBackgroundHostPolicyPinsPermissionMode(t *testing.T) {
	a, err := backgroundHostPolicy(Config{PermissionMode: harness.PermissionModeDangerous})
	if err != nil {
		t.Fatal(err)
	}
	b, err := backgroundHostPolicy(Config{PermissionMode: harness.PermissionModeAll})
	if err != nil || a == b {
		t.Fatalf("background host permission policy drift was not pinned: first=%s second=%s err=%v", a, b, err)
	}
}
