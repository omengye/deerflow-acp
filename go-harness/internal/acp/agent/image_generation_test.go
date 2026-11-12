package agent

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
)

func TestImageGenerationFailureHasSafeGuidanceWithoutReplay(t *testing.T) {
	for _, test := range []struct{ kind, text string }{
		{"unsupported", "端点或模型"}, {"unsupported_complexity", "复杂度"}, {"access_denied", "权限"}, {"quota_exceeded", "额度"}, {"failed", "未完成"}, {"private-kind", "未完成"},
	} {
		cause := &harness.ImageGenerationError{Kind: test.kind, Cause: errors.New("private-provider-credential")}
		for _, public := range []error{mapError(cause), executionUpdateError(cause)} {
			var rpc *protocol.Error
			if !errors.As(public, &rpc) || rpc.Code != -32032 || !strings.Contains(rpc.Message, test.text) {
				t.Fatalf("missing image failure guidance: %v", public)
			}
			data := rpc.Data.(map[string]any)
			encoded, _ := json.Marshal(rpc)
			if data["kind"] != "image_generation_failed" || data["automaticReplay"] != false || strings.Contains(string(encoded), "private-") {
				t.Fatalf("unsafe image failure: %s", encoded)
			}
		}
		for _, recovery := range []error{harness.ErrReconciliationRequired, harness.ErrReceiptConflict, harness.ErrPermissionDenied, harness.ErrCheckpointCommit} {
			want, got := executionUpdateError(recovery), executionUpdateError(errors.Join(cause, recovery))
			if got.Code != want.Code || got.Message != want.Message {
				t.Fatal("image failure hid a recovery error")
			}
		}
	}
}
