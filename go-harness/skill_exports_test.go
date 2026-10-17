package deerflow

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSkillManagedSourceSDKReviewExportAndRecovery(t *testing.T) {
	workspace, cfg := skillFixture(t)
	// A create publication requires an absent authoritative target, rather
	// than hiding a pre-existing directory behind a database-only install.
	if err := os.Rename(filepath.Join(cfg.Skills.Sources[0].Root, "research"), filepath.Join(workspace, "unbound-source")); err != nil {
		t.Fatal(err)
	}
	cfg.Engine = engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		return harness.RunResult{}, nil
	})
	cfg.Skills.ManagedSources = []string{"project"}
	cfg.Skills.ExportRoot = cfg.Skills.Sources[0].Root
	cfg.SkillProposalSources = []string{"project"}
	cfg.SkillValidator = skillValidatorFunc(func(_ context.Context, input harness.SkillValidationInput) (harness.SkillValidationReport, error) {
		return harness.SkillValidationReport{ValidatorVersion: "fixture", BaseVersion: input.Proposal.Base.Version, CandidateVersion: input.Proposal.CandidateVersion, Security: "passed", Behavior: "not_run"}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	c, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if c != nil {
			_ = c.Close()
		}
	}()
	selection := harness.SkillSelection{Workspace: workspace}
	if records, err := c.ListSkills(ctx, selection); err != nil || len(records) != 0 {
		t.Fatalf("managed root was scanned: %+v %v", records, err)
	}
	content := "---\nname: research\ndescription: Generated research\n---\nFollow approved instructions.\n"
	p, err := c.SubmitSkillProposal(ctx, harness.SkillProposalRequest{SourceID: "project", Name: "research", RequestKey: "generate", Action: "create", Content: &content, Reason: "Explicit user request"})
	if err != nil || p.Status != "pending_review" {
		t.Fatalf("candidate: %+v %v", p, err)
	}
	if exports, err := c.ListSkillExports(ctx, selection); err != nil || len(exports) != 0 {
		t.Fatalf("candidate was exported before approval: %+v %v", exports, err)
	}
	// Publishing succeeds while the filesystem is unable to accept a view.
	blocker := filepath.Join(cfg.Skills.ExportRoot, ".deerflow-exports")
	if err = os.WriteFile(blocker, []byte("fixture blocker"), 0600); err != nil {
		t.Fatal(err)
	}
	p, err = c.ApproveSkillProposal(ctx, p.ID, harness.SkillReview{ExpectedStateVersion: p.StateVersion, Actor: "operator"})
	if err != nil || p.Status != "published" {
		t.Fatalf("export failure rejected review: %+v %v", p, err)
	}
	waitExport := func(status string, stateVersion int64) harness.SkillExportState {
		t.Helper()
		for {
			exports, err := c.ListSkillExports(ctx, selection)
			if err != nil {
				t.Fatal(err)
			}
			if len(exports) == 1 && exports[0].Status == status && exports[0].Head.StateVersion == stateVersion {
				return exports[0]
			}
			select {
			case <-ctx.Done():
				t.Fatalf("export never became %s: %+v", status, exports)
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	head, err := c.GetSkillHead(ctx, "project", "research")
	if err != nil || head.Version != p.CandidateVersion {
		t.Fatalf("publication: %+v %v", head, err)
	}
	failed := waitExport("failed", head.StateVersion)
	if failed.Attempts != 3 || failed.ErrorCode != "filesystem_unavailable" {
		t.Fatalf("failure diagnostics: %+v", failed)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c = nil
	if err = os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	c, err = Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	waitExport("failed", head.StateVersion) // Restart does not reset retry limits.
	if err = c.RefreshSkillExports(ctx); err != nil {
		t.Fatal(err)
	}
	synced := waitExport("synced", head.StateVersion)
	viewFile := filepath.Join(cfg.Skills.ExportRoot, filepath.FromSlash(synced.Directory), "files", "SKILL.md")
	if data, err := os.ReadFile(viewFile); err != nil || string(data) != content {
		t.Fatalf("export content: %q %v", data, err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c = nil
	if err = os.Remove(viewFile); err != nil {
		t.Fatal(err)
	}
	c, err = Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt := waitExport("synced", head.StateVersion)
	if rebuilt.Directory == synced.Directory {
		t.Fatal("missing view was not rebuilt")
	}
	if got, err := c.GetSkillHead(ctx, "project", "research"); err != nil || got != head {
		t.Fatalf("recovery moved live publication: %+v %v", got, err)
	}
	if err = c.DeleteSkill(ctx, "project", "research"); err != nil {
		t.Fatal(err)
	}
	deleted := waitExport("synced", head.StateVersion+1)
	manifestFile := filepath.Join(cfg.Skills.ExportRoot, filepath.FromSlash(deleted.Directory), "publication.json")
	data, err := os.ReadFile(manifestFile)
	var manifest struct {
		Head harness.SkillHead `json:"head"`
	}
	if err != nil || json.Unmarshal(data, &manifest) != nil || manifest.Head.Exists || manifest.Head.Enabled {
		t.Fatalf("deleted view: %s %v", data, err)
	}
	if _, err := c.InstallSkill(ctx, "project", "research"); err == nil {
		t.Fatal("missing deleted directory loaded cached content")
	}
}
