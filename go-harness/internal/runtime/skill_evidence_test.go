package runtime

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

func evidenceSkill(t *testing.T, db *sql.DB, workspace string) skills.ReadObservation {
	t.Helper()
	root := filepath.Join(workspace, "skills")
	if err := os.MkdirAll(filepath.Join(root, "sample"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sample", "SKILL.md"), []byte("---\nname: sample\ndescription: Fixture\n---\nPRIVATE_BODY"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := skills.NewRegistry(context.Background(), harness.SkillsConfig{Sources: []harness.SkillSource{{ID: "project", Root: root, Scope: harness.SkillScopeWorkspace, Workspace: workspace}}}, db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	if _, err = r.Install(context.Background(), "project", "sample"); err != nil {
		t.Fatal(err)
	}
	if err = r.SetEnabled(context.Background(), "project", "sample", true); err != nil {
		t.Fatal(err)
	}
	snapshot, err := r.Snapshot(context.Background(), harness.SkillSelection{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	var result skills.ReadObservation
	if _, err = snapshot.WithReadObserver(func(_ context.Context, read skills.ReadObservation) error { result = read; return nil }).Get(context.Background(), "sample"); err != nil {
		t.Fatal(err)
	}
	return result
}

func evidenceCount(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var count int
	if table != "harness_skill_reads" && table != "harness_skill_outcomes" {
		t.Fatal("invalid fixture table")
	}
	if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestSkillReadEvidenceFencesIdentityRoleAndReplay(t *testing.T) {
	f, attempt, role := newRoleContextFixture(t)
	read := evidenceSkill(t, f.s.db, f.req.Session.CWD)
	e := interaction.SkillRead{SessionID: f.req.Session.ID, RunID: f.req.RunID, Read: read}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := attempt.RecordSkillRead(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	e.Role = &role.Invocation
	if err := attempt.RecordSkillRead(ctx, e); err != nil {
		t.Fatal(err)
	}
	if got := evidenceCount(t, f.s.db, "harness_skill_reads"); got != 2 {
		t.Fatalf("replay or role attribution lost: %d", got)
	}
	for _, change := range []func(*interaction.SkillRead){
		func(e *interaction.SkillRead) { e.SessionID = "foreign" },
		func(e *interaction.SkillRead) { e.RunID = "foreign" },
		func(e *interaction.SkillRead) { e.Read.File.Hash = strings.Repeat("0", 64) },
		func(e *interaction.SkillRead) { e.Read.Ref.Version = strings.Repeat("1", 64) },
		func(e *interaction.SkillRead) { e.Read.Kind = "listed" },
		func(e *interaction.SkillRead) { r := *e.Role; r.RootID = "foreign"; e.Role = &r },
		func(e *interaction.SkillRead) { r := *e.Role; r.Spec.Model = "changed"; e.Role = &r },
	} {
		bad := e
		change(&bad)
		if err := attempt.RecordSkillRead(ctx, bad); err == nil {
			t.Fatal("changed evidence was recorded")
		}
	}
	if _, err := attempt.service.finishExecutionAttempt(ctx, f.lease, harness.RunResult{StopReason: "end_turn"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := attempt.RecordSkillRead(ctx, e); !errors.Is(err, harness.ErrExecutionConflict) {
		t.Fatalf("closed attempt wrote evidence: %v", err)
	}
	if got := evidenceCount(t, f.s.db, "harness_skill_reads"); got != 2 {
		t.Fatalf("invalid reads left records: %d", got)
	}
}

func TestSkillOutcomeOutboxCommitsWithForegroundTerminal(t *testing.T) {
	f := newExecutionFixture(t)
	ctx := context.Background()
	finish := func(tx *sql.Tx) error {
		if _, err := f.s.EndExecutionAttemptTx(ctx, tx, f.lease, harness.ExecutionStopCompleted, ""); err != nil {
			return err
		}
		return f.ledger.EndAttemptTx(ctx, tx, f.lease.Scope, budget.OutcomeCompleted)
	}
	if _, err := f.s.db.Exec(`CREATE TRIGGER evidence_fault BEFORE INSERT ON harness_skill_outcomes BEGIN SELECT RAISE(ABORT,'fixture outbox fault'); END`); err != nil {
		t.Fatal(err)
	}
	if err := executionTx(ctx, f.s, finish); err == nil {
		t.Fatal("terminal write succeeded without outbox")
	}
	state, err := f.s.Execution(ctx, f.req.Session.ID, f.req.RunID)
	if err != nil || state.Status != harness.ExecutionRunning || evidenceCount(t, f.s.db, "harness_skill_outcomes") != 0 {
		t.Fatalf("partial terminal write: %+v %v", state, err)
	}
	if _, err = f.s.db.Exec(`DROP TRIGGER evidence_fault`); err != nil {
		t.Fatal(err)
	}
	mustExecutionTx(t, f, finish)
	if _, err = f.s.db.Exec(`UPDATE harness_runs SET status=status WHERE id=?`, f.req.RunID); err != nil {
		t.Fatal(err)
	}
	var status string
	var uncertain, processed int
	if err = f.s.db.QueryRow(`SELECT status,effects_uncertain,processed FROM harness_skill_outcomes WHERE run_id=?`, f.req.RunID).Scan(&status, &uncertain, &processed); err != nil || status != "completed" || uncertain != 0 || processed != 0 || evidenceCount(t, f.s.db, "harness_skill_outcomes") != 1 {
		t.Fatalf("outbox=%s/%d/%d error=%v", status, uncertain, processed, err)
	}
}

func TestSkillOutcomeWaitReconciliationAndCancellationStayDistinct(t *testing.T) {
	f := newExecutionFixture(t)
	f.pending(t)
	state := f.suspend(t)
	if got := evidenceCount(t, f.s.db, "harness_skill_outcomes"); got != 0 {
		t.Fatal("waiting checkpoint counted as a task outcome")
	}
	mustExecutionTx(t, f, func(tx *sql.Tx) error {
		_, err := f.s.CancelExecutionTx(context.Background(), tx, f.req.Session.ID, "owner", harness.CancelExecutionRequest{RunID: f.req.RunID, ExpectedVersion: state.Version})
		return err
	})
	var status string
	if err := f.s.db.QueryRow(`SELECT status FROM harness_skill_outcomes WHERE run_id=?`, f.req.RunID).Scan(&status); err != nil || status != "cancelled" {
		t.Fatalf("cancel observation=%s %v", status, err)
	}
	other := newExecutionFixture(t)
	mustExecutionTx(t, other, func(tx *sql.Tx) error {
		_, err := other.s.EndExecutionAttemptTx(context.Background(), tx, other.lease, harness.ExecutionStopFailed, "provider error with private details")
		return err
	})
	if got := evidenceCount(t, other.s.db, "harness_skill_outcomes"); got != 0 {
		t.Fatal("reconciliation state counted as a task outcome")
	}
}

func TestSkillReadEvidenceAndOutcomeForLegacyBackground(t *testing.T) {
	f := newBackgroundPermissionFixture(t, func(host *backgroundHostFixture) {
		host.spec.AgentVersion = "deerflow-child-v1"
		host.binding.AgentVersion = host.spec.AgentVersion
		var err error
		host.binding.ExecutionContract, err = host.spec.Contract()
		if err != nil {
			t.Fatal(err)
		}
		host.scope.Binding = host.binding
	})
	read := evidenceSkill(t, f.store.db, f.binding.Workspace)
	e := interaction.SkillRead{SessionID: f.binding.ChildSessionID, RunID: f.binding.TaskID, Read: read}
	ctx := context.Background()
	if err := f.attempt.RecordSkillRead(ctx, e); err != nil {
		t.Fatal(err)
	}
	task, err := f.tasks.Get(ctx, f.binding.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.tasks.Complete(ctx, &bt.CompleteTaskRequest{TaskID: f.binding.TaskID, ExpectedVersion: task.Version}); err != nil {
		t.Fatal(err)
	}
	if err := f.attempt.RecordSkillRead(ctx, e); err == nil {
		t.Fatal("ended background attempt recorded a read")
	}
	var root, run, status string
	if err := f.store.db.QueryRow(`SELECT r.root_id,r.run_id,o.status FROM harness_skill_reads r JOIN harness_skill_outcomes o ON o.run_id=r.run_id`).Scan(&root, &run, &status); err != nil || root != f.binding.RootBudgetID || run != f.binding.TaskID || status != "completed" {
		t.Fatalf("background evidence=%s/%s/%s %v", root, run, status, err)
	}
}

func TestSkillOutcomeBackgroundFailureTransactionPreservesUncertainty(t *testing.T) {
	f := newBackgroundPermissionFixture(t)
	ctx := context.Background()
	f.receipt(t, "external", "write_file", true)
	task, err := f.tasks.Get(ctx, f.binding.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	request := &bt.FailTaskRequest{TaskID: task.Spec.ID, ExpectedVersion: task.Version, Error: "PRIVATE_PROVIDER_FAILURE"}
	if _, err = f.store.db.Exec(`CREATE TRIGGER evidence_fault BEFORE INSERT ON harness_skill_outcomes BEGIN SELECT RAISE(ABORT,'fixture outbox fault'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = f.tasks.Fail(ctx, request); err == nil {
		t.Fatal("native failure committed without its outbox")
	}
	task, err = f.tasks.Get(ctx, f.binding.TaskID)
	if err != nil || task.Status != bt.StatusRunning || evidenceCount(t, f.store.db, "harness_skill_outcomes") != 0 {
		t.Fatalf("native task partially committed: %+v %v", task, err)
	}
	receipt, err := readReceipt(ctx, f.store.db, f.binding.ChildSessionID, f.binding.TaskID, "external")
	if err != nil || receipt.State != harness.ReceiptStarted {
		t.Fatalf("receipt partially committed: %+v %v", receipt, err)
	}
	if _, err = f.store.db.Exec(`DROP TRIGGER evidence_fault`); err != nil {
		t.Fatal(err)
	}
	if _, err = f.tasks.Fail(ctx, request); err != nil {
		t.Fatal(err)
	}
	var status string
	var uncertain int
	if err = f.store.db.QueryRow(`SELECT status,effects_uncertain FROM harness_skill_outcomes WHERE run_id=?`, f.binding.TaskID).Scan(&status, &uncertain); err != nil || status != "failed" || uncertain != 1 {
		t.Fatalf("uncertain failure lost: %s/%d %v", status, uncertain, err)
	}
	if evidenceCount(t, f.store.db, "harness_skill_reads") != 0 {
		t.Fatal("task failure invented a skill read")
	}
}
