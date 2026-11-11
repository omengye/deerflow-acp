package deerflow

import (
	"encoding/json"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills/evolution"
)

func TestManagementCandidateLifecycleKeepsHistoryReadOnlyAndDiffInDetail(t *testing.T) {
	c, _ := skillEvolutionClient(t)
	p := submitSkillEdit(t, c, "mobile-history", "Historical candidate instructions")
	check := func(row map[string]any) {
		t.Helper()
		// Check the wire contract, not Go's in-memory numeric representations.
		raw, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		var decoded struct {
			StateVersion int64                             `json:"state_version"`
			Lifecycle    evolution.CandidateProgress       `json:"lifecycle"`
			Disposition  *harness.SkillProposalDisposition `json:"disposition"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.StateVersion != p.StateVersion || decoded.Lifecycle.StateVersion != decoded.StateVersion ||
			decoded.Lifecycle.Status != "historical" || decoded.Lifecycle.CanCancel {
			t.Fatal("legacy storage state became an active candidate", decoded)
		}
		if decoded.Disposition == nil || decoded.Disposition.Reason != "manual_review" || !decoded.Disposition.NeedsAttention {
			t.Fatal("lifecycle projection lost legacy disposition", decoded)
		}
	}
	for _, operation := range []string{"proposal.list", "proposal.history"} {
		page, err := skillManagementCall(t, c, map[string]any{"operation": operation, "limit": 1})
		if err != nil {
			t.Fatal(operation, err)
		}
		result := page.(map[string]any)
		capabilities := result["capabilities"].(map[string]bool)
		if capabilities["review"] || !capabilities["automatic"] || !capabilities["lifecycle"] {
			t.Fatal(operation, "lost automatic lifecycle capabilities", capabilities)
		}
		rows := result["proposals"].([]map[string]any)
		if len(rows) != 1 || rows[0]["diff"] != nil {
			t.Fatal(operation, "list duplicated full candidate diff", rows)
		}
		if rows[0]["review_retry_available"] != false {
			t.Fatal(operation, "historical candidate offered manual recovery")
		}
		check(rows[0])
	}
	detail, err := skillManagementCall(t, c, map[string]any{"operation": "proposal.get", "proposal_id": p.ID})
	if err != nil {
		t.Fatal(err)
	}
	row := detail.(map[string]any)
	check(row)
	if row["diff"] != p.Diff || p.Diff == "" {
		t.Fatal("detail lost its full diff")
	}
	if _, err := skillManagementCall(t, c, map[string]any{"operation": "proposal.approve", "proposal_id": p.ID}); err == nil {
		t.Fatal("read-only projection re-enabled approval")
	}
}
