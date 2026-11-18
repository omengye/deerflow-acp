package memory

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestMemoryStage1UnknownKeyForgetDoesNotClaimDeletion(t *testing.T) {
	for _, origin := range []string{"create", "import", "promote"} {
		t.Run(origin, func(t *testing.T) {
			ctx := context.Background()
			s, scope := personalStore(t)
			candidate := Candidate{Content: "用户最近告知所在地是北京。", Category: "profile", Confidence: 1, Source: Source{ID: "fixture", Kind: "operator"}}
			switch origin {
			case "create":
				if _, err := s.Create(ctx, scope, candidate); err != nil {
					t.Fatal(err)
				}
			case "import":
				raw, err := json.Marshal(map[string]any{"version": "1.0", "facts": []map[string]any{
					{"id": "legacy-city", "content": candidate.Content, "category": candidate.Category, "confidence": 1},
				}})
				if err != nil {
					t.Fatal(err)
				}
				report, err := s.ImportLegacy(ctx, scope, raw)
				if err != nil || report.Imported != 1 {
					t.Fatalf("import fixture: %+v %v", report, err)
				}
			case "promote":
				workspace, err := NewScope(WorkspaceScope, t.TempDir(), "", "", "")
				if err != nil {
					t.Fatal(err)
				}
				source, err := s.Create(ctx, workspace, candidate)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = s.Promote(ctx, workspace, scope, source.ID, source.Revision, Source{ID: "promotion", Kind: "operator"}); err != nil {
					t.Fatal(err)
				}
			}
			page, err := s.List(ctx, scope, "", 10)
			if err != nil || len(page.Facts) != 1 {
				t.Fatalf("fixture facts: %+v %v", page, err)
			}
			key, err := s.FactKey(ctx, scope, page.Facts[0].ID)
			if err != nil || key != "" {
				t.Fatalf("fixture must have no slot: key=%q err=%v", key, err)
			}
			forget := change("forget-missing-key", "city", "", 0)
			forget.Action = "forget"
			receipt, err := s.Mutate(ctx, scope, forget)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.Get(ctx, scope, page.Facts[0].ID); err != nil {
				t.Fatalf("key lookup must not guess the identity of an unslotted fact: %v", err)
			}
			if receipt.Status != "not_found" {
				t.Fatalf("P1-2: missing key must report not_found while the unslotted fact remains; got %s", receipt.Status)
			}
			removed, err := s.Mutate(ctx, scope, Mutation{SchemaVersion: 2, OperationID: "forget-by-id", Action: "forget", FactID: page.Facts[0].ID, ExpectedRevision: page.Facts[0].Revision, Source: Source{ID: "forget-by-id", Kind: "operator"}})
			if err != nil || removed.Status != "forgotten" {
				t.Fatalf("%s ID deletion: %+v %v", origin, removed, err)
			}
			if _, err = s.Get(ctx, scope, page.Facts[0].ID); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("%s remained visible: %v", origin, err)
			}
			listed, err := s.List(ctx, scope, "", 10)
			if err != nil || len(listed.Facts) != 0 {
				t.Fatalf("%s listed after deletion: %+v %v", origin, listed, err)
			}
			for _, read := range []func() (int, error){
				func() (int, error) { hits, err := s.Search(ctx, scope, "北京", 10); return len(hits), err },
				func() (int, error) { hits, _, err := s.SnapshotSearch(ctx, scope, "北京", 10); return len(hits), err },
				func() (int, error) { hits, _, err := s.HybridSearch(ctx, scope, "北京", 10); return len(hits), err },
			} {
				if count, err := read(); err != nil || count != 0 {
					t.Fatalf("%s still retrievable: count=%d err=%v", origin, count, err)
				}
			}
			if origin == "promote" {
				var count int
				if err = s.db.QueryRow("SELECT COUNT(*) FROM memory_facts WHERE deleted=0").Scan(&count); err != nil || count != 1 {
					t.Fatalf("promotion source removed: %d %v", count, err)
				}
			}

		})
	}
}

func TestMemoryStage1ExplicitActionsSuppressAutomaticReinterpretation(t *testing.T) {
	for _, action := range []string{"external_policy", "unchanged"} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			s, scope := personalStore(t)
			if action == "unchanged" {
				if _, err := s.Mutate(ctx, scope, change("seed-name", "profile.name", "用户昵称是小鹿。", 0)); err != nil {
					t.Fatal(err)
				}
			}
			j := stage1AcceptedJob(t, s, scope, "source-run", "source-input", "我在北京")
			explicit := change("explicit-action", "profile.name", "用户昵称是小鹿。", 0)
			explicit.Source.RunID, explicit.Source.InputID = "source-run", "source-input"
			explicit.Source.EventSequence = j.Sequence
			explicit.RequireSnapshot = true
			want := "saved"
			if action == "external_policy" {
				explicit.Source.PolicyVersion = ExternalContextPolicy
			} else {
				explicit.ExpectedRevision, want = 1, "unchanged"
			}
			receipt, err := s.Mutate(ctx, scope, explicit)
			if err != nil || receipt.Status != want {
				t.Fatalf("explicit fixture: %+v %v", receipt, err)
			}
			raw := stage0AttributeJSON(t, ExtractedAttribute{Scope: GlobalScope, Key: "city", Category: "profile", Content: "用户最近告知所在地是北京。", Evidence: j.Text})
			if err = stage3ApplyFixture(t, s, ctx, j, raw); err != nil {
				t.Fatal(err)
			}
			page, err := s.List(ctx, scope, "", 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Facts) != 1 {
				t.Fatalf("P2-6: explicit %s was reinterpreted into an extra fact; facts=%d", action, len(page.Facts))
			}
		})
	}
}

func stage0AttributeJSON(t *testing.T, facts ...ExtractedAttribute) string {
	t.Helper()
	for i := range facts {
		f := &facts[i]
		f.Evidence = strings.TrimSpace(f.Evidence)
		start, end, confidence := 0, len([]rune(f.Evidence)), .9
		f.EvidenceStart, f.EvidenceEnd, f.Confidence = &start, &end, &confidence
		f.Durability, f.Authority = "durable", "descriptive"
		switch f.Evidence {
		case "我在北京":
			f.Value = "北京"
		case "项目使用 SQLite":
			f.Value = "SQLite"
		default:
			f.Value = f.Evidence
		}
	}
	raw, err := json.Marshal(map[string]any{"schema_version": 2, "facts": facts})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
