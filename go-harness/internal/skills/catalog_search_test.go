package skills

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestCatalogNameContainsFiltersBeforePagination(t *testing.T) {
	ctx := context.Background()
	cfg := harness.SkillsConfig{Sources: []harness.SkillSource{
		{ID: "a", Root: t.TempDir(), Scope: harness.SkillScopeGlobal},
		{ID: "b", Root: t.TempDir(), Scope: harness.SkillScopeGlobal},
	}}
	r, db := registry(t, cfg)
	for _, source := range cfg.Sources {
		names := []string{"instabox-exec-windows"}
		if source.ID == "a" {
			names = append(names, "alpha", "x-instabox-old", "z-instabox-tools")
		}
		for _, name := range names {
			writeSkill(t, source.Root, name, name)
			installEnabled(t, r, source.ID, name)
		}
	}
	if err := r.SetEnabled(ctx, "a", "z-instabox-tools", false); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, "a", "x-instabox-old"); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ source, name, at string }{
		{"a", "alpha", "2026-10-30T06:00:00Z"},
		{"a", "instabox-exec-windows", "2026-10-30T07:00:00Z"},
		{"b", "instabox-exec-windows", "2026-10-30T08:00:00Z"},
		{"a", "z-instabox-tools", "2026-10-30T09:00:00Z"},
	} {
		if _, err := db.DB().Exec(`UPDATE df_skill_installs SET updated_at=? WHERE source_key=? AND name=?`, row.at, r.sources[row.source].identity, row.name); err != nil {
			t.Fatal(err)
		}
	}
	collect := func(t *testing.T, q harness.SkillQuery) []string {
		t.Helper()
		var got []string
		for range 10 {
			page, err := r.QueryCatalog(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			for _, skill := range page.Skills {
				got = append(got, skill.Target.Name)
			}
			if page.Next == "" {
				return got
			}
			q.Cursor = page.Next
		}
		t.Fatal("catalog pagination did not finish")
		return nil
	}
	current := []string{"z-instabox-tools", "instabox-exec-windows", "instabox-exec-windows"}
	for _, keyword := range []string{"inst", "insta", "INST", "  InSt  ", "box-"} {
		t.Run(keyword, func(t *testing.T) {
			got := collect(t, harness.SkillQuery{NameContains: keyword, Status: "current", Limit: 1})
			if !reflect.DeepEqual(got, current) {
				t.Fatalf("substring search lost matching skills across pages: %v", got)
			}
		})
	}
	for _, tc := range []struct {
		name  string
		query harness.SkillQuery
		want  []string
	}{
		{"enabled", harness.SkillQuery{NameContains: "inst", Status: "enabled"}, current[1:]},
		{"disabled", harness.SkillQuery{NameContains: "inst", Status: "disabled"}, current[:1]},
		{"deleted", harness.SkillQuery{NameContains: "inst", Status: "deleted"}, []string{"x-instabox-old"}},
		{"all-states", harness.SkillQuery{NameContains: "inst"}, append(append([]string{}, current...), "x-instabox-old")},
		{"source", harness.SkillQuery{NameContains: "inst", SourceID: "a", Status: "current"}, []string{"z-instabox-tools", "instabox-exec-windows"}},
		{"exact-prefix-does-not-match", harness.SkillQuery{Name: "inst", Status: "current"}, nil},
		{"exact-full-name", harness.SkillQuery{Name: "instabox-exec-windows", Status: "current"}, current[1:]},
		{"combined-filters", harness.SkillQuery{Name: "instabox-exec-windows", NameContains: "exec", Status: "current"}, current[1:]},
		{"combined-filters-disagree", harness.SkillQuery{Name: "instabox-exec-windows", NameContains: "tools", Status: "current"}, nil},
		{"literal-percent", harness.SkillQuery{NameContains: "inst%", Status: "current"}, nil},
		{"literal-underscore", harness.SkillQuery{NameContains: "inst_box", Status: "current"}, nil},
		{"empty", harness.SkillQuery{Status: "current"}, append(append([]string{}, current...), "alpha")},
		{"whitespace", harness.SkillQuery{NameContains: " \t ", Status: "current"}, append(append([]string{}, current...), "alpha")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.query.Limit = 1
			if got := collect(t, tc.query); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("names = %v, want %v", got, tc.want)
			}
		})
	}

	query := harness.SkillQuery{NameContains: "inst", Status: "current", Limit: 1}
	first, err := r.QueryCatalog(ctx, query)
	if err != nil || first.Next == "" {
		t.Fatalf("first search page: %+v %v", first, err)
	}
	for _, change := range []func(*harness.SkillQuery){
		func(q *harness.SkillQuery) { q.NameContains = "insta" },
		func(q *harness.SkillQuery) { q.NameContains = "" },
		func(q *harness.SkillQuery) { q.Name = "instabox-exec-windows" },
		func(q *harness.SkillQuery) { q.SourceID = "a" },
		func(q *harness.SkillQuery) { q.Status = "enabled" },
	} {
		changed := query
		changed.Cursor = first.Next
		change(&changed)
		if _, err := r.QueryCatalog(ctx, changed); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatalf("changed filter accepted cursor: %+v %v", changed, err)
		}
	}
	query.Cursor, query.NameContains = first.Next, "  INST  "
	if _, err := r.QueryCatalog(ctx, query); err != nil {
		t.Fatalf("equivalent normalized substring rejected cursor: %v", err)
	}

	plain, err := r.QueryCatalog(ctx, harness.SkillQuery{Status: "current", Limit: 1})
	if err != nil || len(plain.Skills) != 1 || plain.Skills[0].Target.Name != "z-instabox-tools" {
		t.Fatalf("unfiltered first page: %+v %v", plain, err)
	}
	legacyFilter, _ := json.Marshal([]any{plain.Sources, "", "current", ""})
	legacyCursor := encodeCatalogCursor(catalogCursor{
		Kind: "skill-catalog-v1", Filter: digest(legacyFilter), Name: plain.Skills[0].Target.Name,
		Source: plain.Skills[0].Target.SourceIdentity, Revision: plain.CatalogVersion,
	})
	for _, keyword := range []string{"", " \t "} {
		page, err := r.QueryCatalog(ctx, harness.SkillQuery{NameContains: keyword, Status: "current", Limit: 1, Cursor: plain.Next})
		if err != nil || len(page.Skills) != 1 || page.Skills[0].Target.Name != "instabox-exec-windows" {
			t.Fatalf("empty search did not preserve the recent list cursor: %+v %v", page, err)
		}
		if _, err := r.QueryCatalog(ctx, harness.SkillQuery{NameContains: keyword, Status: "current", Limit: 1, Cursor: legacyCursor}); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatalf("obsolete alphabetical cursor was accepted: %v", err)
		}
	}
	for _, keyword := range []string{strings.Repeat("x", 129), "inst\x00", string([]byte{0xff})} {
		if _, err := r.QueryCatalog(ctx, harness.SkillQuery{NameContains: keyword, Limit: 1}); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatalf("accepted invalid substring: %q %v", keyword, err)
		}
	}
	if err := r.SetEnabled(ctx, "a", "instabox-exec-windows", false); err != nil {
		t.Fatal(err)
	}
	if _, err := r.QueryCatalog(ctx, query); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatalf("stale substring cursor: %v", err)
	}
}
