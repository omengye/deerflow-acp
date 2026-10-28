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
	r, _ := registry(t, cfg)
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
	current := []string{"instabox-exec-windows", "instabox-exec-windows", "z-instabox-tools"}
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
		{"enabled", harness.SkillQuery{NameContains: "inst", Status: "enabled"}, current[:2]},
		{"disabled", harness.SkillQuery{NameContains: "inst", Status: "disabled"}, current[2:]},
		{"deleted", harness.SkillQuery{NameContains: "inst", Status: "deleted"}, []string{"x-instabox-old"}},
		{"all-states", harness.SkillQuery{NameContains: "inst"}, []string{"instabox-exec-windows", "instabox-exec-windows", "x-instabox-old", "z-instabox-tools"}},
		{"source", harness.SkillQuery{NameContains: "inst", SourceID: "a", Status: "current"}, []string{"instabox-exec-windows", "z-instabox-tools"}},
		{"exact-prefix-does-not-match", harness.SkillQuery{Name: "inst", Status: "current"}, nil},
		{"exact-full-name", harness.SkillQuery{Name: "instabox-exec-windows", Status: "current"}, current[:2]},
		{"combined-filters", harness.SkillQuery{Name: "instabox-exec-windows", NameContains: "exec", Status: "current"}, current[:2]},
		{"combined-filters-disagree", harness.SkillQuery{Name: "instabox-exec-windows", NameContains: "tools", Status: "current"}, nil},
		{"literal-percent", harness.SkillQuery{NameContains: "inst%", Status: "current"}, nil},
		{"literal-underscore", harness.SkillQuery{NameContains: "inst_box", Status: "current"}, nil},
		{"empty", harness.SkillQuery{Status: "current"}, append([]string{"alpha"}, current...)},
		{"whitespace", harness.SkillQuery{NameContains: " \t ", Status: "current"}, append([]string{"alpha"}, current...)},
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
	if err != nil || len(plain.Skills) != 1 || plain.Skills[0].Target.Name != "alpha" {
		t.Fatalf("unfiltered first page: %+v %v", plain, err)
	}
	legacyFilter, _ := json.Marshal([]any{plain.Sources, "", "current", ""})
	legacyCursor := encodeCatalogCursor(catalogCursor{
		Kind: "skill-catalog-v1", Filter: digest(legacyFilter), Name: "alpha",
		Source: plain.Skills[0].Target.SourceIdentity, Revision: plain.CatalogVersion,
	})
	for _, keyword := range []string{"", " \t "} {
		page, err := r.QueryCatalog(ctx, harness.SkillQuery{NameContains: keyword, Status: "current", Limit: 1, Cursor: legacyCursor})
		if err != nil || len(page.Skills) != 1 || page.Skills[0].Target.Name != "instabox-exec-windows" {
			t.Fatalf("legacy list cursor was not preserved: %+v %v", page, err)
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
