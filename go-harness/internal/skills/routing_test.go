package skills

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestAutomaticRoutesPinTasksAndHideScopedContentAcrossWorkspaces(t *testing.T) {
	ctx := context.Background()
	root, workspace, other := t.TempDir(), t.TempDir(), t.TempDir()
	writeSkill(t, root, "research", "Public baseline")
	r, db := registry(t, harness.SkillsConfig{Sources: []harness.SkillSource{{ID: "project", Root: root, Scope: harness.SkillScopeGlobal}}})
	if err := r.InstallConfigured(ctx, "project", "research", true); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`CREATE TABLE budget_members(id TEXT,root_id TEXT); CREATE TABLE harness_runs(id TEXT,status TEXT)`); err != nil {
		t.Fatal(err)
	}
	base := publicationHead(t, r)
	c := candidate(t, r, "Scoped candidate")
	var head SkillHead
	var target harness.SkillTarget
	if err := r.WithPublication(ctx, "project", "research", func(pub *PublicationTx) error {
		target = pub.Target()
		v, e := pub.Stage(c)
		if e == nil {
			head, e = pub.PublishAutomatic(v, base, true, workspace)
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
	sampled := ""
	for i := 0; sampled == ""; i++ {
		v := fmt.Sprintf("root-%d", i)
		if strings.ContainsRune("0123", rune(digest([]byte(v + "/" + target.SourceIdentity + "/research"))[0])) {
			sampled = v
		}
	}
	check := func(scope, task, version string) *Snapshot {
		t.Helper()
		snap, e := r.Snapshot(ctx, harness.SkillSelection{Workspace: scope, IncludeGlobal: true, RootID: task})
		if e != nil {
			t.Fatal(e)
		}
		if len(snap.Refs()) != 1 || snap.Refs()[0].Version != version {
			t.Fatal("incorrect routed version", scope, task, snap.Refs())
		}
		return snap
	}
	ordinary := check(workspace, "", base.Version)
	defer ordinary.Close()
	foreign := check(other, sampled, base.Version)
	defer foreign.Close()
	observed := check(workspace, sampled, head.Version)
	defer observed.Close()
	ref := observed.Refs()[0]
	if _, err := r.Restore(ctx, harness.SkillSelection{Workspace: other, IncludeGlobal: true}, []harness.SkillRef{ref}); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("cross-workspace restore disclosed scoped content", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "research", "SKILL.md"))
	if err != nil || !strings.Contains(string(data), "Public baseline") {
		t.Fatal("automatic observation changed public files", err)
	}
	// A newer owner edit wins the global CAS; existing task snapshots stay pinned.
	next := publishCandidate(t, r, candidate(t, r, "Owner replacement"), head)
	check(other, "new-root", next.Version).Close()
	if observed.Refs()[0].Version != head.Version || ordinary.Refs()[0].Version != base.Version {
		t.Fatal("publication rewrote running snapshots")
	}
	if err := r.WithPublication(ctx, "project", "research", func(pub *PublicationTx) error {
		_, e := pub.PublishAutomatic(base.Version, head, true, workspace)
		return e
	}); !errors.Is(err, ErrPublicationConflict) {
		t.Fatal("late rollback overwrote owner edit", err)
	}
}

func TestForeignNewSkillNameDoesNotBlockIndependentWorkspaceOrBypassTombstone(t *testing.T) {
	ctx := context.Background()
	root, first, second := t.TempDir(), t.TempDir(), t.TempDir()
	r, db := registry(t, harness.SkillsConfig{Sources: []harness.SkillSource{{ID: "project", Root: root, Scope: harness.SkillScopeGlobal}}})
	c, err := r.PrepareCandidate("json-audit", map[string][]byte{"SKILL.md": []byte("---\nname: json-audit\ndescription: Audit JSON\n---\nCheck every required field\n")})
	if err != nil {
		t.Fatal(err)
	}
	if err = r.WithPublication(ctx, "project", "json-audit", func(pub *PublicationTx) error {
		v, err := pub.Stage(c)
		if err == nil {
			_, err = pub.PublishAutomatic(v, SkillHead{}, true, first)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	a, err := r.NewMatchAccess(harness.SkillSelection{IncludeGlobal: true})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = a.NewCapabilityTargetTx(ctx, tx, first, "project", "json-audit"); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatal("same workspace bypassed occupied name", err)
	}
	target, err := a.NewCapabilityTargetTx(ctx, tx, second, "project", "json-audit")
	if err != nil || target.Name != WorkspaceSkillName("json-audit", second) {
		t.Fatal("foreign scope blocked independent creation", target, err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO df_skill_control(source_key,name,state_version,managed,deleted,last_import_version,import_directory) VALUES(?,?,1,1,1,'',?)`, target.SourceIdentity, target.Name, target.Name); err != nil {
		t.Fatal(err)
	}
	if _, err = a.NewCapabilityTargetTx(ctx, tx, second, "project", "json-audit"); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatal("workspace alias bypassed owner deletion", err)
	}
}
