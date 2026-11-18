package memory

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

// Minimal host acceptance fixture; integration tests exercise actual BeginRun.
func stage1AcceptedJob(t *testing.T, s *Store, scope Scope, run, input, text string) Job {
	t.Helper()
	ctx := t.Context()
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS harness_runs(id TEXT PRIMARY KEY,input_id TEXT,session_id TEXT,status TEXT);
 CREATE TABLE IF NOT EXISTS harness_sessions(id TEXT PRIMARY KEY);
 CREATE TABLE IF NOT EXISTS harness_inputs(id TEXT PRIMARY KEY,run_id TEXT,content BLOB);
 CREATE TABLE IF NOT EXISTS harness_events(sequence INTEGER PRIMARY KEY AUTOINCREMENT,run_id TEXT,event BLOB);
 CREATE TABLE IF NOT EXISTS harness_background_specs(child_session_id TEXT);
 CREATE TABLE IF NOT EXISTS harness_tool_receipts(session_id TEXT,receipt BLOB);`)
	if err != nil {
		t.Fatal(err)
	}
	if s.ledger == nil {
		ledger, e := budget.New(s.db, budget.Config{CheckEffectTx: s.CheckJobBudgetEffectTx, CheckHeartbeatTx: s.CheckJobBudgetHeartbeatTx})
		if e != nil {
			t.Fatal(e)
		}
		s.SetJobLedger(ledger)
	}
	req := harness.RunRequest{RunID: run, InputID: input, RootBudgetID: run,
		Session: harness.Session{ID: "session-" + run, CWD: t.TempDir()}, Input: []harness.Content{{Type: "text", Text: text}}}
	if scope.Kind() == WorkspaceScope {
		req.Session.CWD = scope.Workspace()
	}
	raw, err := json.Marshal(req.Input)
	if err != nil {
		t.Fatal(err)
	}
	err = transact(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT INTO harness_sessions(id) VALUES(?)", req.Session.ID); err != nil {
			return err
		}
		if err := s.ledger.CreateRootTx(ctx, tx, budget.RootSpec{RootBudgetID: run, RootRunID: run, SessionID: req.Session.ID}); err != nil {
			return err
		}
		if err := s.ledger.BeginAttemptTx(ctx, tx, budget.Scope{RootBudgetID: run, MemberID: run, SessionID: req.Session.ID, AttemptID: "attempt-" + run, Fence: 1}); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO harness_runs(id,input_id,session_id,status) VALUES(?,?,?,'completed')", run, input, req.Session.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO harness_inputs(id,run_id,content) VALUES(?,?,?)", input, run, raw); err != nil {
			return err
		}
		event, _ := json.Marshal(harness.RunEvent{RunID: run, SessionID: req.Session.ID, Kind: "user_message", Content: req.Input})
		result, err := tx.ExecContext(ctx, "INSERT INTO harness_events(run_id,event) VALUES(?,?)", run, event)
		if err != nil {
			return err
		}
		sequence, err := result.LastInsertId()
		if err != nil {
			return err
		}
		return s.CaptureInputTx(ctx, tx, req, sequence, raw, scope.subject, []Scope{scope}, true, true)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Enqueue(ctx, Job{RunID: run, InputID: input, AttemptID: "attempt-" + run}); err != nil {
		t.Fatal(err)
	}
	j := Job{Version: 2, BudgetPolicy: JobBudgetPolicy, MemberID: jobMember(OperationID("extract/v3", run, input)), WritePolicy: WritePolicy, ExtractionSchema: ExtractionSchemaVersion, ID: OperationID("extract/v3", run, input), RunID: run, InputID: input, RootBudgetID: run, SessionID: req.Session.ID}
	if err = s.ledger.EndAttempt(ctx, budget.Scope{RootBudgetID: run, MemberID: run, SessionID: req.Session.ID, AttemptID: "attempt-" + run, Fence: 1}, budget.OutcomeCompleted); err != nil {
		t.Fatal(err)
	}
	err = transact(ctx, s.db, func(tx *sql.Tx) error { return hydrateJob(ctx, tx, &j) })
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func stage1City(t *testing.T) string {
	return stage0AttributeJSON(t, ExtractedAttribute{Scope: GlobalScope, Key: "city", Category: "profile", Content: "用户最近告知所在地是北京。", Evidence: "我在北京"})
}

func TestMemoryStage1ClearAndUnslottedDeleteFenceOldInputs(t *testing.T) {
	for _, action := range []string{"empty_clear", "clear", "delete", "forget_id"} {
		t.Run(action, func(t *testing.T) {
			ctx := t.Context()
			s, scope := personalStore(t)
			var fact Fact
			var err error
			if action != "empty_clear" {
				fact, err = s.Create(ctx, scope, Candidate{Content: "用户最近告知所在地是北京。", Category: "profile", Confidence: 1, Source: Source{ID: "seed", Kind: "operator"}})
				if err != nil {
					t.Fatal(err)
				}
			}
			job := stage1AcceptedJob(t, s, scope, "old-run", "old-input", "我在北京")
			before, err := s.InputSnapshot(ctx, job.RunID, job.InputID)
			if err != nil {
				t.Fatal(err)
			}
			switch action {
			case "empty_clear", "clear":
				revision, err := s.Revision(ctx, scope)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = s.Clear(ctx, scope, revision, "clear", "operator"); err != nil {
					t.Fatal(err)
				}
			case "delete":
				if err = s.Delete(ctx, scope, fact.ID, fact.Revision, Source{ID: "delete", Kind: "operator"}); err != nil {
					t.Fatal(err)
				}
			case "forget_id":
				if _, err = s.Mutate(ctx, scope, Mutation{SchemaVersion: 2, OperationID: "forget-id", Action: "forget", FactID: fact.ID, ExpectedRevision: fact.Revision, Source: Source{ID: "forget-id", Kind: "operator"}}); err != nil {
					t.Fatal(err)
				}
			}
			// Caller-provided revisions/profile/text cannot rebase the durable job.
			job.ProfileID = "forged"
			job.Versions = map[string]map[string]int64{"global": {CanonicalKey("city"): 99}}
			if err = stage3ApplyFixture(t, s, ctx, job, stage1City(t)); err != nil {
				t.Fatal(err)
			}
			page, err := s.List(ctx, scope, "", 20)
			if err != nil || len(page.Facts) != 0 {
				t.Fatalf("old source escaped barrier: %+v %v", page, err)
			}
			after, err := s.InputSnapshot(ctx, job.RunID, job.InputID)
			if err != nil {
				t.Fatal(err)
			}
			a, _ := json.Marshal(before)
			b, _ := json.Marshal(after)
			if string(a) != string(b) {
				t.Fatal("source manifest changed")
			}
			fresh := stage1AcceptedJob(t, s, scope, "new-run", "new-input", "我在北京")
			if err = stage3ApplyFixture(t, s, ctx, fresh, stage1City(t)); err != nil {
				t.Fatal(err)
			}
			page, err = s.List(ctx, scope, "", 20)
			if err != nil || len(page.Facts) != 1 {
				t.Fatalf("new source rejected without a key tombstone: %+v %v", page, err)
			}
		})
	}
}

func TestMemoryStage1IDDeletionReceiptAtomicityAndIsolation(t *testing.T) {
	ctx := t.Context()
	s, scope := personalStore(t)
	fact, err := s.Create(ctx, scope, Candidate{Content: "fixture city", Category: "profile", Confidence: 1, Source: Source{ID: "seed", Kind: "operator"}})
	if err != nil {
		t.Fatal(err)
	}
	in := Mutation{SchemaVersion: 2, OperationID: "forget", Action: "forget", FactID: fact.ID, ExpectedRevision: 1, Source: Source{ID: "forget", Kind: "operator"}}
	if _, err = s.db.Exec(`CREATE TRIGGER fail_receipt BEFORE INSERT ON memory_operations BEGIN SELECT RAISE(ABORT,'receipt_failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Mutate(ctx, scope, in); err == nil || !strings.Contains(err.Error(), "receipt_failure") {
		t.Fatalf("wrong injected failure: %v", err)
	}
	if _, err = s.Get(ctx, scope, fact.ID); err != nil {
		t.Fatalf("partial deletion: %v", err)
	}
	var barriers int
	if err = s.db.QueryRow("SELECT COUNT(*) FROM memory_scope_barriers").Scan(&barriers); err != nil || barriers != 0 {
		t.Fatalf("barrier escaped rollback: %d %v", barriers, err)
	}
	if _, err = s.db.Exec("DROP TRIGGER fail_receipt"); err != nil {
		t.Fatal(err)
	}
	first, err := s.Mutate(ctx, scope, in)
	if err != nil || first.Status != "forgotten" || !first.Barrier {
		t.Fatalf("receipt: %+v %v", first, err)
	}
	replay, err := s.Mutate(ctx, scope, in)
	if err != nil || replay != first {
		t.Fatalf("lost-response replay: %+v %v", replay, err)
	}
	changedRequest := in
	changedRequest.ExpectedRevision++
	if _, err = s.Mutate(ctx, scope, changedRequest); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("same operation accepted a changed request: %v", err)
	}
	second := in
	second.OperationID = "forget-again"
	second.ExpectedRevision = first.Revision
	again, err := s.Mutate(ctx, scope, second)
	if err != nil || again.Status != "already_forgotten" || again.Revision != first.Revision {
		t.Fatalf("repeated delete: %+v %v", again, err)
	}
	other, err := s.Create(ctx, scope, Candidate{Content: "another fact", Category: "profile", Confidence: 1, Source: Source{ID: "other", Kind: "operator"}})
	if err != nil {
		t.Fatal(err)
	}
	collision := in
	collision.FactID = other.ID
	if _, err = s.Mutate(ctx, scope, collision); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("target collision: %v", err)
	}
	if MutationOperationID(scope, in) == MutationOperationID(scope, collision) {
		t.Fatal("different ID targets share an operation identity")
	}
	foreign, err := NewScope(GlobalScope, "", "", "other-profile", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Mutate(ctx, foreign, in); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("same operation accepted a changed scope: %v", err)
	}
	collision.OperationID = "cross-scope"
	miss, err := s.Mutate(ctx, foreign, collision)
	if err != nil || miss.Status != "not_found" {
		t.Fatalf("cross-scope delete: %+v %v", miss, err)
	}
	if _, err = s.Get(ctx, scope, other.ID); err != nil {
		t.Fatal("deleted another profile's fact")
	}
	in.OperationID = "invalid-two-targets"
	in.Key = "city"
	if _, err = s.Mutate(ctx, scope, in); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatalf("two targets accepted: %v", err)
	}
}

func TestMemoryStage1SlottedIDForgetPreservesCASAndTombstone(t *testing.T) {
	ctx := t.Context()
	s, scope := personalStore(t)
	seed, err := s.Mutate(ctx, scope, change("seed", "city", "北京", 0))
	if err != nil {
		t.Fatal(err)
	}
	old := stage1AcceptedJob(t, s, scope, "old", "old-input", "我在北京")
	remove := Mutation{SchemaVersion: 2, OperationID: "forget-id", Action: "forget", FactID: seed.FactID,
		Source: Source{ID: "forget-id", Kind: "operator"}}
	if _, err = s.Mutate(ctx, scope, remove); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale revision deleted a slotted fact: %v", err)
	}
	remove.ExpectedRevision = seed.Revision
	receipt, err := s.Mutate(ctx, scope, remove)
	if err != nil || receipt.Status != "forgotten" || !receipt.Barrier || receipt.Key != CanonicalKey("city") {
		t.Fatalf("ID deletion lost its slot: %+v %v", receipt, err)
	}
	key, err := s.FactKey(ctx, scope, seed.FactID)
	if err != nil || key != CanonicalKey("city") {
		t.Fatalf("ID deletion broke the version chain: %q %v", key, err)
	}
	fresh := stage1AcceptedJob(t, s, scope, "fresh", "fresh-input", "我在北京")
	for _, job := range []Job{old, fresh} {
		if err = stage3ApplyFixture(t, s, ctx, job, stage1City(t)); err != nil {
			t.Fatal(err)
		}
		state, err := s.KeyState(ctx, scope, "city")
		if err != nil || !state.Deleted || state.Revision != receipt.Revision {
			t.Fatalf("legacy value or pre-deletion source restored automatically: %+v %v", state, err)
		}
	}
}

func TestMemoryStage1ExplicitRestoreNeedsNewAcceptedInput(t *testing.T) {
	ctx := t.Context()
	s, scope := personalStore(t)
	seed, err := s.Mutate(ctx, scope, change("seed", "city", "北京", 0))
	if err != nil {
		t.Fatal(err)
	}
	old := stage1AcceptedJob(t, s, scope, "old", "old-input", "请记住我在北京")
	forget := change("delete", "city", "", seed.Revision)
	forget.Action = "forget"
	deleted, err := s.Mutate(ctx, scope, forget)
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.KeyState(ctx, scope, "city")
	if err != nil || !state.Deleted || state.Revision != deleted.Revision {
		t.Fatalf("tombstone metadata: %+v %v", state, err)
	}
	raw, _ := json.Marshal(state)
	if strings.Contains(string(raw), "北京") {
		t.Fatal("deleted content leaked through metadata")
	}
	restore := change("stale-restore", "city", "北京", state.Revision)
	restore.RequireSnapshot = true
	restore.Source.RunID = old.RunID
	restore.Source.InputID = old.InputID
	restore.Source.EventSequence = old.Sequence
	if _, err = s.Mutate(ctx, scope, restore); !errors.Is(err, ErrSourceSuperseded) {
		t.Fatalf("old input upgraded by state read: %v", err)
	}
	fresh := stage1AcceptedJob(t, s, scope, "fresh", "fresh-input", "请记住我在北京")
	restore.OperationID = "fresh-restore"
	restore.Source.RunID = fresh.RunID
	restore.Source.InputID = fresh.InputID
	restore.Source.EventSequence = fresh.Sequence
	start, end := 0, len([]rune("请记住我在北京"))
	valid, validationErr := ValidateWrite(fresh.Text, WriteRequest{Action: "remember", Key: "city", Category: "profile", Value: "北京", Evidence: "请记住我在北京", EvidenceStart: &start, EvidenceEnd: &end}, false)
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	restore.Content, restore.Validated = valid.Content, &valid
	restore.Source.PolicyVersion = ExplicitPolicyV2
	if _, err = s.Mutate(ctx, scope, restore); err != nil {
		t.Fatal(err)
	}
	// Even after explicit recovery, the old source remains behind the last deletion.
	restore.OperationID = "stale-after-restore"
	restore.ExpectedRevision++
	restore.Source.RunID = old.RunID
	restore.Source.InputID = old.InputID
	restore.Source.EventSequence = old.Sequence
	if _, err = s.Mutate(ctx, scope, restore); !errors.Is(err, ErrSourceSuperseded) {
		t.Fatalf("old source crossed historical deletion: %v", err)
	}
}

func TestMemoryStage1NoOpActionsSuppressClaimAndApply(t *testing.T) {
	for _, action := range []string{"unchanged", "not_found", "already_forgotten"} {
		t.Run(action, func(t *testing.T) {
			ctx := t.Context()
			s, scope := personalStore(t)
			seed := change("seed", "profile.name", "用户昵称是小鹿。", 0)
			var revision int64
			if action != "not_found" {
				if action == "already_forgotten" {
					seed.Action = "forget"
					seed.Content = ""
				}
				r, err := s.Mutate(ctx, scope, seed)
				if err != nil {
					t.Fatal(err)
				}
				revision = r.Revision
			}
			job := stage1AcceptedJob(t, s, scope, "run", "input", "我在北京")
			// Claim first, then accept the explicit action to exercise the apply boundary.
			claimed, err := s.ClaimJob(ctx)
			if err != nil || claimed.ID != job.ID {
				t.Fatalf("claim: %+v %v", claimed, err)
			}
			if err = stage3SaveFixture(t, s, claimed, stage1City(t)); err != nil {
				t.Fatal(err)
			}
			explicit := change("explicit", "profile.name", "用户昵称是小鹿。", revision)
			if action != "unchanged" {
				explicit.Action = "forget"
				explicit.Content = ""
			}
			explicit.RequireSnapshot = true
			explicit.Source.RunID, explicit.Source.InputID, explicit.Source.EventSequence = job.RunID, job.InputID, job.Sequence
			r, err := s.Mutate(ctx, scope, explicit)
			if err != nil || r.Status != action {
				t.Fatalf("action: %+v %v", r, err)
			}
			if err = stage3ApplyFixture(t, s, ctx, claimed, stage1City(t)); err != nil {
				t.Fatal(err)
			}
			state, err := s.KeyState(ctx, scope, "city")
			if err != nil || state.Exists {
				t.Fatalf("explicit action reinterpreted: %+v %v", state, err)
			}
			// The same marker also suppresses a queued job before provider dispatch.
			if _, err = s.db.Exec("UPDATE memory_jobs SET status='queued' WHERE id=?", job.ID); err != nil {
				t.Fatal(err)
			}
			if _, err = s.ClaimJob(ctx); !errors.Is(err, harness.ErrNotFound) {
				t.Fatalf("explicit action was claimed: %v", err)
			}
		})
	}
}

func TestMemoryStage1LegacyMigrationPreservesReceiptsAndDiscardsJobs(t *testing.T) {
	ctx := t.Context()
	s, scope := personalStore(t)
	in := change("legacy-operation", CanonicalKey("city"), "北京", 0)
	raw, _ := json.Marshal(in)
	old := Receipt{OperationID: in.OperationID, Status: "saved", Key: CanonicalKey("city"), Scope: GlobalScope, FactID: "legacy-fact", Revision: 1, UpdatedAt: time.Unix(10, 0).UTC()}
	receipt, _ := json.Marshal(old)
	if _, err := s.db.Exec("INSERT INTO memory_operations(operation_id,scope_key,request_sha,receipt,created_at) VALUES(?,?,?,?,?)", in.OperationID, scope.key, fmt.Sprintf("%x", sha256.Sum256(raw)), receipt, "old"); err != nil {
		t.Fatal(err)
	}
	// Upgrade from a real pre-source layout without re-encoding old rows.
	stage2DropWriteSchema(t, s)
	for _, table := range []string{"memory_operation_sources", "memory_inputs", "memory_scope_barriers", "memory_job_dispositions", "memory_schema_migrations"} {
		if _, err := s.db.Exec("DROP TABLE " + table); err != nil {
			t.Fatal(err)
		}
	}
	for _, status := range []string{"queued", "running", "completed"} {
		if _, err := s.db.Exec(`INSERT INTO memory_jobs(id,run_id,input_id,attempt_id,payload,result,status,attempts,available_at,updated_at) VALUES(?,?,?,?,?,'{"facts":[]}',?,2,'old','old')`, status, "run-"+status, "input-"+status, "attempt", []byte(`{"run_id":"legacy","text":"synthetic"}`), status); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		var err error
		s, err = New(ctx, s.db)
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Mutate(ctx, scope, in)
	if err != nil || got != old {
		t.Fatalf("legacy replay changed: %+v %v", got, err)
	}
	var persisted []byte
	if err = s.db.QueryRow("SELECT receipt FROM memory_operations WHERE operation_id=?", in.OperationID).Scan(&persisted); err != nil || string(persisted) != string(receipt) {
		t.Fatal("legacy receipt rewritten")
	}
	if err = s.RecoverJobs(ctx); err != nil {
		t.Fatal(err)
	}
	status, err := s.JobStatus(ctx)
	if err != nil || status["discarded"] != 2 || status["completed"] != 1 {
		t.Fatalf("legacy dispositions: %+v %v", status, err)
	}
	var unknown int
	if err = s.db.QueryRow("SELECT COUNT(*) FROM memory_job_dispositions WHERE usage_state='legacy_usage_unavailable'").Scan(&unknown); err != nil || unknown != 2 {
		t.Fatalf("legacy usage invented: %d %v", unknown, err)
	}
	if err = s.Enqueue(ctx, Job{RunID: "legacy", InputID: "legacy", AttemptID: "attempt"}); !errors.Is(err, ErrSourceSnapshotMissing) {
		t.Fatalf("legacy source rebased: %v", err)
	}
}

func TestMemoryStage1TwoIDDeletesShareOneSourceAndAtomicMarker(t *testing.T) {
	ctx := t.Context()
	s, scope := personalStore(t)
	var facts []Fact
	for _, name := range []string{"one", "two"} {
		f, err := s.Create(ctx, scope, Candidate{Content: name, Category: "profile", Confidence: 1, Source: Source{ID: name, Kind: "operator"}})
		if err != nil {
			t.Fatal(err)
		}
		facts = append(facts, f)
	}
	job := stage1AcceptedJob(t, s, scope, "run", "input", "请忘记这两条资料")
	in := Mutation{SchemaVersion: 2, RequireSnapshot: true, Action: "forget", ExpectedRevision: 1, FactID: facts[0].ID,
		Source: Source{ID: "source", Kind: "operator", RunID: job.RunID, InputID: job.InputID, EventSequence: job.Sequence}}
	in.OperationID = MutationOperationID(scope, in)
	if _, err := s.db.Exec(`CREATE TRIGGER fail_marker BEFORE INSERT ON memory_operation_sources BEGIN SELECT RAISE(ABORT,'marker_failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Mutate(ctx, scope, in); err == nil || !strings.Contains(err.Error(), "marker_failure") {
		t.Fatalf("wrong injected failure: %v", err)
	}
	if _, err := s.Get(ctx, scope, facts[0].ID); err != nil {
		t.Fatalf("fact escaped marker rollback: %v", err)
	}
	if _, err := s.Receipt(ctx, scope, in.OperationID); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("receipt escaped marker rollback: %v", err)
	}
	if _, err := s.db.Exec("DROP TRIGGER fail_marker"); err != nil {
		t.Fatal(err)
	}
	first := in
	for _, f := range facts {
		in.FactID = f.ID
		in.OperationID = MutationOperationID(scope, in)
		receipt, err := s.Mutate(ctx, scope, in)
		if err != nil || receipt.Status != "forgotten" {
			t.Fatalf("multi-target revocation: %+v %v", receipt, err)
		}
	}
	replay, err := s.Mutate(ctx, scope, first)
	if err != nil || replay.Status != "forgotten" {
		t.Fatalf("replay after own deletion epoch changed: %+v %v", replay, err)
	}
	if err = stage3ApplyFixture(t, s, ctx, job, `{"facts":[]}`); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryStage1SourceRejectsForgedEventAndInput(t *testing.T) {
	ctx := t.Context()
	s, scope := personalStore(t)
	job := stage1AcceptedJob(t, s, scope, "run", "input", "我在北京")
	in := change("forged", "city", "北京", 0)
	in.RequireSnapshot = true
	in.Source.RunID, in.Source.InputID, in.Source.EventSequence = job.RunID, job.InputID, job.Sequence+1
	if _, err := s.Mutate(ctx, scope, in); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatalf("forged event accepted: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE harness_inputs SET content='[{"type":"text","text":"changed"}]' WHERE id=?`, job.InputID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InputSnapshot(ctx, job.RunID, job.InputID); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatalf("input digest ignored: %v", err)
	}
}

func TestMemoryStage1SchemaMigrationRollsBackAndRejectsUnknownVersion(t *testing.T) {
	ctx := t.Context()
	s, _ := personalStore(t)
	stage2DropWriteSchema(t, s)
	for _, table := range []string{"memory_operation_sources", "memory_inputs", "memory_scope_barriers", "memory_job_dispositions"} {
		if _, err := s.db.Exec("DROP TABLE " + table); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`DELETE FROM memory_schema_migrations;
 CREATE TRIGGER fail_migration BEFORE INSERT ON memory_schema_migrations BEGIN SELECT RAISE(ABORT,'migration_failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := New(ctx, s.db); err == nil || !strings.Contains(err.Error(), "migration_failure") {
		t.Fatalf("migration failure lost: %v", err)
	}
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name='memory_inputs'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial schema survived: %d %v", count, err)
	}
	if _, err := s.db.Exec("DROP TRIGGER fail_migration"); err != nil {
		t.Fatal(err)
	}
	if _, err := New(ctx, s.db); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE memory_schema_migrations SET version=6 WHERE version=2"); err != nil {
		t.Fatal(err)
	}
	if _, err := New(ctx, s.db); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatalf("newer schema opened by older implementation: %v", err)
	}
}

func stage2DropWriteSchema(t *testing.T, s *Store) {
	t.Helper()
	for _, statement := range []string{
		"DROP TABLE memory_value_identities",
		"DROP TABLE memory_index_failures", "DROP TABLE memory_index_backoff",
		"DROP TABLE memory_job_leases", "DELETE FROM memory_schema_migrations WHERE version>=3",
		"DROP TABLE memory_extraction_items", "DROP TABLE memory_extraction_results", "DROP TABLE memory_daily_attempts",
		"ALTER TABLE memory_fact_revisions DROP COLUMN provenance", "ALTER TABLE memory_staged_facts DROP COLUMN provenance",
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}
