package agent

import (
	"errors"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
)

func TestTaskTreeStopErrorPreservesReconciliation(t *testing.T) {
	for _, test := range []struct {
		err  error
		kind string
	}{
		{harness.ErrTaskTreeStopped, "execution_stopped"},
		{errors.Join(harness.ErrTaskTreeStopped, harness.ErrReconciliationRequired), "reconciliation_required"},
	} {
		var rpc *protocol.Error
		if !errors.As(mapError(test.err), &rpc) {
			t.Fatalf("unmapped stopped execution: %v", test.err)
		}
		details, ok := rpc.Data.(map[string]any)
		if !ok || details["kind"] != test.kind || details["automaticReplay"] != false {
			t.Fatalf("lost outcome priority: %+v", rpc)
		}
	}
}

func TestSkillNameConflictHasActionableRecovery(t *testing.T) {
	conflict := &harness.SkillNameConflictError{Name: "sample", FirstSource: "custom", SecondSource: "managed"}
	var rpc *protocol.Error
	if !errors.As(mapError(conflict), &rpc) || rpc.Message == "Invalid request parameters" {
		t.Fatal("skill conflict lost recovery guidance", rpc)
	}
	data := rpc.Data.(map[string]any)
	if data["kind"] != "skill_name_conflict" || data["skillName"] != "sample" || data["automaticReplay"] != false {
		t.Fatal(data)
	}
	if !errors.As(mapError(errors.Join(conflict, harness.ErrReconciliationRequired)), &rpc) || rpc.Code != -32010 {
		t.Fatal("skill guidance must not hide uncertain effects", rpc)
	}
}
