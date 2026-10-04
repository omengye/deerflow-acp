package evolution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

type validatorFunc func(context.Context, ValidationInput) (Report, error)

func (f validatorFunc) Validate(ctx context.Context, input ValidationInput) (Report, error) {
	return f(ctx, input)
}
func validReport(input ValidationInput) Report {
	return Report{ValidatorVersion: "fixture-v1", BaseVersion: input.Proposal.Base.Version, CandidateVersion: input.Proposal.CandidateVersion, Security: "passed", Behavior: "passed", Evidence: []string{"isolated fixture passed"}}
}
func skillFiles(body string) map[string][]byte {
	return map[string][]byte{"SKILL.md": []byte("---\nname: research\ndescription: Research a topic\n---\n" + body + "\n")}
}

type fixture struct {
	service  *Service
	registry *skills.Registry
	db       *sqlite.Store
	cfg      harness.SkillsConfig
	calls    atomic.Int32
}

func newFixture(t *testing.T, validator Validator) *fixture {
	t.Helper()
	ws := t.TempDir()
	root := filepath.Join(ws, "skills")
	if err := os.MkdirAll(filepath.Join(root, "research"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "research", "SKILL.md"), skillFiles("Original instructions")["SKILL.md"], 0600); err != nil {
		t.Fatal(err)
	}
	f := &fixture{cfg: harness.SkillsConfig{Sources: []harness.SkillSource{{ID: "project", Root: root, Scope: harness.SkillScopeWorkspace, Workspace: ws}}}}
	var err error
	f.db, err = sqlite.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	f.registry, err = skills.NewRegistry(context.Background(), f.cfg, f.db.DB())
	if err != nil {
		t.Fatal(err)
	}
	if err = f.registry.InstallConfigured(context.Background(), "project", "research", true); err != nil {
		t.Fatal(err)
	}
	if validator == nil {
		validator = validatorFunc(func(_ context.Context, input ValidationInput) (Report, error) {
			f.calls.Add(1)
			return validReport(input), nil
		})
	}
	f.service, err = New(context.Background(), f.registry, validator)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.registry.Close(); _ = f.db.Close() })
	return f
}
func (f *fixture) head(t *testing.T) skills.SkillHead {
	t.Helper()
	var head skills.SkillHead
	err := f.registry.WithPublication(context.Background(), "project", "research", func(tx *skills.PublicationTx) error { var err error; head, err = tx.Current(); return err })
	if err != nil {
		t.Fatal(err)
	}
	return head
}
func (f *fixture) request(t *testing.T, key, body string) Request {
	return Request{SourceID: "project", Name: "research", RequestKey: key, Action: "edit", Reason: "Improve the steps", Base: f.head(t), Files: skillFiles(body)}
}
func (f *fixture) submit(t *testing.T, r Request) Proposal {
	t.Helper()
	p, err := f.service.Submit(context.Background(), r)
	if err != nil || p.Status != "pending_review" {
		t.Fatalf("submit: %+v %v", p, err)
	}
	return p
}
func review(p Proposal) Review {
	return Review{ExpectedStateVersion: p.StateVersion, Actor: "desktop", Note: "Reviewed diff and evaluation"}
}

func TestProposalPublishRollbackRestartAndDuplicateRequests(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	base := f.head(t)
	selection := harness.SkillSelection{Workspace: f.cfg.Sources[0].Workspace}
	pinned, err := f.registry.Snapshot(ctx, selection)
	if err != nil {
		t.Fatal(err)
	}
	r := f.request(t, "edit-one", "Improved instructions")
	p := f.submit(t, r)
	if f.head(t) != base || !strings.Contains(p.Diff, "+Improved instructions") {
		t.Fatalf("proposal changed live skill or lost diff: %+v", p)
	}
	published, err := f.service.Approve(ctx, p.ID, review(p))
	if err != nil || published.Status != "published" || published.PublishedRevision != 2 {
		t.Fatalf("publish: %+v %v", published, err)
	}
	head := f.head(t)
	if head.Version != p.CandidateVersion || !head.Managed {
		t.Fatalf("publication head: %+v", head)
	}
	for range 2 {
		again, err := f.service.Submit(ctx, r)
		if err != nil || again.ID != p.ID || again.Status != "published" {
			t.Fatalf("submit replay: %+v %v", again, err)
		}
		again, err = f.service.Approve(ctx, p.ID, review(p))
		if err != nil || again.PublishedRevision != 2 {
			t.Fatalf("approve replay: %+v %v", again, err)
		}
	}
	if f.calls.Load() != 1 {
		t.Fatalf("duplicate submission reran validation: %d", f.calls.Load())
	}
	rollbackRequest := Rollback{SourceID: "project", Name: "research", RequestKey: "undo-one", Revision: 1, Expected: head, Actor: "desktop"}
	rolled, err := f.service.Rollback(ctx, rollbackRequest)
	if err != nil || rolled.Revision != 3 || rolled.ContentVersion != base.Version {
		t.Fatalf("first edit cannot roll back to imported base: %+v %v", rolled, err)
	}
	if data, err := pinned.ReadFile(ctx, "research", "SKILL.md"); err != nil || string(data) != string(skillFiles("Original instructions")["SKILL.md"]) {
		t.Fatalf("old pinned snapshot changed: %q %v", data, err)
	}
	if err = f.registry.Close(); err != nil {
		t.Fatal(err)
	}
	f.registry, err = skills.NewRegistry(ctx, f.cfg, f.db.DB())
	if err != nil {
		t.Fatal(err)
	}
	if err = f.registry.InstallConfigured(ctx, "project", "research", false); err != nil {
		t.Fatal(err)
	}
	f.service, err = New(ctx, f.registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.head(t) != rolled.Head {
		t.Fatal("restart reapplied old startup flags")
	}
	if again, err := f.service.Rollback(ctx, rollbackRequest); err != nil || again != rolled {
		t.Fatalf("rollback replay changed revision: %+v %v", again, err)
	}
	history, err := f.service.History(ctx, "project", "research", 0, 100)
	if err != nil || len(history) != 3 || history[0].Action != "baseline" {
		t.Fatalf("history: %+v %v", history, err)
	}
	r.Files = skillFiles("Changed request under same key")
	if _, err = f.service.Submit(ctx, r); !errors.Is(err, ErrConflict) {
		t.Fatalf("request key reused with different candidate: %v", err)
	}
}

func TestProposalConcurrentApprovalPersistsStaleLoser(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	a := f.submit(t, f.request(t, "a", "Candidate A"))
	b := f.submit(t, f.request(t, "b", "Candidate B"))
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, p := range []Proposal{a, b} {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := f.service.Approve(ctx, p.ID, review(p)); results <- err }()
	}
	wg.Wait()
	close(results)
	wins, conflicts := 0, 0
	for err := range results {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("approval race wins=%d conflicts=%d", wins, conflicts)
	}
	list, err := f.service.List(ctx, "project", "research", "", "stale", 100)
	if err != nil || len(list) != 1 || list[0].StateVersion != 3 {
		t.Fatalf("stale result did not commit: %+v %v", list, err)
	}
	history, err := f.service.History(ctx, "project", "research", 0, 100)
	if err != nil || len(history) != 2 {
		t.Fatalf("conflicting release published: %+v %v", history, err)
	}
}

func TestProposalPublishAuditFailureIsAtomic(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	before := f.head(t)
	p := f.submit(t, f.request(t, "fault", "Candidate"))
	if _, err := f.db.DB().Exec(`CREATE TRIGGER fail_release BEFORE INSERT ON df_skill_publications WHEN NEW.request_key LIKE 'approve/%' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Approve(ctx, p.ID, review(p)); err == nil {
		t.Fatal("audit failure published candidate")
	}
	saved, err := f.service.Get(ctx, p.ID)
	if err != nil || saved.Status != "pending_review" || saved.StateVersion != p.StateVersion || f.head(t) != before {
		t.Fatalf("partial publication: %+v %v", saved, err)
	}
	history, err := f.service.History(ctx, "project", "research", 0, 100)
	if err != nil || len(history) != 0 {
		t.Fatalf("partial baseline: %+v %v", history, err)
	}
	if _, err = f.db.DB().Exec(`DROP TRIGGER fail_release`); err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.Approve(ctx, p.ID, review(p)); err != nil {
		t.Fatal(err)
	}
}

func TestProposalSecurityValidationAndSourceIsolation(t *testing.T) {
	for _, fault := range []string{"unavailable", "wrong_digest", "failed_behavior", "script_without_isolation"} {
		t.Run(fault, func(t *testing.T) {
			f := newFixture(t, validatorFunc(func(_ context.Context, input ValidationInput) (Report, error) {
				report := validReport(input)
				switch fault {
				case "unavailable":
					return Report{}, errors.New("credential must never be persisted")
				case "wrong_digest":
					report.CandidateVersion = "wrong"
				case "failed_behavior":
					report.Behavior = "failed"
				}
				return report, nil
			}))
			r := f.request(t, "blocked", "Candidate")
			if fault == "script_without_isolation" {
				r.Files["scripts/run.sh"] = []byte("echo sample\n")
			}
			p, err := f.service.Submit(context.Background(), r)
			if err != nil || p.Status != "failed" || strings.Contains(p.Error, "credential") {
				t.Fatalf("validation did not fail closed: %+v %v", p, err)
			}
			if _, err = f.service.Approve(context.Background(), p.ID, review(p)); !errors.Is(err, ErrConflict) {
				t.Fatalf("failed candidate approved: %v", err)
			}
			if f.head(t) != r.Base {
				t.Fatal("failed validation changed live skill")
			}
		})
	}
	f := newFixture(t, nil)
	p := f.submit(t, f.request(t, "scoped", "Candidate"))
	cfg := f.cfg
	cfg.Sources = append([]harness.SkillSource(nil), cfg.Sources...)
	ws := t.TempDir()
	cfg.Sources[0].Workspace = ws
	cfg.Sources[0].Root = ws
	r, err := skills.NewRegistry(context.Background(), cfg, f.db.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s, err := New(context.Background(), r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Get(context.Background(), p.ID); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatalf("source ID reuse acquired old proposal: %v", err)
	}
}

func TestProposalRejectDuringValidationAndDeleteRollback(t *testing.T) {
	started, release := make(chan Proposal, 1), make(chan struct{})
	f := newFixture(t, validatorFunc(func(_ context.Context, input ValidationInput) (Report, error) {
		started <- input.Proposal
		<-release
		return validReport(input), nil
	}))
	ctx := context.Background()
	r := f.request(t, "reject", "Candidate")
	done := make(chan error, 1)
	go func() { _, err := f.service.Submit(ctx, r); done <- err }()
	p := <-started
	if _, err := f.service.Reject(ctx, p.ID, review(p)); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; !errors.Is(err, ErrConflict) {
		t.Fatalf("validator overwrote rejection: %v", err)
	}
	if again, err := f.service.Reject(ctx, p.ID, review(p)); err != nil || again.Status != "rejected" {
		t.Fatalf("reject replay: %+v %v", again, err)
	}
	var err error
	f.service, err = New(ctx, f.registry, validatorFunc(func(_ context.Context, input ValidationInput) (Report, error) { return validReport(input), nil }))
	if err != nil {
		t.Fatal(err)
	}
	del := r
	del.RequestKey = "delete"
	del.Action = "delete"
	del.Files = nil
	p = f.submit(t, del)
	if _, err = f.service.Approve(ctx, p.ID, review(p)); err != nil {
		t.Fatal(err)
	}
	head := f.head(t)
	if head.Exists {
		t.Fatal("delete proposal kept live install")
	}
	if err = f.registry.InstallConfigured(ctx, "project", "research", true); err != nil || f.head(t) != head {
		t.Fatalf("startup undid deletion: %v", err)
	}
	rolled, err := f.service.Rollback(ctx, Rollback{SourceID: "project", Name: "research", RequestKey: "restore", Revision: 1, Expected: head, Actor: "desktop"})
	if err != nil || !rolled.Head.Exists {
		t.Fatalf("deleted skill could not be restored: %+v %v", rolled, err)
	}
}

func TestProposalRejectsPythonAndBrokenReferences(t *testing.T) {
	f := newFixture(t, nil)
	for _, body := range []string{"Run `python3 scripts/run.py`.", "See [guide](missing.md).", "See [escape](../secret.md).", "See [escape](%2e%2e/secret.md)."} {
		if _, err := f.service.Submit(context.Background(), f.request(t, body, body)); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatalf("accepted invalid candidate %q: %v", body, err)
		}
	}
	r := f.request(t, "python_file", "Candidate")
	r.Files["tool.PY"] = []byte("print('fixture')")
	if _, err := f.service.Submit(context.Background(), r); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatalf("accepted Python support file: %v", err)
	}
	r = f.request(t, "reference", "See [guide](references/guide.md).")
	r.Files["references/guide.md"] = []byte("Read [main](../SKILL.md).\n")
	f.submit(t, r)
}
