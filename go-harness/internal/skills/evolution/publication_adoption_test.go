package evolution

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

func createRequest(key string) Request {
	return Request{SourceID: "project", Name: "created", RequestKey: key, Action: "create", Reason: "Complete reusable workflow", Files: map[string][]byte{
		"SKILL.md":           []byte("---\nname: created\ndescription: A complete workflow\n---\nUse the reference.\n"),
		"assets/config.json": []byte("{\"endpoint\":\"https://example.invalid\"}\n"),
	}}
}

func writeCandidate(t *testing.T, f *fixture, request Request) {
	t.Helper()
	for name, data := range request.Files {
		file := filepath.Join(f.cfg.Sources[0].Root, request.Name, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func assertNotInstalled(t *testing.T, f *fixture) {
	t.Helper()
	_, head, _, err := f.service.Head(context.Background(), "project", "created")
	if err != nil || head.Exists {
		t.Fatalf("unapproved source installed: %+v %v", head, err)
	}
}

func TestCreatePublicationCreatesOrAdoptsExactPackage(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(map[bool]string{false: "create", true: "adopt"}[present], func(t *testing.T) {
			f, ctx := newFixture(t, nil), context.Background()
			r := createRequest("complete")
			var before os.FileInfo
			file := filepath.Join(f.cfg.Sources[0].Root, "created", "assets", "config.json")
			if present {
				writeCandidate(t, f, r)
				before, _ = os.Stat(file)
			}
			p := f.submit(t, r)
			assertNotInstalled(t, f)
			got, err := f.service.Approve(ctx, p.ID, review(p))
			if err != nil || got.Status != "published" || got.Approval == nil || got.Approval.CandidateVersion != p.CandidateVersion {
				t.Fatalf("publish: %+v %v", got, err)
			}
			if present {
				after, err := os.Stat(file)
				if err != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
					t.Fatal("adoption rewrote file", err)
				}
			}
			for range 2 {
				again, err := f.service.Approve(ctx, p.ID, review(p))
				if err != nil || again.PublishedRevision != got.PublishedRevision || *again.Approval != *got.Approval {
					t.Fatal("replay changed decision", err)
				}
			}
			snapshot, err := f.registry.Snapshot(ctx, harness.SkillSelection{Workspace: f.cfg.Sources[0].Workspace})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, ref := range snapshot.Refs() {
				if ref.Name == "created" && ref.Version == p.CandidateVersion {
					found = true
				}
			}
			if !found {
				t.Fatal("next execution did not see published skill")
			}
		})
	}
}

func TestCreatePreflightRejectsExtraFilesWithoutReviewCalls(t *testing.T) {
	f, ctx := newFixture(t, nil), context.Background()
	r := createRequest("different")
	writeCandidate(t, f, r)
	extra := filepath.Join(f.cfg.Sources[0].Root, "created", "extra.txt")
	if err := os.WriteFile(extra, []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := f.service.Submit(ctx, r)
	var conflict *skills.PublicationConflict
	if !errors.As(err, &conflict) || conflict.Code != "target_content_differs" || p.ID == "" || p.Status != "stale" || f.calls.Load() != 0 {
		t.Fatalf("preflight: %+v %v", p, err)
	}
	if err = f.registry.InstallConfigured(ctx, "project", "created", true); err != nil {
		t.Fatal(err)
	}
	assertNotInstalled(t, f)
	if data, _ := os.ReadFile(extra); string(data) != "keep me" {
		t.Fatal("preflight changed files")
	}
	fresh, err := f.service.ReviewSource(ctx, p.ID, p.StateVersion, "preflight-current")
	if err != nil || fresh.Status != "pending_review" || fresh.Approval != nil {
		t.Fatal("preflight conflict has no recovery path", err)
	}
}

func TestSourceReviewNeverSendsLocalCredentialsToValidator(t *testing.T) {
	f, ctx := newFixture(t, nil), context.Background()
	r := createRequest("credentials")
	p := f.submit(t, r)
	r.Files["assets/config.json"] = []byte(`{"api_key":"LOCAL_PRIVATE_CREDENTIAL_VALUE"}`)
	writeCandidate(t, f, r)
	blocked, err := f.service.Approve(ctx, p.ID, review(p))
	if !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	_, err = f.service.ReviewSource(ctx, p.ID, blocked.StateVersion, "private-current")
	var conflict *skills.PublicationConflict
	if !errors.As(err, &conflict) || conflict.Code != "source_credentials_require_local_handling" || f.calls.Load() != 1 {
		t.Fatal("credential sent for review", err)
	}
	data, _ := os.ReadFile(filepath.Join(f.cfg.Sources[0].Root, "created", "assets", "config.json"))
	if string(data) != string(r.Files["assets/config.json"]) {
		t.Fatal("credential file modified")
	}
}

func TestApprovalConflictRetainsDecisionAndReviewsNewSource(t *testing.T) {
	f, ctx := newFixture(t, nil), context.Background()
	r := createRequest("original")
	p := f.submit(t, r)
	r.Files["assets/config.json"] = []byte("{\"endpoint\":\"https://changed.invalid\"}\n")
	writeCandidate(t, f, r)
	got, err := f.service.Approve(ctx, p.ID, review(p))
	if !errors.Is(err, ErrConflict) || got.Status != "stale" || got.Approval == nil || got.PublicationError != "target_content_differs" {
		t.Fatalf("conflict: %+v %v", got, err)
	}
	if err = f.registry.InstallConfigured(ctx, "project", "created", true); err != nil {
		t.Fatal(err)
	}
	assertNotInstalled(t, f)
	for range 2 {
		again, err := f.service.Approve(ctx, p.ID, review(p))
		if !errors.Is(err, ErrConflict) || again.Approval == nil || *again.Approval != *got.Approval {
			t.Fatal("blocked replay lost approval", err)
		}
	}
	fresh, err := f.service.ReviewSource(ctx, p.ID, got.StateVersion, "source-one")
	if err != nil || fresh.ID == p.ID || fresh.CandidateVersion == p.CandidateVersion || fresh.Approval != nil || fresh.Status != "pending_review" {
		t.Fatalf("source review: %+v %v", fresh, err)
	}
	if _, err = f.service.Approve(ctx, fresh.ID, review(fresh)); err != nil {
		t.Fatal(err)
	}
	// A response replay remains tied to the original source snapshot, not new disk bytes.
	r.Files["SKILL.md"] = append(r.Files["SKILL.md"], []byte("Later external edit\n")...)
	writeCandidate(t, f, r)
	again, err := f.service.ReviewSource(ctx, p.ID, got.StateVersion, "source-one")
	if err != nil || again.ID != fresh.ID || f.calls.Load() != 2 {
		t.Fatal("source review replay regenerated", err)
	}
}

func TestApprovalIsDurableBeforePublicationAuditFailure(t *testing.T) {
	f, ctx := newFixture(t, nil), context.Background()
	p := f.submit(t, createRequest("audit"))
	if _, err := f.db.DB().Exec(`CREATE TRIGGER fail_publish BEFORE INSERT ON df_skill_publications BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	got, err := f.service.Approve(ctx, p.ID, review(p))
	if err == nil || got.Approval == nil || got.Status != "pending_review" {
		t.Fatalf("decision lost: %+v %v", got, err)
	}
	if _, err = os.Stat(filepath.Join(f.cfg.Sources[0].Root, "created")); !os.IsNotExist(err) {
		t.Fatal("files before journal commit", err)
	}
	if _, err = f.db.DB().Exec(`DROP TRIGGER fail_publish`); err != nil {
		t.Fatal(err)
	}
	again, err := f.service.Approve(ctx, p.ID, review(p))
	if err != nil || again.Status != "published" || *again.Approval != *got.Approval {
		t.Fatal("decision retry", err)
	}
}

func TestApprovalStorageFailureNeverPublishes(t *testing.T) {
	f, ctx := newFixture(t, nil), context.Background()
	p := f.submit(t, createRequest("approval-storage"))
	if _, err := f.db.DB().Exec(`CREATE TRIGGER fail_approval BEFORE INSERT ON df_skill_approvals BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Approve(ctx, p.ID, review(p)); err == nil {
		t.Fatal("approval storage failure ignored")
	}
	assertNotInstalled(t, f)
	if _, err := os.Stat(filepath.Join(f.cfg.Sources[0].Root, "created")); !os.IsNotExist(err) {
		t.Fatal("unapproved files written")
	}
}

func TestDurableApprovalResumesWithoutAnotherUserDecision(t *testing.T) {
	f, ctx := newFixture(t, nil), context.Background()
	p := f.submit(t, createRequest("recover-decision"))
	if err := f.service.recordApproval(ctx, p.ID, review(p)); err != nil {
		t.Fatal(err)
	}
	assertNotInstalled(t, f)
	service, err := New(ctx, f.registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = service.RecoverApprovals(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := service.Get(ctx, p.ID)
	if err != nil || got.Status != "published" || got.Approval == nil || f.calls.Load() != 1 {
		t.Fatal("approved decision not recovered", err)
	}
	if err = service.RecoverApprovals(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestApprovalRecoverySkipsRemovedSourceWithoutLosingDecision(t *testing.T) {
	f, ctx := newFixture(t, nil), context.Background()
	p := f.submit(t, createRequest("removed-source"))
	if err := f.service.recordApproval(ctx, p.ID, review(p)); err != nil {
		t.Fatal(err)
	}
	registry, err := skills.NewRegistry(ctx, harness.SkillsConfig{}, f.db.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	service, err := New(ctx, registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = service.RecoverApprovals(ctx); err != nil {
		t.Fatal("removed source prevented host startup", err)
	}
	got, err := f.service.Get(ctx, p.ID)
	if err != nil || got.Status != "pending_review" || got.Approval == nil {
		t.Fatal("removed source lost decision", err)
	}
	assertNotInstalled(t, f)
}

func TestApprovalIdentityCannotBeReboundToAnotherCandidate(t *testing.T) {
	for _, field := range []string{"proposal", "candidate", "target", "evaluation"} {
		t.Run(field, func(t *testing.T) {
			f, ctx := newFixture(t, nil), context.Background()
			p := f.submit(t, createRequest("identity"))
			if err := f.service.recordApproval(ctx, p.ID, review(p)); err != nil {
				t.Fatal(err)
			}
			got, err := f.service.Get(ctx, p.ID)
			if err != nil {
				t.Fatal(err)
			}
			a := *got.Approval
			switch field {
			case "proposal":
				a.ProposalID = "p_other"
			case "candidate":
				a.CandidateVersion = "different"
			case "target":
				a.Target.SourceIdentity = "different"
			case "evaluation":
				a.EvaluationDigest = "different"
			}
			data, _ := json.Marshal(a)
			if _, err = f.db.DB().Exec(`UPDATE df_skill_approvals SET payload=?,payload_sha=? WHERE proposal_id=?`, data, hash(data), p.ID); err != nil {
				t.Fatal(err)
			}
			if _, err = f.service.RetryPublication(ctx, p.ID, p.StateVersion); err == nil {
				t.Fatal("rebound approval accepted")
			}
			assertNotInstalled(t, f)
		})
	}
}

func TestCreationGuardSurvivesRestartAndMigratesLegacyProposal(t *testing.T) {
	f, ctx := newFixture(t, nil), context.Background()
	r := createRequest("legacy")
	f.submit(t, r)
	writeCandidate(t, f, r)
	if _, err := f.db.DB().Exec(`DELETE FROM df_skill_creation_guards`); err != nil {
		t.Fatal(err)
	}
	if err := f.registry.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.registry, err = skills.NewRegistry(ctx, f.cfg, f.db.DB())
	if err != nil {
		t.Fatal(err)
	}
	if err = f.registry.InstallConfigured(ctx, "project", "created", true); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = f.db.DB().QueryRow(`SELECT count(*) FROM df_skill_installs WHERE name='created'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("startup bypassed review", count, err)
	}
}
