package deerflow

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSkillVerificationUsesRunningStoreScopeAndExactVersion(t *testing.T) {
	workspace, cfg := skillFixture(t)
	cfg.Engine = engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		return harness.RunResult{}, nil
	})
	cfg.Skills.Install = []harness.SkillInstall{{SourceID: "project", Directory: "research", Enabled: true}}
	c, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	x, err := c.NewSession(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	page, err := c.skills.QueryCatalog(context.Background(), harness.SkillQuery{Limit: 10})
	if err != nil || len(page.Skills) != 1 {
		t.Fatal(page, err)
	}
	entry := page.Skills[0]
	modelTool := &skillManageTool{registry: c.skills, selection: cfg.SkillSelection, dataDir: c.DataDirectory(), run: harness.RunRequest{Session: x}, readAllowed: true}
	proposal := harness.SkillProposal{Status: "published", Action: "create", SkillName: "research", Target: entry.Target, CandidateVersion: entry.Head.Version}
	var modelResult map[string]any
	if err = json.Unmarshal([]byte(modelTool.result(context.Background(), proposal, nil)), &modelResult); err != nil || modelResult["availability_verified"] != true {
		t.Fatal(modelResult, err)
	}
	proposal.Status = "pending_review"
	_ = json.Unmarshal([]byte(modelTool.result(context.Background(), proposal, nil)), &modelResult)
	if modelResult["availability_verified"] != false || modelResult["published"] != false {
		t.Fatal("candidate reported as available", modelResult)
	}
	request := map[string]any{"operation": "skill.verify", "target": entry.Target, "session_id": x.ID, "expected_version": entry.Head.Version}
	check := func(want bool, reason string) {
		t.Helper()
		value, err := skillManagementCall(t, c, request)
		if err != nil {
			t.Fatal(err)
		}
		got := value.(map[string]any)
		if got["available"] != want || got["body_readable"] != want || got["execution_checked"] != false || got["reason"] != reason || got["data_dir"] != c.DataDirectory() {
			t.Fatal(got)
		}
	}
	check(true, "verified")
	request["expected_version"] = strings.Repeat("0", 64)
	check(false, "version_changed")
	request["expected_version"] = entry.Head.Version
	other, err := c.NewSession(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	request["session_id"] = other.ID
	check(false, "not_enabled_or_not_visible")
	request["session_id"] = x.ID
	if err = os.WriteFile(filepath.Join(cfg.Skills.Sources[0].Root, "research", "SKILL.md"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	check(false, "not_enabled_or_not_visible")
}
