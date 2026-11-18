package deerflow

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestMemoryReviewB5GuidanceAndPinnedContracts(t *testing.T) {
	c, session, cfg := memoryStage0ConfiguredClient(t, Config{MemorySingleUser: true}, func(w http.ResponseWriter, r *http.Request) { memoryStage0Completion(t, w, "done", "", nil) })
	cfg.memoryGlobalID = c.service.MemoryGlobalID
	if _, err := c.CreateMemoryFact(t.Context(), session.ID, harness.MemoryGlobal, harness.MemoryCandidate{Content: "用户姓名是林青。", Category: "profile", Confidence: 1}); err != nil {
		t.Fatal(err)
	}
	req := harness.RunRequest{Session: session, RunID: "guide", InputID: "guide", RootBudgetID: "guide"}
	factory := extensionFactory(cfg, c.mcp, c.skills, c.assets, c.service.Memory)
	modern, err := factory(t.Context(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if modern.Cleanup != nil {
		defer modern.Cleanup()
	}
	var state extensionState
	if err = json.Unmarshal(modern.State, &state); err != nil {
		t.Fatal(err)
	}
	if state.MemoryGuidanceVersion != 1 || state.MemoryWriteProtocol != "memory-tools-v5" || !strings.Contains(modern.InstructionAppend, harness.MemoryProvenanceNotice) || strings.Contains(modern.InstructionAppend, "were saved after reading third-party content") {
		t.Fatal("new guidance conflates exposure with missing evidence")
	}
	for _, version := range []string{"memory-tools-v3", "memory-tools-v4"} {
		t.Run(version, func(t *testing.T) {
			pinnedState := state
			pinnedState.MemoryGuidanceVersion = 0
			pinnedState.MemoryWriteProtocol = version
			pinned, _ := json.Marshal(pinnedState)
			resumed, err := factory(t.Context(), req, pinned)
			if err != nil {
				t.Fatal(err)
			}
			if resumed.Cleanup != nil {
				defer resumed.Cleanup()
			}
			if !bytes.Equal(pinned, resumed.State) || strings.Contains(resumed.InstructionAppend, harness.MemoryProvenanceNotice) || !strings.Contains(resumed.InstructionAppend, "were saved after reading third-party content") {
				t.Fatal("pinned contract changed")
			}
			want := personalMemoryInstructionV3
			if version == "memory-tools-v4" {
				want = personalMemoryInstructionV4
			}
			if !strings.Contains(resumed.InstructionAppend, want) {
				t.Fatal("pinned tools instruction changed")
			}
		})
	}
}
