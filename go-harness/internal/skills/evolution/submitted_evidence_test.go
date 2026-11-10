package evolution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/learning"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillbehavior"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

type submittedNativeEvaluator struct{ *fixtureBehaviorEvaluator }

func (e submittedNativeEvaluator) Evaluate(ctx context.Context, in harness.SkillBehaviorInput) (harness.SkillBehaviorResult, error) {
	out, err := e.fixtureBehaviorEvaluator.Evaluate(ctx, in)
	if err != nil || !skillbehavior.TriggerCase(in.Case) {
		return out, err
	}
	_, digest, err := skillbehavior.SelectionCatalog(in)
	if err != nil {
		return out, err
	}
	selected := ""
	if in.Case.Kind == "trigger_positive" {
		selected = in.SkillName
	}
	selection := harness.SkillBehaviorSelection{RuleVersion: skillbehavior.SelectionRuleVersion, TargetName: in.SkillName, SelectedName: selected, CatalogDigest: digest}
	check := skillbehavior.SelectionCheck(in.Case.Assertions[0], selection)
	out.Selection, out.Checks, out.Passed, out.OutputDigest = &selection, []harness.SkillBehaviorCheck{check}, check.Passed, check.ActualDigest
	return out, nil
}

type submittedNativeFixture struct {
	*collectorFixture
	session *harness.Session
	g       *fixtureGenerator
	v       *fixtureGenerationValidator
	e       *fixtureBehaviorEvaluator
}

func newSubmittedNativeFixture(t *testing.T) *submittedNativeFixture {
	t.Helper()
	f, g, v, e := submittedFixture(t, false)
	n := &submittedNativeFixture{collectorFixture: &collectorFixture{fixture: f}, g: g, v: v, e: e}
	ctx := context.Background()
	var err error
	n.runtime, err = hr.NewStore(ctx, f.db.DB())
	if err != nil {
		t.Fatal(err)
	}
	n.ledger, err = budget.New(f.db.DB(), budget.Config{})
	if err != nil {
		t.Fatal(err)
	}
	l, err := learning.New(ctx, f.db.DB(), &harness.SkillLearningOptions{Idle: time.Second, Policy: harness.SkillMaintenancePolicy{MaxJobSeconds: 60, Workers: 1}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	n.runtime.Learning, n.runtime.CaptureSkillOracle = l, f.service.CaptureTaskOracleTx
	f.service.behavior.evaluator = submittedNativeEvaluator{e}
	snapshot, err := f.registry.Snapshot(ctx, harness.SkillSelection{Workspace: f.cfg.Sources[0].Workspace})
	if err != nil {
		t.Fatal(err)
	}
	_, err = snapshot.WithReadObserver(func(_ context.Context, read skills.ReadObservation) error { n.read = read; return nil }).Get(ctx, "research")
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *submittedNativeFixture) start(t *testing.T, workspace, prompt string) harness.RunRequest {
	t.Helper()
	ctx := context.Background()
	var x harness.Session
	var err error
	if f.session != nil && f.session.CWD == workspace {
		x = *f.session
	} else {
		x, err = f.runtime.CreateSession(ctx, workspace, "fixture")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.runtime.Learning.SetControl(ctx, x.ID, harness.SkillLearningControlRequest{Contribute: true}); err != nil {
			t.Fatal(err)
		}
		if f.session == nil {
			f.session = &x
		}
	}
	id := hr.NewID()
	r := harness.RunRequest{Session: x, RunID: id, RootBudgetID: id, InputID: hr.NewID(), Input: []harness.Content{{Type: "text", Text: prompt}}}
	if err = f.runtime.BeginRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err = f.ledger.CreateRoot(ctx, budget.RootSpec{RootBudgetID: id, RootRunID: id, SessionID: x.ID}); err != nil {
		t.Fatal(err)
	}
	if err = f.ledger.BeginAttempt(ctx, budget.Scope{RootBudgetID: id, MemberID: id, SessionID: x.ID, AttemptID: id, Fence: 1}); err != nil {
		t.Fatal(err)
	}
	return r
}

func (f *submittedNativeFixture) finish(t *testing.T, r harness.RunRequest, useSkill bool) {
	t.Helper()
	ctx := context.Background()
	if useSkill {
		read := f.read
		// Digest-only read envelope at the native persistence boundary. The
		// collector rechecks its attempt, root, manifest and completed outcome.
		_, err := f.db.DB().Exec(`INSERT INTO harness_skill_reads(run_id,session_id,root_id,attempt_id,role_id,source_id,source_key,scope,name,version,path,file_hash,bytes,kind,observed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			r.RunID, r.Session.ID, r.RootBudgetID, r.RunID, "", read.Ref.SourceID, read.Ref.SourceIdentity, read.Scope, read.Ref.Name, read.Ref.Version, read.File.Path, read.File.Hash, read.File.Bytes, "skill_load", time.Now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.runtime.Append(ctx, harness.RunEvent{SessionID: r.Session.ID, RunID: r.RunID, Kind: "text_delta", AgentName: "deerflow", Text: "verified"}); err != nil {
		t.Fatal(err)
	}
	if err := f.ledger.EndAttempt(ctx, budget.Scope{RootBudgetID: r.RootBudgetID, MemberID: r.RunID, SessionID: r.Session.ID, AttemptID: r.RunID, Fence: 1}, budget.Outcome("completed")); err != nil {
		t.Fatal(err)
	}
	if err := f.runtime.Finish(ctx, r.RunID, "end_turn", nil); err != nil {
		t.Fatal(err)
	}
	f.drain(t, 32)
}

func (f *submittedNativeFixture) submit(t *testing.T) (Proposal, harness.RunRequest) {
	t.Helper()
	workspace := f.cfg.Sources[0].Workspace
	origin := f.start(t, workspace, "Improve research by verifying sources before summarizing.")
	r := f.request(t, "native-candidate", "Verify sources before summarizing")
	r.Trigger = harness.SkillTrigger{Type: "explicit_agent_proposal", ThreadID: origin.Session.ID, RunID: origin.RunID, Summary: r.Reason}
	p, err := f.service.SubmitAutomatic(context.Background(), r, workspace)
	if err != nil {
		t.Fatal(err)
	}
	f.finish(t, origin, false)
	return p, origin
}

func TestSubmittedNativeEvidenceResumesOriginalCandidateWithoutDiscovery(t *testing.T) {
	ctx := context.Background()
	f := newSubmittedNativeFixture(t)
	p, _ := f.submit(t)
	generationRun(t, f.service)
	// Persist the previous release's shape: a native trigger without a
	// submission record. This is the shape of the user's existing V2.
	legacySubmittedSnapshots(t, f.fixture, p.ID)
	workspace := f.cfg.Sources[0].Workspace
	for i := range 2 {
		r := f.start(t, workspace, fmt.Sprintf("Verify research task %d.\nExpected output:\n```text\nverified\n```", i))
		f.finish(t, r, true)
	}
	// Neither a different workspace nor an oracle with no actual skill use
	// can fill the missing independent holdout.
	for _, other := range []bool{true, false} {
		cwd := workspace
		if other {
			cwd = t.TempDir()
		}
		r := f.start(t, cwd, "Verify unrelated research task.\nExpected output:\n```text\nverified\n```")
		f.finish(t, r, other)
	}
	// Non-shared conversations cannot contribute evidence to this proposal.
	ownerSession := f.session
	f.session = nil
	private := f.start(t, workspace, "Verify private research task.\nExpected output:\n```text\nverified\n```")
	f.finish(t, private, true)
	f.session = ownerSession
	f.service.generation.dependencyProbe = time.Time{}
	if worked, err := f.service.RunGeneration(ctx); err != nil || worked {
		t.Fatal("insufficient evidence woke candidate", worked, err)
	}
	progress, err := f.service.CandidateProgress(ctx, p.ID)
	if err != nil || progress.Evidence == nil || !progress.Evidence.SourceTaskBound || progress.Evidence.SupportingCases != 2 || progress.Evidence.HoldoutCases != 0 || !slices.Contains(progress.Evidence.Missing, "independent_holdout_oracle") {
		t.Fatal("wrong evidence diagnostics", progress.Evidence, err)
	}
	if f.e.calls.Load() != 0 || f.v.calls.Load() != 1 {
		t.Fatal("probes consumed model calls")
	}
	holdout := f.start(t, workspace, "Verify a separate research task.\nExpected output:\n```text\nverified\n```")
	f.finish(t, holdout, true)
	// Restart, rather than forcing a retry or rebuilding the candidate.
	next, err := New(ctx, f.registry, f.v)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = next.Close() })
	configureGeneration(t, next, f.g)
	if err = next.ConfigureBehavior(submittedNativeEvaluator{f.e}, harness.SkillBehaviorOptions{}); err != nil {
		t.Fatal(err)
	}
	if err = next.ConfigureAutoPatch(harness.SkillAutoPatchOptions{MaxChangedLines: 40, ObservationHours: 72, MinObservations: 5, RollbackFailureThreshold: 2}); err != nil {
		t.Fatal(err)
	}
	f.service = next
	for range 8 {
		worked, err := f.service.RunGeneration(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			break
		}
	}
	if err = f.service.RunAutoPatch(ctx); err != nil {
		t.Fatal(err)
	}
	done, err := f.service.Get(ctx, p.ID)
	if err != nil || done.Status != "published" || done.CandidateVersion != p.CandidateVersion || f.g.calls.Load() != 0 || f.v.calls.Load() != 1 {
		t.Fatal("native evidence did not resume exact candidate", done.Status, done.Error, err, f.g.calls.Load(), f.v.calls.Load())
	}
	var discoveries int
	if err = f.db.DB().QueryRow(`SELECT count(*) FROM df_skill_discoveries`).Scan(&discoveries); err != nil || discoveries != 0 {
		t.Fatal("required discovery pipeline", discoveries, err)
	}
	view, err := f.service.BehaviorEvaluation(ctx, p.ID)
	if err != nil || view.Report == nil || view.Report.HoldoutCases != 1 || view.Report.TargetedCases != 2 {
		t.Fatal("missing real task bindings", view.Report, err)
	}
}

func legacySubmittedSnapshots(t *testing.T, f *fixture, id string) {
	t.Helper()
	ctx := context.Background()
	b, err := readBehavior(ctx, f.db.DB(), id)
	if err != nil || b.input.Submission == nil {
		t.Fatal("new submission did not pin provenance", err)
	}
	r, err := readGeneration(ctx, f.db.DB(), b.jobID)
	if err != nil || r.input.Submission == nil {
		t.Fatal("generation lost provenance", err)
	}
	r.input.Submission, b.input.Submission = nil, nil
	g, _ := json.Marshal(r.input)
	behavior, _ := json.Marshal(b.input)
	tx, err := f.db.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, item := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE df_skill_generation_inputs SET payload=? WHERE job_id=?`, []any{g, b.jobID}},
		{`UPDATE df_skill_maintenance_jobs SET input_digest=? WHERE id=?`, []any{hash(g), b.jobID}},
		{`UPDATE df_skill_behavior_evaluations SET payload=?,payload_sha=? WHERE proposal_id=?`, []any{behavior, hash(behavior), id}},
	} {
		if _, err = tx.ExecContext(ctx, item.sql, item.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestSubmittedNativeEvidenceRechecksConsentAndOrigin(t *testing.T) {
	ctx := context.Background()
	f := newSubmittedNativeFixture(t)
	p, origin := f.submit(t)
	var tasks []harness.RunRequest
	for i := range 3 {
		r := f.start(t, f.cfg.Sources[0].Workspace, fmt.Sprintf("Verify distinct research case %d.\nExpected output:\n```text\nverified\n```", i))
		f.finish(t, r, true)
		tasks = append(tasks, r)
	}
	b, err := readBehavior(ctx, f.db.DB(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.db.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.deriveBehaviorCasesTx(ctx, tx, p, &b.input); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if b.input.Suite == nil || b.input.Submission == nil {
		_ = tx.Rollback()
		t.Fatal("source not bound")
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	control, err := f.runtime.Learning.Control(ctx, tasks[2].Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.runtime.Learning.SetControl(ctx, tasks[2].Session.ID, harness.SkillLearningControlRequest{Contribute: false, ExpectedEpoch: control.Epoch}); err != nil {
		t.Fatal(err)
	}
	tx, err = f.db.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = f.service.validateDerivedSuiteTx(ctx, tx, p, b.input)
	_ = tx.Rollback()
	if !errors.Is(err, errDiscoveryEvidence) {
		t.Fatal("withdrawn source still accepted", err)
	}
	// A source session mismatch cannot be introduced through the host API.
	r := f.request(t, "forged-source", "Verify sources before summarizing")
	r.Trigger = harness.SkillTrigger{Type: "explicit_agent_proposal", ThreadID: hr.NewID(), RunID: origin.RunID}
	if _, err = f.service.SubmitAutomatic(ctx, r, f.cfg.Sources[0].Workspace); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("forged source accepted", err)
	}
}

func TestSubmittedLegacySourceBindingCannotBeRewrittenWhileWaiting(t *testing.T) {
	ctx := context.Background()
	f := newSubmittedNativeFixture(t)
	p, source := f.submit(t)
	generationRun(t, f.service)
	legacySubmittedSnapshots(t, f.fixture, p.ID)
	f.service.generation.dependencyProbe = time.Time{}
	if err := f.service.refreshWaiting(ctx); err != nil {
		t.Fatal(err)
	}
	bound, err := readBehavior(ctx, f.db.DB(), p.ID)
	if err != nil || bound.input.Submission == nil {
		t.Fatal("legacy source was not pinned", err)
	}
	if _, err = f.db.DB().Exec(`UPDATE harness_inputs SET content=? WHERE run_id=?`, []byte(`[{"type":"text","text":"Changed source task"}]`), source.RunID); err != nil {
		t.Fatal(err)
	}
	progress, err := f.service.CandidateProgress(ctx, p.ID)
	if err != nil || progress.Evidence == nil || progress.Evidence.SourceTaskBound || !slices.Contains(progress.Evidence.Missing, "source_task_binding") {
		t.Fatal("source change hidden", progress.Evidence, err)
	}
	f.service.generation.dependencyProbe = time.Time{}
	if worked, err := f.service.RunGeneration(ctx); err != nil || worked {
		t.Fatal("invalid source blocked worker or resumed candidate", worked, err)
	}
	after, err := readBehavior(ctx, f.db.DB(), p.ID)
	if err != nil || *after.input.Submission != *bound.input.Submission || f.v.calls.Load() != 1 {
		t.Fatal("rebound source or repeated paid review", err)
	}
}
