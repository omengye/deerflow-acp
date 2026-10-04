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
