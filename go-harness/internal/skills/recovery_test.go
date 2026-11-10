package skills

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func recoveryFixture(t *testing.T) (*Registry, *Registry, harness.SkillTarget, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	writeSkill(t, root, "published", "Published guidance")
	writeFile(t, filepath.Join(root, "published", "reference.txt"), []byte("Published reference"))
	cfg := harness.SkillsConfig{Sources: []harness.SkillSource{{ID: "managed", Root: root, Scope: harness.SkillScopeGlobal}}, ManagedSources: []string{"managed"}, ExportRoot: t.TempDir()}
	history, oldStore := registry(t, cfg)
	target := sourceTarget(history.sources["managed"], "published")
	err := history.WithPublication(ctx, "managed", "published", func(p *PublicationTx) error {
		pkg, e := history.readPackage(ctx, p.source, "published")
		if e != nil {
			return e
		}
		version, e := p.stagePackage(pkg)
		if e != nil {
			return e
		}
		head, e := p.moveHead(SkillHead{}, version, true, true, version, "published")
		if e != nil {
			return e
		}
		if e = p.recordSourceStatus("published", "ready", version, ""); e != nil {
			return e
		}
		for _, sql := range []string{
			`CREATE TABLE df_skill_publications(source_key TEXT,name TEXT,revision INTEGER,payload BLOB)`,
			`CREATE TABLE df_skill_proposals(id TEXT,source_key TEXT,name TEXT,status TEXT,payload BLOB)`,
			`CREATE TABLE harness_sessions(id TEXT)`,
			`INSERT INTO harness_sessions VALUES('old-conversation')`,
		} {
			if _, e = p.tx.ExecContext(ctx, sql); e != nil {
				return e
			}
		}
		publication := harness.SkillPublication{Action: "create", Name: target.Name, Target: target, Revision: 1, ContentVersion: version, Enabled: true, Head: head, ProposalID: "historical-proposal", Actor: "fixture", CreatedAt: time.Now().UTC()}
		body, _ := json.Marshal(publication)
		if _, e = p.tx.ExecContext(ctx, `INSERT INTO df_skill_publications VALUES(?,?,1,?)`, target.SourceIdentity, target.Name, body); e != nil {
			return e
		}
		proposal := harness.SkillProposal{ID: "historical-proposal", Target: target, SkillName: target.Name, Status: "published", Origin: "manual_agent", CandidateVersion: version}
		body, _ = json.Marshal(proposal)
		if _, e = p.tx.ExecContext(ctx, `INSERT INTO df_skill_proposals VALUES(?,?,?,'published',?)`, proposal.ID, target.SourceIdentity, target.Name, body); e != nil {
			return e
		}
		_, e = p.tx.ExecContext(ctx, `INSERT INTO df_skill_file_publication_results VALUES(?,?,1,'applied','fixture')`, target.SourceIdentity, target.Name)
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	var seq int
	var dbName, dbPath string
	if err = oldStore.DB().QueryRow(`PRAGMA database_list`).Scan(&seq, &dbName, &dbPath); err != nil {
		t.Fatal(err)
	}
	current, store := registry(t, cfg)
	if _, err = store.DB().Exec(`CREATE TABLE harness_sessions(id TEXT); INSERT INTO harness_sessions VALUES('current-conversation')`); err != nil {
		t.Fatal(err)
	}
	return current, history, target, dbPath
}

func recoveryInput(plan harness.SkillRecoveryPlan, history string) harness.SkillRecovery {
	return harness.SkillRecovery{Target: plan.Target, HistoryDatabase: history, Version: plan.Version, ReceiptSHA256: plan.ReceiptSHA256, Enabled: plan.Enabled, RequestKey: "recover-once", Actor: "test-host"}
}

func TestRecoverPublishedRegistrationWithoutConversationsOrFileChanges(t *testing.T) {
	ctx := context.Background()
	r, history, target, dbPath := recoveryFixture(t)
	plan, err := r.PlanRecovery(ctx, target, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err = r.db.QueryRow(`SELECT count(*) FROM df_skill_installs`).Scan(&count); err != nil || count != 0 {
		t.Fatal("preview installed files", count, err)
	}
	file := filepath.Join(r.sources["managed"].config.Root, "published", "SKILL.md")
	before, _ := os.ReadFile(file)
	in := recoveryInput(plan, dbPath)
	result, err := r.RecoverPublished(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Skill.Head.Exists || !result.Skill.Head.Enabled || !result.Skill.Head.Managed || result.Skill.Head.Version != plan.Version || result.Skill.SourceStatus != "ready" {
		t.Fatalf("bad recovery: %+v", result)
	}
	var session string
	if err = r.db.QueryRow(`SELECT group_concat(id) FROM harness_sessions`).Scan(&session); err != nil || session != "current-conversation" {
		t.Fatal("conversations migrated", session, err)
	}
	after, _ := os.ReadFile(file)
	if string(before) != string(after) {
		t.Fatal("recovery rewrote source files")
	}
	snap := snapshot(t, r, harness.SkillSelection{IncludeGlobal: true})
	defer snap.Close()
	if _, err = snap.Get(ctx, target.Name); err != nil {
		t.Fatal("recovered skill is not available", err)
	}
	if err = history.Close(); err != nil {
		t.Fatal(err)
	}
	if err = history.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(dbPath, dbPath+".archived"); err != nil {
		t.Fatal(err)
	}
	replay, err := r.RecoverPublished(ctx, in)
	if err != nil || !replay.Replayed || replay.AppliedHead != result.AppliedHead {
		t.Fatal("retry requires old database", replay, err)
	}
	if _, err = r.DeleteChecked(ctx, harness.SkillDeletion{Target: target, Expected: result.AppliedHead, RequestKey: "delete", Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	replay, err = r.RecoverPublished(ctx, in)
	if err != nil || replay.Skill.Head.Exists {
		t.Fatal("retry resurrected deleted skill", replay, err)
	}
}

func TestRecoveryRejectsChangedOrUnpublishedPackages(t *testing.T) {
	for _, mode := range []string{"file_changed", "receipt_changed", "automatic", "unpublished", "deleted_target", "creation_guard", "wrong_source"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			r, old, target, dbPath := recoveryFixture(t)
			plan, err := r.PlanRecovery(ctx, target, dbPath)
			if err != nil {
				t.Fatal(err)
			}
			in := recoveryInput(plan, dbPath)
			switch mode {
			case "file_changed":
				writeFile(t, filepath.Join(r.sources["managed"].config.Root, "published", "reference.txt"), []byte("unreviewed edit"))
			case "receipt_changed":
				in.ReceiptSHA256 = strings.Repeat("0", 64)
			case "automatic":
				_, err = old.db.Exec(`UPDATE df_skill_proposals SET payload=json_set(CAST(payload AS TEXT),'$.origin','automatic')`)
			case "unpublished":
				_, err = old.db.Exec(`UPDATE df_skill_proposals SET status='failed'`)
			case "deleted_target":
				_, err = r.db.Exec(`INSERT INTO df_skill_control VALUES(?,?,1,1,1,'','')`, target.SourceIdentity, target.Name)
			case "creation_guard":
				_, err = r.db.Exec(`INSERT INTO df_skill_creation_guards VALUES(?,?)`, target.SourceIdentity, target.Name)
			case "wrong_source":
				in.Target.SourceIdentity = strings.Repeat("0", 64)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = r.RecoverPublished(ctx, in); err == nil {
				t.Fatal("accepted invalid recovery")
			}
			var count int
			if err = r.db.QueryRow(`SELECT count(*) FROM df_skill_installs`).Scan(&count); err != nil || count != 0 {
				t.Fatal("failed recovery changed installs", count, err)
			}
		})
	}
}

func TestUnregisteredManagedSkillIsDiagnosedWithoutImplicitInstallation(t *testing.T) {
	ctx := context.Background()
	r, _, target, _ := recoveryFixture(t)
	selection := harness.SkillSelection{IncludeGlobal: true}
	for i := 0; i < 2; i++ {
		snap := snapshot(t, r, selection)
		if len(snap.Refs()) != 0 || !strings.Contains(snap.SourceWarnings(), "no installation registration") {
			t.Fatal("missing orphan diagnosis", snap.SourceWarnings())
		}
		_ = snap.Close()
		if err := r.RefreshAllSources(ctx); err != nil {
			t.Fatal(err)
		}
	}
	page, err := r.QueryCatalog(ctx, harness.SkillQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Skills) != 0 || len(page.Diagnostics) != 1 || page.Diagnostics[0].Code != "installation_missing" {
		t.Fatalf("catalog missed orphan: %+v", page)
	}
	var unavailable *harness.SkillAvailabilityError
	if err = r.VisibilityError(ctx, selection, target.Name); !errors.As(err, &unavailable) || unavailable.Reason != "installation_missing" {
		t.Fatal("incorrect model reason", err)
	}
	if err = r.VisibilityError(ctx, harness.SkillSelection{}, target.Name); !errors.As(err, &unavailable) || unavailable.Reason == "installation_missing" {
		t.Fatal("unauthorized source was disclosed", err)
	}
	var bindings int
	if err = r.db.QueryRow(`SELECT count(*) FROM df_skill_source_status`).Scan(&bindings); err != nil || bindings != 0 {
		t.Fatal("diagnosis created an import binding", bindings, err)
	}
	if _, err = r.db.Exec(`INSERT INTO df_skill_control VALUES(?,?,1,1,1,'','')`, target.SourceIdentity, target.Name); err != nil {
		t.Fatal(err)
	}
	page, err = r.QueryCatalog(ctx, harness.SkillQuery{Limit: 10})
	if err != nil || len(page.Diagnostics) != 0 {
		t.Fatal("deleted skill diagnosed as recoverable", page.Diagnostics, err)
	}
}
