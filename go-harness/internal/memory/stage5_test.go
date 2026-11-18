package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestMemoryStage5WorkspaceIdentityAndIsolation(t *testing.T) {
	s, global := personalStore(t)
	workspace, err := NewScope(WorkspaceScope, t.TempDir(), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	stage5Publish(t, s, workspace, "seed", "project.database", "project", "PostgreSQL", "The project database is PostgreSQL")
	deleted := stage5Delete(t, s, workspace, "project.database")
	stage5Publish(t, s, workspace, "alias", "project.database", "project", "postgres", "The project database is postgres")
	state, err := s.KeyState(t.Context(), workspace, "project.database")
	if err != nil || !state.Deleted || state.Revision != deleted.Revision {
		t.Fatalf("database alias restored: %+v %v", state, err)
	}
	stage5Publish(t, s, workspace, "new", "project.database", "project", "SQLite", "The project database is SQLite")
	state, err = s.KeyState(t.Context(), workspace, "project.database")
	if err != nil || state.Deleted || state.Revision != deleted.Revision+1 {
		t.Fatalf("workspace restore: %+v %v", state, err)
	}
	other, err := NewScope(WorkspaceScope, t.TempDir(), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []Scope{global, other} {
		state, err = s.KeyState(t.Context(), scope, "project.database")
		if err != nil || state.Exists {
			t.Fatalf("scope leaked: %+v %v", state, err)
		}
	}
}

func TestMemoryStage5RelearnedHeadDoesNotReuseOldIndex(t *testing.T) {
	for _, mode := range []string{"terminal", "late_vector"} {
		t.Run(mode, func(t *testing.T) {
			s, scope := personalStore(t)
			stage5Publish(t, s, scope, "seed", "city", "profile", "北京", "我在北京")
			entered, release := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			e := &stage4Embedder{model: "stage5", embed: func(ctx context.Context, texts []string) ([][]float64, error) {
				if strings.Contains(texts[0], "北京") {
					if mode == "terminal" {
						return nil, ErrEmbeddingInput
					}
					close(entered)
					select {
					case <-release:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				return stage4Vectors(ctx, texts)
			}}
			s.SetEmbedder(e)
			if err := s.SetIndexConfig(harness.MemoryIndexConfig{MaxAttempts: 1}); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			if mode == "terminal" {
				_ = s.IndexPending(t.Context())
				failures, err := s.IndexFailures(t.Context(), 10)
				if err != nil || len(failures) != 1 || !failures[0].Terminal {
					t.Fatalf("terminal fixture: %+v %v", failures, err)
				}
			} else {
				go func() { done <- s.IndexPending(t.Context()) }()
				select {
				case <-entered:
				case <-t.Context().Done():
					t.Fatal(t.Context().Err())
				}
			}
			deleted := stage5Delete(t, s, scope, "city")
			stage5Publish(t, s, scope, "new", "city", "profile", "上海", "我在上海")
			if mode == "late_vector" {
				unblock()
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				var n int
				if err := s.db.QueryRow("SELECT count(*) FROM memory_vectors").Scan(&n); err != nil || n != 0 {
					t.Fatalf("late vector resurrected old revision: %d %v", n, err)
				}
			}
			if err := s.IndexPending(t.Context()); err != nil {
				t.Fatal(err)
			}
			var revision int64
			if err := s.db.QueryRow("SELECT revision FROM memory_vectors WHERE fact_id=?", deleted.FactID).Scan(&revision); err != nil || revision != deleted.Revision+1 {
				t.Fatalf("new head not indexed: %d %v", revision, err)
			}
			failures, err := s.IndexFailures(t.Context(), 10)
			if err != nil || len(failures) != 0 || e.calls != 2 {
				t.Fatalf("old failure blocked new head: %+v calls=%d %v", failures, e.calls, err)
			}
		})
	}
}

func TestMemoryStage5MixedBatchAndProofSubstitution(t *testing.T) {
	t.Run("mixed_batch", func(t *testing.T) {
		s, scope := personalStore(t)
		stage5Publish(t, s, scope, "seed", "city", "profile", "北京", "我在北京")
		deleted := stage5Delete(t, s, scope, "city")
		j := stage1AcceptedJob(t, s, scope, "new", "new-input", "我在北京。我叫小鹿")
		var raw map[string]any
		_ = json.Unmarshal([]byte(stage5Raw(GlobalScope, "city", "profile", "北京", "我在北京")), &raw)
		var sibling map[string]any
		_ = json.Unmarshal([]byte(stage5Raw(GlobalScope, "name", "profile", "小鹿", "我叫小鹿")), &sibling)
		item := sibling["facts"].([]any)[0].(map[string]any)
		item["evidence_start"], item["evidence_end"] = 5, 9
		raw["facts"] = append(raw["facts"].([]any), item)
		encoded, _ := json.Marshal(raw)
		if err := stage3ApplyFixture(t, s, t.Context(), j, string(encoded)); err != nil {
			t.Fatal(err)
		}
		city, err := s.KeyState(t.Context(), scope, "city")
		if err != nil || !city.Deleted || city.Revision != deleted.Revision {
			t.Fatalf("same city restored: %+v %v", city, err)
		}
		name, err := s.KeyState(t.Context(), scope, "name")
		if err != nil || !name.Exists || name.Deleted {
			t.Fatalf("valid sibling lost: %+v %v", name, err)
		}
		var decision string
		if err = s.db.QueryRow("SELECT decision FROM memory_inputs WHERE input_id=?", j.InputID).Scan(&decision); err != nil || decision != "processed" {
			t.Fatalf("mixed outcome misclassified: %s %v", decision, err)
		}
	})
	for _, mode := range []string{"source", "value", "content", "automatic"} {
		t.Run(mode, func(t *testing.T) {
			s, scope := personalStore(t)
			stage5Publish(t, s, scope, "seed", "city", "profile", "北京", "我在北京")
			deleted := stage5Delete(t, s, scope, "city")
			j := stage1AcceptedJob(t, s, scope, "new", "new-input", "我在上海")
			start, end := 0, 4
			text := j.Text
			if mode == "source" {
				text = "我在上海。我叫小鹿"
			}
			valid, err := ValidateWrite(text, WriteRequest{Action: "remember", Key: "city", Category: "profile", Value: "上海", Evidence: "我在上海", EvidenceStart: &start, EvidenceEnd: &end}, mode != "automatic")
			if err != nil {
				t.Fatal(err)
			}
			if mode == "value" {
				valid.Value = "广州"
			}
			if mode == "content" {
				valid.Content = "用户最近告知所在地是广州。"
			}
			_, err = s.Mutate(t.Context(), scope, Mutation{SchemaVersion: 2, RequireSnapshot: true, Automatic: true, Validated: &valid, OperationID: fmt.Sprint("substitution-", mode), Action: "remember", Key: "city", Category: "profile", Content: valid.Content, ExpectedRevision: deleted.Revision, Source: Source{ID: "substitution", Kind: "model", RunID: j.RunID, InputID: j.InputID, EventSequence: j.Sequence, PolicyVersion: ExtractionPolicyV2}})
			if !errors.Is(err, harness.ErrInvalidInput) {
				t.Fatalf("substituted proof accepted: %v", err)
			}
		})
	}
}

func stage5Raw(scope ScopeKind, key, category, value, text string) string {
	raw, _ := json.Marshal(map[string]any{"schema_version": 2, "facts": []any{map[string]any{"scope": scope, "key": key, "category": category, "value": value, "evidence": text, "evidence_start": 0, "evidence_end": len([]rune(text)), "durability": "durable", "authority": "descriptive", "confidence": .95}}})
	return string(raw)
}

func stage5Explicit(t *testing.T, s *Store, scope Scope, j Job, key, category, value, evidence string, revision int64, op string) (Receipt, error) {
	t.Helper()
	start := strings.Index(j.Text, evidence)
	if start < 0 {
		t.Fatal("fixture evidence not present")
	}
	start = len([]rune(j.Text[:start]))
	end := start + len([]rune(evidence))
	valid, err := ValidateWrite(j.Text, WriteRequest{Action: "remember", Key: key, Category: category, Value: value, Evidence: evidence, EvidenceStart: &start, EvidenceEnd: &end}, false)
	if err != nil {
		return Receipt{}, err
	}
	return s.Mutate(t.Context(), scope, Mutation{SchemaVersion: 2, RequireSnapshot: true, Validated: &valid, OperationID: op, Action: "remember", Key: valid.Key, Category: valid.Category, Content: valid.Content, ExpectedRevision: revision, Source: Source{ID: op, Kind: "operator", RunID: j.RunID, InputID: j.InputID, EventSequence: j.Sequence, PolicyVersion: ExplicitPolicyV2}})
}

func TestMemoryStage5ExplicitRestoreRequiresActualRequest(t *testing.T) {
	for _, tc := range []struct {
		name, text, evidence string
		allowed              bool
	}{
		{"statement_only", "我在北京", "我在北京", false},
		{"explicit_prefix", "请记住我在北京", "请记住我在北京", true},
		{"separate_prefix", "请记住，我在北京", "我在北京", true},
		{"unrelated_request", "请记住我叫小鹿。我在北京", "我在北京", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, scope := personalStore(t)
			stage5Publish(t, s, scope, "seed", "city", "profile", "北京", "我在北京")
			deleted := stage5Delete(t, s, scope, "city")
			j := stage1AcceptedJob(t, s, scope, "fresh", "fresh-input", tc.text)
			r, err := stage5Explicit(t, s, scope, j, "city", "profile", "北京", tc.evidence, deleted.Revision, "restore")
			if err != nil {
				t.Fatal(err)
			}
			if tc.allowed {
				if r.Status != "updated" || r.Revision != deleted.Revision+1 {
					t.Fatalf("explicit restore: %+v", r)
				}
			} else if r.Status != "requires_explicit" || r.Revision != deleted.Revision {
				t.Fatalf("unrequested restore: %+v", r)
			}
			again, err := stage5Explicit(t, s, scope, j, "city", "profile", "北京", tc.evidence, deleted.Revision, "restore")
			if err != nil || again != r {
				t.Fatalf("receipt replay changed: %+v %+v %v", r, again, err)
			}
		})
	}
}

func TestMemoryStage5UnsupportedAndLegacyValuesStayDeleted(t *testing.T) {
	for _, mode := range []string{"legacy_value", "empty_tombstone", "untyped_correction", "generic_value", "legacy_input", "saved_legacy_result"} {
		t.Run(mode, func(t *testing.T) {
			s, scope := personalStore(t)
			key, category, value, text := "city", "profile", "上海", "我在上海"
			switch mode {
			case "legacy_value":
				if _, err := s.Mutate(t.Context(), scope, change("old", "city", "用户最近告知所在地是北京。", 0)); err != nil {
					t.Fatal(err)
				}
			case "empty_tombstone":
			case "generic_value":
				key, category, value, text = "preference.drink", "preference", "我喜欢咖啡", "请记住我喜欢咖啡"
				stage5Publish(t, s, scope, "seed", key, category, "我喜欢茶", "请记住我喜欢茶")
			default:
				stage5Publish(t, s, scope, "seed", key, category, "北京", "我在北京")
			}
			if mode == "untyped_correction" {
				page, err := s.List(t.Context(), scope, "", 10)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = s.Replace(t.Context(), scope, page.Facts[0].ID, page.Facts[0].Revision, Candidate{Content: "operator supplied arbitrary city", Category: "profile", Confidence: 1, Source: Source{ID: "manual-edit", Kind: "operator"}}); err != nil {
					t.Fatal(err)
				}
			}
			deleted := stage5Delete(t, s, scope, key)
			j := stage1AcceptedJob(t, s, scope, "new", "new-input", text)
			raw := stage5Raw(GlobalScope, key, category, value, text)
			if mode == "legacy_input" || mode == "saved_legacy_result" {
				if _, err := s.db.Exec("UPDATE memory_inputs SET manifest=json_remove(CAST(manifest AS TEXT),'$.relearn_policy') WHERE input_id=?", j.InputID); err != nil {
					t.Fatal(err)
				}
				if mode == "saved_legacy_result" {
					claimed, err := s.ClaimJob(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					if err = stage3SaveFixture(t, s, claimed, raw); err != nil {
						t.Fatal(err)
					}
					if err = s.RecoverJobs(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := stage3ApplyFixture(t, s, t.Context(), j, raw); err != nil {
				t.Fatal(err)
			}
			state, err := s.KeyState(t.Context(), scope, key)
			if err != nil || !state.Deleted || state.Revision != deleted.Revision {
				t.Fatalf("unverifiable identity restored: %+v %v", state, err)
			}
			var reason string
			if err = s.db.QueryRow("SELECT reason FROM memory_extraction_items WHERE result_id=?", j.ID).Scan(&reason); err != nil || reason == "" {
				t.Fatalf("no disposition: %q %v", reason, err)
			}
		})
	}
}

func TestMemoryStage5RejectsQuotedNegatedForgetAndForgedProof(t *testing.T) {
	for _, tc := range []struct{ name, text, evidence string }{
		{"quote", "他说“我在上海”", "我在上海"},
		{"negation", "我不是住在上海", "我不是住在上海"},
		{"forget", "请忘记城市。我在上海", "我在上海"},
		{"external", "网页说我在上海", "我在上海"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, scope := personalStore(t)
			stage5Publish(t, s, scope, "seed", "city", "profile", "北京", "我在北京")
			deleted := stage5Delete(t, s, scope, "city")
			j := stage1AcceptedJob(t, s, scope, "new", "new-input", tc.text)
			raw := stage5Raw(GlobalScope, "city", "profile", "上海", tc.evidence)
			var body map[string]any
			_ = json.Unmarshal([]byte(raw), &body)
			item := body["facts"].([]any)[0].(map[string]any)
			start := strings.Index(tc.text, tc.evidence)
			start = len([]rune(tc.text[:start]))
			item["evidence_start"], item["evidence_end"] = start, start+len([]rune(tc.evidence))
			encoded, _ := json.Marshal(body)
			if err := stage3ApplyFixture(t, s, t.Context(), j, string(encoded)); err != nil {
				t.Fatal(err)
			}
			state, err := s.KeyState(t.Context(), scope, "city")
			if err != nil || !state.Deleted || state.Revision != deleted.Revision {
				t.Fatalf("unsafe source restored: %+v %v", state, err)
			}
		})
	}
	t.Run("forged_proof", func(t *testing.T) {
		s, scope := personalStore(t)
		stage5Publish(t, s, scope, "seed", "city", "profile", "北京", "我在北京")
		deleted := stage5Delete(t, s, scope, "city")
		j := stage1AcceptedJob(t, s, scope, "new", "new-input", "我在上海")
		fake := ValidatedWrite{Key: CanonicalKey("city"), Category: "profile", Value: "上海", Content: "用户最近告知所在地是上海。"}
		_, err := s.Mutate(t.Context(), scope, Mutation{SchemaVersion: 2, RequireSnapshot: true, Automatic: true, Validated: &fake, OperationID: "forged", Action: "remember", Key: "city", Category: "profile", Content: fake.Content, ExpectedRevision: deleted.Revision, Source: Source{ID: "forged", Kind: "model", RunID: j.RunID, InputID: j.InputID, EventSequence: j.Sequence, PolicyVersion: ExtractionPolicyV2}})
		if !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatalf("unsealed proof accepted: %v", err)
		}
	})
}

func TestMemoryStage5OldInputsAndConcurrentCorrectionsCannotRestore(t *testing.T) {
	s, scope := personalStore(t)
	stage5Publish(t, s, scope, "seed", "city", "profile", "北京", "我在北京")
	old := stage1AcceptedJob(t, s, scope, "old", "old-input", "我在上海")
	deleted := stage5Delete(t, s, scope, "city")
	if err := stage3ApplyFixture(t, s, t.Context(), old, stage5Raw(GlobalScope, "city", "profile", "上海", "我在上海")); err != nil {
		t.Fatal(err)
	}
	state, err := s.KeyState(t.Context(), scope, "city")
	if err != nil || !state.Deleted || state.Revision != deleted.Revision {
		t.Fatalf("old source restored: %+v %v", state, err)
	}
	a := stage1AcceptedJob(t, s, scope, "a", "input-a", "我在上海")
	b := stage1AcceptedJob(t, s, scope, "b", "input-b", "我在广州")
	raws := map[string]string{a.ID: stage5Raw(GlobalScope, "city", "profile", "上海", "我在上海"), b.ID: stage5Raw(GlobalScope, "city", "profile", "广州", "我在广州")}
	var claimed []Job
	for range 2 {
		j, e := s.ClaimJob(t.Context())
		if e != nil {
			t.Fatal(e)
		}
		if e = stage3SaveFixture(t, s, j, raws[j.ID]); e != nil {
			t.Fatal(e)
		}
		claimed = append(claimed, j)
	}
	begin := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, j := range claimed {
		wg.Add(1)
		go func() { defer wg.Done(); <-begin; errs <- s.ApplyJob(t.Context(), j, raws[j.ID]) }()
	}
	close(begin)
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	state, err = s.KeyState(t.Context(), scope, "city")
	if err != nil || state.Deleted || state.Revision != deleted.Revision+1 {
		t.Fatalf("competing revisions overwrote each other: %+v %v", state, err)
	}
	var updated, superseded int
	if err = s.db.QueryRow("SELECT count(*) FROM memory_extraction_items WHERE status='updated'").Scan(&updated); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow("SELECT count(*) FROM memory_extraction_items WHERE status='superseded'").Scan(&superseded); err != nil || updated != 1 || superseded < 1 {
		t.Fatalf("updated=%d superseded=%d %v", updated, superseded, err)
	}
}

func TestMemoryStage5IdentityReceiptAndFactCommitAtomically(t *testing.T) {
	s, scope := personalStore(t)
	stage5Publish(t, s, scope, "seed", "city", "profile", "北京", "我在北京")
	deleted := stage5Delete(t, s, scope, "city")
	j := stage1AcceptedJob(t, s, scope, "new", "new-input", "我在上海")
	raw := stage5Raw(GlobalScope, "city", "profile", "上海", "我在上海")
	claimed, err := s.ClaimJob(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = stage3SaveFixture(t, s, claimed, raw); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("CREATE TRIGGER fail_value_identity BEFORE INSERT ON memory_value_identities BEGIN SELECT RAISE(ABORT,'identity_failure'); END"); err != nil {
		t.Fatal(err)
	}
	if err = s.ApplyJob(t.Context(), claimed, raw); err == nil {
		t.Fatal("identity failure lost")
	}
	state, err := s.KeyState(t.Context(), scope, "city")
	if err != nil || !state.Deleted || state.Revision != deleted.Revision {
		t.Fatalf("partial restore: %+v %v", state, err)
	}
	var status string
	var rows int
	if err = s.db.QueryRow("SELECT status FROM memory_jobs WHERE id=?", j.ID).Scan(&status); err != nil || status != "running" {
		t.Fatalf("job completed before fact: %s %v", status, err)
	}
	if err = s.db.QueryRow("SELECT count(*) FROM memory_extraction_items WHERE result_id=?", j.ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("partial receipt/items: %d %v", rows, err)
	}
	if _, err = s.db.Exec("DROP TRIGGER fail_value_identity"); err != nil {
		t.Fatal(err)
	}
	if err = s.RecoverJobs(t.Context()); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(t.Context(), s.db)
	if err != nil {
		t.Fatal(err)
	}
	stage3AttachLedger(t, reopened)
	if err = stage3ApplyFixture(t, reopened, t.Context(), j, raw); err != nil {
		t.Fatal(err)
	}
	if err = reopened.db.QueryRow("SELECT count(*) FROM budget_reservations WHERE member_id=?", j.MemberID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("recovery billed twice: %d %v", rows, err)
	}
}

func TestMemoryStage5UnknownBudgetStillBlocksRelearning(t *testing.T) {
	s, scope := personalStore(t)
	stage5Publish(t, s, scope, "seed", "city", "profile", "北京", "我在北京")
	deleted := stage5Delete(t, s, scope, "city")
	_ = stage1AcceptedJob(t, s, scope, "new", "new-input", "我在上海")
	j, err := s.ClaimJob(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	g, err := s.ReserveJobCall(t.Context(), j, "fixture", 10, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DispatchJobCall(t.Context(), j, g); err != nil {
		t.Fatal(err)
	}
	if err = s.AbortJob(t.Context(), j, context.Canceled); err != nil {
		t.Fatal(err)
	}
	if err = s.ApplyJob(t.Context(), j, stage5Raw(GlobalScope, "city", "profile", "上海", "我在上海")); err == nil {
		t.Fatal("unknown usage restored fact")
	}
	state, err := s.KeyState(t.Context(), scope, "city")
	if err != nil || !state.Deleted || state.Revision != deleted.Revision {
		t.Fatalf("%+v %v", state, err)
	}
	snap, err := s.ledger.Snapshot(t.Context(), j.RootBudgetID)
	if err != nil || snap.HeldTokens == 0 {
		t.Fatalf("unknown hold lost: %+v %v", snap, err)
	}
}

func stage5Publish(t *testing.T, s *Store, scope Scope, id, key, category, value, text string) Job {
	t.Helper()
	j := stage1AcceptedJob(t, s, scope, id, "input-"+id, text)
	if err := stage3ApplyFixture(t, s, t.Context(), j, stage5Raw(scope.Kind(), key, category, value, text)); err != nil {
		t.Fatal(err)
	}
	return j
}
func stage5Delete(t *testing.T, s *Store, scope Scope, key string) Receipt {
	t.Helper()
	state, err := s.KeyState(t.Context(), scope, key)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Mutate(t.Context(), scope, Mutation{SchemaVersion: 2, OperationID: "delete-" + key, Action: "forget", Key: key, ExpectedRevision: state.Revision, Source: Source{ID: "delete-" + key, Kind: "operator"}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestMemoryStage5RelearnNewStructuredValue(t *testing.T) {
	s, scope := personalStore(t)
	stage5Publish(t, s, scope, "seed", "city", "profile", "北京", "我在北京")
	removed := stage5Delete(t, s, scope, "city")
	j := stage5Publish(t, s, scope, "new", "city", "profile", "上海", "我在上海")
	state, err := s.KeyState(t.Context(), scope, "city")
	if err != nil || state.Deleted || state.Revision != removed.Revision+1 {
		t.Fatalf("new post-deletion value not learned: %+v %v", state, err)
	}
	fact, err := s.Get(t.Context(), scope, removed.FactID)
	if err != nil || fact.Content != "用户最近告知所在地是上海。" {
		t.Fatalf("new value unavailable: %+v %v", fact, err)
	}
	if err = stage3ApplyFixture(t, s, t.Context(), j, stage5Raw(GlobalScope, "city", "profile", "上海", "我在上海")); err != nil {
		t.Fatal(err)
	}
	after, err := s.KeyState(t.Context(), scope, "city")
	if err != nil || after.Revision != state.Revision {
		t.Fatalf("duplicate restoration: %+v %v", after, err)
	}
}

func TestMemoryStage5SameNormalizedValueRequiresExplicit(t *testing.T) {
	for _, tc := range []struct{ name, key, cat, old, oldText, new, newText string }{
		{"name_case_punctuation", "name", "profile", "Alice", "My name is Alice", "AL-ICE", "My name is AL-ICE"},
		{"city_template", "city", "profile", "北京", "我在北京", "北京", "我的城市是北京"},
		{"city_width_space", "city", "profile", "London", "I live in London", "ＬＯＮＤＯＮ", "I am in ＬＯＮＤＯＮ"},
		{"language_alias", "language", "preference", "英语", "以后请用英语回答", "English", "Please answer in English"},
		{"hardware_punctuation", "profile.hardware", "profile", "BOARD-1", "My hardware is BOARD-1", "board_1", "My hardware is board_1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, scope := personalStore(t)
			stage5Publish(t, s, scope, "seed", tc.key, tc.cat, tc.old, tc.oldText)
			deleted := stage5Delete(t, s, scope, tc.key)
			j := stage5Publish(t, s, scope, "new", tc.key, tc.cat, tc.new, tc.newText)
			state, err := s.KeyState(t.Context(), scope, tc.key)
			if err != nil || !state.Deleted || state.Revision != deleted.Revision {
				t.Fatalf("same value restored: %+v %v", state, err)
			}
			var status, reason string
			if err = s.db.QueryRow("SELECT status,reason FROM memory_extraction_items WHERE result_id=? AND ordinal=0", j.ID).Scan(&status, &reason); err != nil || status != "requires_explicit" || reason != "same_deleted_value" {
				t.Fatalf("missing disposition: %s %s %v", status, reason, err)
			}
		})
	}
}
