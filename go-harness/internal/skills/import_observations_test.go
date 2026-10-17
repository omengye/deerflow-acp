package skills

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestManagedImportObservationsSurviveRestartAndDeletion(t *testing.T) {
	r, db, selection := publicationFixture(t)
	ctx := context.Background()
	root := r.sources["project"].config.Root
	writeFile(t, filepath.Join(root, "research", "removed.txt"), []byte("original reference"))
	if _, err := r.Install(ctx, "project", "research"); err != nil {
		t.Fatal(err)
	}
	base := publicationHead(t, r)
	files := map[string][]byte{"SKILL.md": []byte(skillText("research", "Reviewed managed content")), "removed.txt": []byte("original reference")}
	c, err := r.PrepareCandidate("research", files)
	if err != nil {
		t.Fatal(err)
	}
	managed := publishCandidate(t, r, c, base)
	var catalog int64
	if err := db.DB().QueryRow(`SELECT revision FROM df_skill_catalog`).Scan(&catalog); err != nil {
		t.Fatal(err)
	}
	check := func(status string) harness.SkillImportObservation {
		t.Helper()
		if err := r.ObserveImports(ctx); err != nil {
			t.Fatal(err)
		}
		items, err := r.ListImportObservations(ctx, selection)
		if err != nil || len(items) != 1 || items[0].Status != status || items[0].ImportedVersion != managed.Version {
			t.Fatalf("observation: %+v %v", items, err)
		}
		if head := publicationHead(t, r); head != managed {
			t.Fatalf("observation changed publication: %+v", head)
		}
		var next int64
		if err := db.DB().QueryRow(`SELECT revision FROM df_skill_catalog`).Scan(&next); err != nil || next != catalog {
			t.Fatalf("observation changed publication catalog: %d %v", next, err)
		}
		return items[0]
	}
	check("unchanged")
	writeSkill(t, root, "research", "Upstream changed instructions")
	if err := os.Remove(filepath.Join(root, "research", "removed.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "research", "added.txt"), []byte("new upstream reference"))
	changed := check("changed")
	var changes []string
	for _, change := range changed.Changes {
		changes = append(changes, change.Path+":"+change.Change)
	}
	if !reflect.DeepEqual(changes, []string{"SKILL.md:modified", "added.txt:added", "removed.txt:removed"}) || changed.ObservedVersion == managed.Version {
		t.Fatalf("incorrect source comparison: %+v", changed)
	}
	// The saved report is stable until another explicit observation. Reopening
	// needs no import profile, and does not restore the mutable source as live.
	cfg := harness.SkillsConfig{Sources: []harness.SkillSource{r.sources["project"].config}}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err = NewRegistry(ctx, cfg, db.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	items, err := r.ListImportObservations(ctx, selection)
	if err != nil || len(items) != 1 || !reflect.DeepEqual(items[0], changed) {
		t.Fatalf("saved observation: %+v %v", items, err)
	}
	// A removed directory is observed even though launch discovery cannot add it
	// to the next Install list. Use a rename to retain the fixture bytes.
	if err = os.Rename(filepath.Join(root, "research"), filepath.Join(root, "old-research")); err != nil {
		t.Fatal(err)
	}
	check("missing")
	if visible := snapshot(t, r, selection); len(visible.Refs()) != 0 {
		t.Fatal("missing directory loaded cached publication")
	}
	// Scope filtering applies to reports even when no installed head exists.
	if hidden, err := r.ListImportObservations(ctx, harness.SkillSelection{Workspace: t.TempDir()}); err != nil || len(hidden) != 0 {
		t.Fatalf("other workspace exposed reports: %+v %v", hidden, err)
	}
	if hidden, err := r.ListImportObservations(ctx, harness.SkillSelection{Workspace: selection.Workspace, Names: []string{}}); err != nil || len(hidden) != 0 {
		t.Fatalf("empty selection exposed reports: %+v %v", hidden, err)
	}
}

func TestManagedImportObservationFailuresAndBounds(t *testing.T) {
	r, db, selection := publicationFixture(t)
	ctx := context.Background()
	root := r.sources["project"].config.Root
	before := publicationHead(t, r)
	managed := publishCandidate(t, r, candidate(t, r, "Published content"), before)
	assertStatus := func(status string) {
		t.Helper()
		if err := r.ObserveImports(ctx); err != nil {
			t.Fatal(err)
		}
		items, err := r.ListImportObservations(ctx, selection)
		if err != nil || len(items) != 1 || items[0].Status != status {
			t.Fatalf("source status: %+v %v", items, err)
		}
		if head := publicationHead(t, r); head != managed {
			t.Fatalf("source error changed live state: %+v", head)
		}
	}
	writeFile(t, filepath.Join(root, "research", "SKILL.md"), []byte("BROKEN_PRIVATE_CONTENT"))
	assertStatus("invalid")
	writeSkill(t, root, "research", "Updated source")
	for i := 0; i < 110; i++ {
		writeFile(t, filepath.Join(root, "research", fmt.Sprintf("reference-%03d.txt", i)), []byte("PRIVATE_REFERENCE"))
	}
	assertStatus("changed")
	items, err := r.ListImportObservations(ctx, selection)
	if err != nil || len(items) != 1 || !items[0].Truncated || len(items[0].Changes) != 100 {
		t.Fatalf("unbounded source diff: %+v %v", items, err)
	}
	var payload string
	if err := db.DB().QueryRow(`SELECT payload FROM df_skill_import_observations`).Scan(&payload); err != nil || strings.Contains(payload, "PRIVATE_") || strings.Contains(payload, root) {
		t.Fatalf("source content/path leaked: %s %v", payload, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := r.ObserveImports(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation became an advisory: %v", err)
	}
	if _, err = db.DB().Exec(`CREATE TRIGGER observation_failure BEFORE INSERT ON df_skill_import_observations BEGIN SELECT RAISE(ABORT,'injected storage failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err = r.ObserveImports(ctx); err == nil {
		t.Fatal("observation persistence failure was hidden")
	}
	if publicationHead(t, r) != managed {
		t.Fatal("failed observation changed the head")
	}
	var retained string
	if err = db.DB().QueryRow(`SELECT payload FROM df_skill_import_observations`).Scan(&retained); err != nil || retained != payload {
		t.Fatalf("failed observation changed saved report: %v", err)
	}
	if _, err = db.DB().Exec(`DROP TRIGGER observation_failure`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.DB().Exec(`UPDATE df_skill_files SET data='corrupt' WHERE version=?`, managed.Version); err != nil {
		t.Fatal(err)
	}
	if err = r.ObserveImports(ctx); err == nil {
		t.Fatal("corrupt immutable baseline accepted")
	}
}

func TestManagedImportObservationConcurrentPublication(t *testing.T) {
	r, _, selection := publicationFixture(t)
	ctx := context.Background()
	publishCandidate(t, r, candidate(t, r, "Managed original"), publicationHead(t, r))
	writeSkill(t, r.sources["project"].config.Root, "research", "Upstream update")
	next := candidate(t, r, "Next approved publication")
	var wg sync.WaitGroup
	errorsSeen := make(chan error, 4)
	for range 3 {
		wg.Go(func() { errorsSeen <- r.ObserveImports(ctx) })
	}
	wg.Go(func() {
		errorsSeen <- r.WithPublication(ctx, "project", "research", func(tx *PublicationTx) error {
			head, err := tx.Current()
			if err != nil {
				return err
			}
			version, err := tx.Stage(next)
			if err == nil {
				_, err = tx.Publish(version, head, true)
			}
			return err
		})
	})
	wg.Wait()
	close(errorsSeen)
	conflicts := 0
	for err := range errorsSeen {
		if errors.Is(err, ErrPublicationConflict) {
			conflicts++
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	items, err := r.ListImportObservations(ctx, selection)
	if err != nil || len(items) != 1 || items[0].Status != "changed" || publicationHead(t, r).Version == next.Version() || conflicts != 1 {
		t.Fatalf("concurrent observation/publication: %+v %v", items, err)
	}
}
