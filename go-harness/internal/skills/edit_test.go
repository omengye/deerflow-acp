package skills

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func editString(s string) *string { return &s }
func fanrenEdits() []harness.SkillFileEdit {
	return []harness.SkillFileEdit{
		{Path: "SKILL.md", Operation: "replace", Find: editString("Original instructions"), Replace: editString("Use the new endpoint")},
		{Path: "config.json", Operation: "json_set", Pointer: "/BaseUrl", Value: json.RawMessage(`"https://new.example:8443"`)},
		{Path: "query.py", Operation: "replace", Find: editString("old-agent"), Replace: editString("Mozilla/5.0")},
	}
}

func editFixture(t *testing.T) (*Registry, harness.SkillSelection, *Snapshot) {
	t.Helper()
	r, _, selection := publicationFixture(t)
	root := r.sources["project"].config.Root
	writeFile(t, filepath.Join(root, "research", "config.json"), []byte("{\"BaseUrl\":\"https://old.example\",\"Token\":\"fixture-private-token-123\"}\r\n"))
	writeFile(t, filepath.Join(root, "research", "query.py"), []byte("import os\r\ntoken = os.environ.get('TOKEN')\r\nheaders = {'User-Agent': 'old-agent'}\r\n"))
	if err := r.InstallConfigured(context.Background(), "project", "research", true); err != nil {
		t.Fatal(err)
	}
	return r, selection, snapshot(t, r, selection)
}

func TestInstalledEditMultiFileSourceAndSnapshots(t *testing.T) {
	r, selection, old := editFixture(t)
	ctx := context.Background()
	ref := old.Refs()[0]
	inspection, err := r.InspectSkill(ctx, ref)
	if err != nil || inspection.Storage != "source" || !inspection.Editable {
		t.Fatalf("inspect: %+v %v", inspection, err)
	}
	before := publicationHead(t, r)
	review, err := r.PrepareEdit(ctx, ref, "owner", "first", fanrenEdits())
	if err != nil {
		t.Fatal(err)
	}
	if !validEditID(review.ID) || len(review.Changes) != 3 || review.Behavior != "not_run" {
		t.Fatalf("bad review: %+v", review)
	}
	data, _ := json.Marshal(review)
	if strings.Contains(string(data), "fixture-private-token") || !strings.Contains(string(data), "new.example") {
		t.Fatalf("unsafe/missing preview: %s", data)
	}
	if publicationHead(t, r) != before {
		t.Fatal("prepare changed head")
	}
	disk, err := r.readPackage(ctx, r.sources["project"], "research")
	if err != nil || (&Candidate{pkg: disk}).Version() != before.Version {
		t.Fatal("prepare changed disk", err)
	}
	replayed, err := r.PrepareEdit(ctx, ref, "owner", "first", fanrenEdits())
	if err != nil || replayed.ID != review.ID {
		t.Fatal("non-idempotent prepare", err)
	}
	if _, err = r.PrepareEdit(ctx, ref, "owner", "first", fanrenEdits()[:1]); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatal("changed intent reused", err)
	}
	forged := review
	forged.Candidate = before.Version
	if _, err = r.ApplyEdit(ctx, review.ID, "owner", forged); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("forged approval accepted", err)
	}
	result, err := r.ApplyEdit(ctx, review.ID, "owner", review)
	if err != nil || result.Status != "applied" || !result.LiveMatches || !result.SourceMatches {
		if rec, e := readEdit(ctx, r.db, review.ID); e == nil {
			var base, candidate *Candidate
			_ = r.WithPublication(ctx, "project", "research", func(p *PublicationTx) error {
				base, _ = p.ReadVersion(review.Base.Version)
				candidate, _ = p.ReadVersion(review.Candidate)
				return nil
			})
			t.Log("source failure detail:", r.applyEditFiles(ctx, r.sources["project"], rec, base, candidate))
		}
		t.Fatalf("apply: %+v %v", result, err)
	}
	if result.AppliedHead.Enabled != before.Enabled || result.AppliedHead.Managed != before.Managed {
		t.Fatal("changed unrelated control state")
	}
	again, err := r.ApplyEdit(ctx, review.ID, "owner", review)
	if err != nil || again.AppliedHead != result.AppliedHead {
		t.Fatal("replay advanced state", err)
	}
	if snapshot(t, r, selection).Refs()[0].Version != review.Candidate || old.Refs()[0].Version != ref.Version {
		t.Fatal("snapshot semantics changed")
	}
	oldBody, _ := old.ReadFile(ctx, "research", "SKILL.md")
	if !strings.Contains(string(oldBody), "Original instructions") {
		t.Fatal("mutated old snapshot")
	}
	config, err := os.ReadFile(filepath.Join(inspection.SourceDirectory, "config.json"))
	if err != nil || !strings.Contains(string(config), "fixture-private-token-123") || !strings.Contains(string(config), "https://new.example:8443") {
		t.Fatal("config not preserved", err)
	}
}

func TestInstalledEditConflictsAndRecovery(t *testing.T) {
	for _, mode := range []string{"disk-conflict", "head-conflict", "recover-partial", "recover-external", "prepared-no-recovery"} {
		t.Run(mode, func(t *testing.T) {
			r, selection, old := editFixture(t)
			ctx := context.Background()
			ref := old.Refs()[0]
			before := publicationHead(t, r)
			review, err := r.PrepareEdit(ctx, ref, "owner", "test", fanrenEdits())
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(r.sources["project"].config.Root, "research")
			if mode == "disk-conflict" {
				writeFile(t, filepath.Join(root, "query.py"), []byte("external edit"))
			}
			if mode == "head-conflict" {
				if err := r.SetEnabled(ctx, "project", "research", false); err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasSuffix(mode, "conflict") {
				if _, err := r.ApplyEdit(ctx, review.ID, "owner", review); !errors.Is(err, harness.ErrSkillConflict) {
					t.Fatal("conflict missed", err)
				}
				return
			}
			if mode != "prepared-no-recovery" {
				err = r.WithPublication(ctx, "project", "research", func(p *PublicationTx) error {
					rec, e := readEdit(ctx, p.tx, review.ID)
					if e != nil {
						return e
					}
					rec.Status = "applying"
					c, e := p.ReadVersion(review.Candidate)
					if e != nil {
						return e
					}
					writeFile(t, filepath.Join(root, "SKILL.md"), c.Files()["SKILL.md"])
					return saveEdit(ctx, p.tx, rec)
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "recover-external" {
				writeFile(t, filepath.Join(root, "query.py"), []byte("external edit"))
			}
			if err := r.RecoverEdits(ctx); err != nil {
				t.Fatal(err)
			}
			state, err := r.EditStatus(ctx, review.ID, "owner")
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "recover-partial":
				if state.Status != "applied" || !state.SourceMatches || !state.LiveMatches {
					t.Fatalf("recovery: %+v", state)
				}
			case "prepared-no-recovery":
				if state.Status != "prepared" || publicationHead(t, r) != before {
					t.Fatal("unapproved recovery")
				}
			case "recover-external":
				if state.Status != "needs_reconciliation" || publicationHead(t, r) != before {
					t.Fatalf("external edit lost: %+v", state)
				}
				if err := r.InstallConfigured(ctx, "project", "research", true); err != nil {
					t.Fatal(err)
				}
				if err := r.ObserveImports(ctx); err != nil {
					t.Fatal(err)
				}
				fresh := snapshot(t, r, selection)
				if len(fresh.Refs()) != 0 || !strings.Contains(fresh.SourceWarnings(), "edit_reconciliation_required") {
					t.Fatal("pending edit fell back to cached installation")
				}
				if restored, e := r.Restore(ctx, selection, old.Refs()); e != nil || restored.Refs()[0].Version != ref.Version {
					t.Fatal("historical execution lost", e)
				}
				if err := r.SetEnabled(ctx, "project", "research", false); !errors.Is(err, harness.ErrReconciliationRequired) {
					t.Fatal("publication not blocked", err)
				}
				inspection, err := r.InspectSkill(ctx, ref)
				if err != nil || inspection.Editable {
					t.Fatal("cannot inspect pending edit", err)
				}
			}
		})
	}
}

func TestInstalledEditStorageOwnership(t *testing.T) {
	r, selection, old := editFixture(t)
	ctx := context.Background()
	head := publicationHead(t, r)
	_, err := r.SetEnabledChecked(ctx, harness.SkillEnablement{Target: sourceTarget(r.sources["project"], "research"), Expected: head, RequestKey: "toggle", Actor: "operator", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	i, err := r.InspectSkill(ctx, old.Refs()[0])
	if err != nil || i.Storage != "source" {
		t.Fatalf("enablement changed ownership: %+v %v", i, err)
	}
	head = publicationHead(t, r)
	publishCandidate(t, r, candidate(t, r, "Managed overlay instructions"), head)
	i, err = r.InspectSkill(ctx, snapshot(t, r, selection).Refs()[0])
	if err != nil || i.Storage != "source" {
		t.Fatalf("publication lost file authority: %+v %v", i, err)
	}
}

func TestInstalledEditRejectsUnsafeContent(t *testing.T) {
	base := map[string][]byte{"config.json": []byte(`{"BaseUrl":"https://old.example","Token":"fixture-private-token-123"}`), "query.py": []byte("token = os.environ.get('TOKEN')\nprint('old')\n")}
	cases := []harness.SkillFileEdit{
		{Path: "../config.json", Operation: "write", Content: editString("x")},
		{Path: "config.json", Operation: "json_set", Pointer: "/Token", Value: json.RawMessage(`"new"`)},
		{Path: "config.json", Operation: "json_set", Pointer: "/BaseUrl", Value: json.RawMessage(`"fixture-private-token-123"`)},
		{Path: "query.py", Operation: "replace", Find: editString("missing"), Replace: editString("new")},
		{Path: "query.py", Operation: "write", Content: editString("token = 'new-secret'\n")},
	}
	for _, c := range cases {
		if _, _, err := applyFileEdits(base, []harness.SkillFileEdit{c}); err == nil {
			t.Fatalf("accepted unsafe edit: %+v", c)
		}
	}
	if _, err := setEditJSON([]byte(`{"Token":"a","Token":"b","url":"old"}`), "/url", json.RawMessage(`"new"`)); err == nil {
		t.Fatal("duplicate key accepted")
	}
}
