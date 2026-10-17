package skills

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func catalogItem(t *testing.T, r *Registry, sourceID, name string) harness.SkillCatalogEntry {
	t.Helper()
	page, err := r.QueryCatalog(context.Background(), harness.SkillQuery{SourceID: sourceID, Name: name, Limit: 10})
	if err != nil || len(page.Skills) != 1 {
		t.Fatalf("catalog item: %+v %v", page, err)
	}
	return page.Skills[0]
}

func TestCatalogPaginationFiltersAndStaleCursors(t *testing.T) {
	ctx := context.Background()
	a, b := t.TempDir(), t.TempDir()
	cfg := harness.SkillsConfig{Sources: []harness.SkillSource{{ID: "a", Root: a, Scope: harness.SkillScopeGlobal}, {ID: "b", Root: b, Scope: harness.SkillScopeGlobal}}}
	r, db := registry(t, cfg)
	for _, source := range cfg.Sources {
		for _, name := range []string{"alpha", "beta", "gamma"} {
			writeSkill(t, source.Root, name, source.ID+name)
			installEnabled(t, r, source.ID, name)
		}
	}
	if err := r.SetEnabled(ctx, "b", "alpha", false); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, "a", "gamma"); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	q := harness.SkillQuery{Limit: 1}
	first, err := r.QueryCatalog(ctx, q)
	if err != nil || first.Next == "" || len(first.Sources) != 2 {
		t.Fatalf("first: %+v %v", first, err)
	}
	lastName := ""
	for {
		page, err := r.QueryCatalog(ctx, q)
		if err != nil || len(page.Skills) != 1 {
			t.Fatalf("page: %+v %v", page, err)
		}
		e := page.Skills[0]
		key := e.Target.SourceID + "/" + e.Target.Name
		if seen[key] || e.Target.Name < lastName {
			t.Fatalf("duplicate or unordered item: %+v", e)
		}
		seen[key], lastName = true, e.Target.Name
		if !e.Head.Exists && (e.Record != nil || e.Head.StateVersion == 0) {
			t.Fatalf("invalid tombstone: %+v", e)
		}
		if page.Next == "" {
			break
		}
		q.Cursor = page.Next
	}
	if len(seen) != 6 {
		t.Fatalf("missing targets: %+v", seen)
	}
	for status, count := range map[string]int{"enabled": 4, "disabled": 1, "deleted": 1} {
		page, err := r.QueryCatalog(ctx, harness.SkillQuery{Status: status, Limit: 100})
		if err != nil || len(page.Skills) != count {
			t.Fatalf("filter %s: %+v %v", status, page, err)
		}
	}
	for _, bad := range []harness.SkillQuery{{Limit: 0}, {Limit: 101}, {Limit: 1, Status: "published"}, {Limit: 1, Cursor: "bad"}, {Limit: 1, Cursor: first.Next, SourceID: "a"}} {
		if _, err := r.QueryCatalog(ctx, bad); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatalf("accepted query %+v: %v", bad, err)
		}
	}
	if err := r.SetEnabled(ctx, "b", "alpha", true); err != nil {
		t.Fatal(err)
	}
	if _, err := r.QueryCatalog(ctx, harness.SkillQuery{Limit: 1, Cursor: first.Next}); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatalf("stale publication cursor: %v", err)
	}
	entry := catalogItem(t, r, "a", "alpha")
	changed := cfg
	changed.Sources = append([]harness.SkillSource(nil), cfg.Sources...)
	changed.Sources[0].Root = t.TempDir()
	r2, err := NewRegistry(ctx, changed, db.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	if _, err = r2.GetCatalogSkill(ctx, entry.Target); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatalf("rebound source accepted old target: %v", err)
	}
	page, err := r2.QueryCatalog(ctx, harness.SkillQuery{SourceID: "a", Limit: 100})
	if err != nil || len(page.Skills) != 0 {
		t.Fatalf("rebound source exposed former records: %+v %v", page, err)
	}
}

func TestCatalogEnablementDurableIdempotentAndImportRefresh(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeSkill(t, root, "research", "original")
	cfg := harness.SkillsConfig{Sources: []harness.SkillSource{{ID: "project", Root: root, Scope: harness.SkillScopeGlobal}}, BuiltinSources: []string{"project"}}
	r, db := registry(t, cfg)
	if err := r.InstallConfigured(ctx, "project", "research", true); err != nil {
		t.Fatal(err)
	}
	entry := catalogItem(t, r, "project", "research")
	before := snapshot(t, r, harness.SkillSelection{IncludeGlobal: true})
	request := harness.SkillEnablement{Target: entry.Target, Expected: entry.Head, Enabled: false, RequestKey: "disable", Actor: "test-operator"}
	result, err := r.SetEnabledChecked(ctx, request)
	if err != nil || result.Replayed || !result.AppliedHead.Managed || result.AppliedHead.Enabled || result.Skill.OverlayBase != nil || !result.Skill.CanRefreshImport {
		t.Fatalf("disable: %+v %v", result, err)
	}
	if len(before.Refs()) != 1 || len(snapshot(t, r, harness.SkillSelection{IncludeGlobal: true}).Refs()) != 0 {
		t.Fatal("enablement changed old snapshot or failed to affect future selection")
	}
	enable := harness.SkillEnablement{Target: entry.Target, Expected: result.AppliedHead, Enabled: true, RequestKey: "enable", Actor: "test-operator"}
	enabled, err := r.SetEnabledChecked(ctx, enable)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := r.SetEnabledChecked(ctx, request)
	if err != nil || !replay.Replayed || replay.AppliedHead != result.AppliedHead || replay.Skill.Head != enabled.AppliedHead {
		t.Fatalf("old retry overwrote newer state: %+v %v", replay, err)
	}
	bad := request
	bad.Enabled = true
	if _, err := r.SetEnabledChecked(ctx, bad); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatalf("reused key accepted changed intent: %v", err)
	}
	bad = request
	bad.RequestKey = "stale"
	if _, err := r.SetEnabledChecked(ctx, bad); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatalf("stale head accepted: %v", err)
	}
	writeSkill(t, root, "research", "modified outside publication")
	observed, err := r.RefreshCatalogImport(ctx, entry.Target, enabled.AppliedHead)
	if err != nil || observed.Skill.SourceStatus != "ready" || observed.Skill.Head.Version == enabled.AppliedHead.Version || !observed.Skill.Head.Enabled {
		t.Fatalf("observation: %+v %v", observed, err)
	}
	if _, err := r.RefreshCatalogImport(ctx, entry.Target, request.Expected); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatalf("refresh accepted stale head: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := NewRegistry(ctx, cfg, db.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	if err = r2.InstallConfigured(ctx, "project", "research", false); err != nil {
		t.Fatal(err)
	}
	after := catalogItem(t, r2, "project", "research")
	if after.Head.Version == enabled.AppliedHead.Version || !after.Head.Enabled || after.OverlayBase != nil || !after.Builtin {
		t.Fatalf("startup failed to observe files or changed durable enablement: %+v", after)
	}
	replay, err = r2.SetEnabledChecked(ctx, request)
	if err != nil || !replay.Replayed || replay.AppliedHead != result.AppliedHead || replay.Skill.Head != after.Head {
		t.Fatalf("restart lost audit: %+v %v", replay, err)
	}
	if err = r2.Delete(ctx, "project", "research"); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("builtin delete allowed", err)
	}
}

func TestCatalogEnablementAtomicAndConcurrent(t *testing.T) {
	ctx := context.Background()
	r, db, _, _ := managedExportFixture(t)
	publishCandidate(t, r, candidate(t, r, "original"), SkillHead{})
	entry := catalogItem(t, r, "project", "research")
	request := harness.SkillEnablement{Target: entry.Target, Expected: entry.Head, Enabled: !entry.Head.Enabled, RequestKey: "rollback", Actor: "operator"}
	if _, err := db.DB().Exec(`CREATE TRIGGER reject_management BEFORE INSERT ON df_skill_management_requests BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetEnabledChecked(ctx, request); err == nil {
		t.Fatal("ignored audit failure")
	}
	after := catalogItem(t, r, "project", "research")
	if after.Head != entry.Head || after.Export.StateToken != entry.Export.StateToken {
		t.Fatal("audit failure committed head or export queue")
	}
	if _, err := db.DB().Exec(`DROP TRIGGER reject_management`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, key := range []string{"a", "b"} {
		wg.Go(func() {
			req := request
			req.RequestKey = key
			_, err := r.SetEnabledChecked(ctx, req)
			results <- err
		})
	}
	wg.Wait()
	close(results)
	wins, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, harness.ErrSkillConflict):
			conflicts++
		default:
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("writers: %d wins %d conflicts", wins, conflicts)
	}
}

func TestCatalogMetadataOnlyAndCorruptAudit(t *testing.T) {
	ctx := context.Background()
	r, db, _, _ := managedExportFixture(t)
	publishCandidate(t, r, candidate(t, r, "original"), SkillHead{})
	entry := catalogItem(t, r, "project", "research")
	req := harness.SkillEnablement{Target: entry.Target, Expected: entry.Head, Enabled: !entry.Head.Enabled, RequestKey: "audit", Actor: "operator"}
	if _, err := r.SetEnabledChecked(ctx, req); err != nil {
		t.Fatal(err)
	}
	// Listing must not load package bodies. Corrupt file bytes remain an error
	// for execution/export verification, but do not prevent inspecting metadata.
	if _, err := db.DB().Exec(`UPDATE df_skill_files SET data='corrupt fixture'`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetCatalogSkill(ctx, entry.Target); err != nil {
		t.Fatalf("catalog loaded body: %v", err)
	}
	if _, err := db.DB().Exec(`UPDATE df_skill_management_requests SET applied_head=json_set(applied_head,'$.state_version',999)`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetEnabledChecked(ctx, req); err == nil || errors.Is(err, harness.ErrSkillConflict) {
		t.Fatalf("corrupt applied head accepted or treated as user conflict: %v", err)
	}
	if _, err := db.DB().Exec(`UPDATE df_skill_versions SET metadata='{}'`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetCatalogSkill(ctx, entry.Target); err == nil {
		t.Fatal("corrupt metadata accepted")
	}
}

func TestCatalogExportRetryPreconditionsAndReplay(t *testing.T) {
	ctx := context.Background()
	r, db, cfg, sel := managedExportFixture(t)
	publishCandidate(t, r, candidate(t, r, "original"), SkillHead{})
	entry := catalogItem(t, r, "project", "research")
	// Save the work item before a new operator retry. Its eventual result must
	// not settle the new queue generation, even when head/attempts are identical.
	s := r.sources["project"]
	old, err := readExport(db.DB().QueryRow(`SELECT `+exportColumns+` FROM df_skill_exports WHERE source_key=? AND name=? AND export_key=?`, s.identity, "research", r.export.key), s, r.export.key)
	if err != nil {
		t.Fatal(err)
	}
	req := harness.SkillExportRetry{Target: entry.Target, Expected: entry.Head, ExpectedToken: entry.Export.StateToken, RequestKey: "retry-one", Actor: "operator"}
	queued, err := r.RetryExportChecked(ctx, req)
	if err != nil || queued.Replayed || queued.Skill.Export.StateToken == req.ExpectedToken || queued.Skill.Export.Status != "pending" {
		t.Fatalf("retry: %+v %v", queued, err)
	}
	if err = r.syncExport(ctx, old); err != nil {
		t.Fatal(err)
	}
	state := exportState(t, r, sel, "pending")
	if state.StateToken != queued.Skill.Export.StateToken {
		t.Fatal("old worker settled new retry")
	}
	stale := req
	stale.RequestKey = "stale-state"
	if _, err = r.RetryExportChecked(ctx, stale); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatalf("stale token accepted: %v", err)
	}
	if err = r.SyncExports(ctx); err != nil {
		t.Fatal(err)
	}
	synced := exportState(t, r, sel, "synced")
	replayed, err := r.RetryExportChecked(ctx, req)
	if err != nil || !replayed.Replayed || replayed.Skill.Export.StateToken != synced.StateToken {
		t.Fatalf("replay reset worker: %+v %v", replayed, err)
	}
	file := filepath.Join(cfg.ExportRoot, filepath.FromSlash(synced.Directory), "files", "SKILL.md")
	if err = os.WriteFile(file, []byte("edited export"), 0600); err != nil {
		t.Fatal(err)
	}
	req.ExpectedToken, req.RequestKey = synced.StateToken, "repair"
	if _, err = r.RetryExportChecked(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err = r.SyncExports(ctx); err != nil {
		t.Fatal(err)
	}
	fixed := exportState(t, r, sel, "synced")
	if fixed.Directory == synced.Directory {
		t.Fatal("edited view was not rebuilt")
	}
	if bytes, err := os.ReadFile(file); err != nil || string(bytes) != "edited export" {
		t.Fatal("repair overwrote edited view")
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.ExportRoot = t.TempDir()
	r2, err := NewRegistry(ctx, cfg, db.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	if err = r2.ReconcileExports(ctx, false); err != nil {
		t.Fatal(err)
	}
	req.ExpectedToken, req.RequestKey = fixed.StateToken, "old-root"
	if _, err = r2.RetryExportChecked(ctx, req); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatalf("rebound export root accepted stale token: %v", err)
	}
}
