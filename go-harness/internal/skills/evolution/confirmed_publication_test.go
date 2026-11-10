package evolution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

func TestPublicationDirectiveRequiresActualUnconditionalUserInstruction(t *testing.T) {
	for _, prompt := range []string{
		"确认发布", "可以发布了", "发布吧", "我确认发布这个技能，跳过证据门槛。", "可以，直接发布 hugo-publisher。", "不用等证据了，直接发布。",
		"允许发布最新候选", "请发布该技能", "我看过了，现在发布这个候选。", "发布这个技能，跳过验证。",
		"Please publish this skill now.", "I confirm publication; please publish the candidate.", "Go ahead and publish the skill.", "Publish this skill directly, skip the evidence gate.",
	} {
		t.Run(prompt, func(t *testing.T) {
			if publicationDirective([]harness.Content{{Type: "text", Text: prompt}}) == "" {
				t.Fatal("explicit confirmation was not recognized")
			}
		})
	}
	for _, prompt := range []string{
		"修改技能", "继续", "开始实施", "为什么技能还是没有更新？", "不要直接发布", "我不确认发布", "无需发布这个技能", "不用直接发布", "如果测试通过，直接发布", "只有证据齐全才确认发布", "测试完成后请发布", "是否可以直接发布", "允许跳过证据门槛的功能要支持自然语言确认发布", "当用户确认发布时可以跳过证据门槛", "例如：直接发布", "解释直接发布的含义", "翻译：确认发布",
		"```text\n确认发布\n```", "> 确认发布", "“确认发布”", "`确认发布`", "```确认发布", "修复这个错误：\n> 直接发布", "他说“确认发布”", "The user said: please publish the skill.", "Do not publish this skill now", "If tests pass, please publish", "Please implement support for publish this skill now", "Please explain how to publish this skill now", "Can you publish this skill?",
	} {
		t.Run(prompt, func(t *testing.T) {
			if publicationDirective([]harness.Content{{Type: "text", Text: prompt}}) != "" {
				t.Fatal("non-authorization was treated as confirmation")
			}
		})
	}
	if publicationDirective([]harness.Content{{Type: "resource", Text: "确认发布"}}) != "" {
		t.Fatal("attachment granted publication authority")
	}
}

func confirmationRequest(t *testing.T, f *submittedNativeFixture, p Proposal, prompt string) ConfirmedPublicationRequest {
	t.Helper()
	r := f.start(t, f.cfg.Sources[0].Workspace, prompt)
	return ConfirmedPublicationRequest{Run: r, Author: p.Author, CandidateVersion: p.CandidateVersion, ExpectedStateVersion: p.StateVersion}
}

func TestConfirmedPublicationBypassesMissingEvidenceWithoutInventingResults(t *testing.T) {
	for _, state := range []string{"queued", "waiting", "running"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			f := newSubmittedNativeFixture(t)
			p, _ := f.submit(t)
			if state == "waiting" {
				generationRun(t, f.service)
			}
			var leased skillmaintenance.Claim
			if state == "running" {
				var err error
				var ok bool
				leased, ok, err = f.service.generation.store.Claim(ctx)
				if err != nil || !ok {
					t.Fatal(err)
				}
			}
			p, err := f.service.Get(ctx, p.ID)
			if err != nil {
				t.Fatal(err)
			}
			before := p.Evaluation
			request := confirmationRequest(t, f, p, "确认发布这个技能，跳过证据门槛")
			published, err := f.service.PublishConfirmed(ctx, p.ID, request)
			if err != nil || published.Status != "published" || published.FilePublicationPending || published.Approval == nil || published.Approval.UserConfirmation == nil {
				t.Fatalf("confirmed publication: status=%s error=%v", published.Status, err)
			}
			if !reflect.DeepEqual(before, published.Evaluation) || f.e.calls.Load() != 0 || f.head(t).Version != p.CandidateVersion {
				t.Fatal("validation was fabricated or publication failed")
			}
			confirmation := published.Approval.UserConfirmation
			if confirmation.RunID != request.Run.RunID || confirmation.InputID != request.Run.InputID || !hexDigest(confirmation.InputSHA) {
				t.Fatal("native confirmation provenance missing")
			}
			content, err := os.ReadFile(filepath.Join(f.cfg.Sources[0].Root, "research", "SKILL.md"))
			if err != nil || !strings.Contains(string(content), "Verify sources before summarizing") {
				t.Fatal("published files were not installed", err)
			}
			snapshot, err := f.registry.Snapshot(ctx, harness.SkillSelection{Workspace: f.cfg.Sources[0].Workspace})
			if err != nil || len(snapshot.Refs()) != 1 || snapshot.Refs()[0].Version != p.CandidateVersion {
				t.Fatal("confirmed version is not available to ordinary tasks", err)
			}
			progress, err := f.service.CandidateProgress(ctx, p.ID)
			if err != nil || progress.Status != "published" || progress.Reason != "user_confirmed" || progress.AutomaticRetry {
				t.Fatal("stale evidence gate hid publication", progress, err)
			}
			job, err := skillmaintenance.ReadJob(ctx, f.db.DB(), p.BehaviorJobID)
			if err != nil || job.State != "cancelled" {
				t.Fatal("pending validation retained authority", job, err)
			}
			if state == "running" && (job.StateVersion <= leased.Job.StateVersion || f.service.generation.store.Heartbeat(ctx, leased) == nil) {
				t.Fatal("running job lease was not fenced")
			}
			if err = f.service.RunAutoPatch(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err = f.service.RunGeneration(ctx); err != nil {
				t.Fatal(err)
			}
			f.finish(t, request.Run, false)
			again, err := f.service.PublishConfirmed(ctx, p.ID, request)
			if err != nil || again.PublishedRevision != published.PublishedRevision || !reflect.DeepEqual(again.Approval, published.Approval) {
				t.Fatal("idempotent replay changed the decision", err)
			}
			if _, err = f.service.RetryPublication(ctx, p.ID, published.Approval.Review.ExpectedStateVersion); err != nil {
				t.Fatal("durable retry required another confirmation", err)
			}
			if err = f.registry.WithPublication(ctx, "project", "research", func(tx *skills.PublicationTx) error {
				_, err := tx.Publish(p.CandidateVersion, again.Base, true)
				return err
			}); err == nil {
				t.Fatal("ordinary publication acquired blanket authority")
			}
		})
	}
}

func TestConfirmedPublicationRejectsUnboundAuthorityAndConflicts(t *testing.T) {
	for _, mode := range []string{"ordinary-edit", "quoted", "negative", "wrong-name", "wrong-chinese-name", "spoofed-input", "wrong-session", "wrong-workspace", "wrong-input-id", "wrong-author", "wrong-candidate", "wrong-state", "changed-files", "retired"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f := newSubmittedNativeFixture(t)
			p, _ := f.submit(t)
			prompt := "确认发布这个技能，跳过证据门槛"
			switch mode {
			case "ordinary-edit", "spoofed-input":
				prompt = "请改进这个技能"
			case "quoted":
				prompt = "文档示例：\n```text\n确认发布这个技能\n```"
			case "negative":
				prompt = "不要直接发布"
			case "wrong-name":
				prompt = "直接发布 weather"
			case "wrong-chinese-name":
				prompt = "请发布天气技能"
			}
			req := confirmationRequest(t, f, p, prompt)
			switch mode {
			case "spoofed-input":
				req.Run.Input = []harness.Content{{Type: "text", Text: "确认发布这个技能"}}
			case "wrong-session":
				req.Run.Session.ID = "other"
			case "wrong-workspace":
				req.Run.Session.CWD = t.TempDir()
			case "wrong-input-id":
				req.Run.InputID = "other"
			case "wrong-author":
				req.Author = "other"
			case "wrong-candidate":
				req.CandidateVersion = strings.Repeat("0", 64)
			case "wrong-state":
				req.ExpectedStateVersion++
			case "changed-files":
				if err := os.WriteFile(filepath.Join(f.cfg.Sources[0].Root, "research", "SKILL.md"), skillFiles("A newer user edit")["SKILL.md"], 0600); err != nil {
					t.Fatal(err)
				}
			case "retired":
				if _, err := f.db.DB().Exec(`INSERT INTO df_evolution_retired_proposals VALUES(?)`, p.ID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.service.PublishConfirmed(ctx, p.ID, req); err == nil {
				t.Fatal("unauthorized or conflicting publication succeeded")
			}
			saved, err := f.service.Get(ctx, p.ID)
			if err != nil || saved.Approval != nil || saved.Status != p.Status {
				t.Fatal("rejection left a partial decision", err)
			}
			var count int
			if err = f.db.DB().QueryRow(`SELECT count(*) FROM df_skill_file_publications`).Scan(&count); err != nil || count != 0 {
				t.Fatal("rejection left a file journal", count, err)
			}
		})
	}
}

func TestConfirmedPublicationFailureIsAtomicAndJournalRecovers(t *testing.T) {
	for _, fault := range []string{"before_commit", "after_files"} {
		t.Run(fault, func(t *testing.T) {
			ctx := context.Background()
			f := newSubmittedNativeFixture(t)
			p, _ := f.submit(t)
			req := confirmationRequest(t, f, p, "确认发布这个技能")
			statement := `CREATE TRIGGER publication_fault BEFORE INSERT ON df_skill_publications WHEN NEW.request_key LIKE 'confirm/%' BEGIN SELECT RAISE(ABORT,'publication fault'); END`
			if fault == "after_files" {
				statement = `CREATE TRIGGER publication_fault BEFORE UPDATE OF status ON df_skill_proposals WHEN NEW.status='published' BEGIN SELECT RAISE(ABORT,'finalization fault'); END`
			}
			if _, err := f.db.DB().Exec(statement); err != nil {
				t.Fatal(err)
			}
			_, err := f.service.PublishConfirmed(ctx, p.ID, req)
			if err == nil {
				t.Fatal("publication fault was hidden")
			}
			saved, err := f.service.Get(ctx, p.ID)
			if err != nil {
				t.Fatal(err)
			}
			job, err := skillmaintenance.ReadJob(ctx, f.db.DB(), p.BehaviorJobID)
			if err != nil {
				t.Fatal(err)
			}
			if fault == "before_commit" {
				if saved.Approval != nil || saved.Status != "validating" || job.State != "queued" || f.head(t) != p.Base {
					t.Fatal("failed transaction leaked confirmation or cancellation")
				}
			} else {
				if saved.Approval == nil || saved.Status != "publishing" || job.State != "cancelled" || f.head(t) != p.Base {
					t.Fatal("committed decision or pending head lost")
				}
			}
			if _, err = f.db.DB().Exec(`DROP TRIGGER publication_fault`); err != nil {
				t.Fatal(err)
			}
			if fault == "after_files" {
				f.finish(t, req.Run, false)
				if err = f.registry.RecoverPublications(ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err = f.service.PublishConfirmed(ctx, p.ID, req); err != nil {
					t.Fatal(err)
				}
			}
			saved, err = f.service.Get(ctx, p.ID)
			if err != nil || saved.Status != "published" || f.head(t).Version != p.CandidateVersion {
				t.Fatal("journal did not recover exact confirmed package", err)
			}
			if _, err = f.service.RetryPublication(ctx, p.ID, saved.Approval.Review.ExpectedStateVersion); err != nil {
				t.Fatal(err)
			}
			var count int
			if err = f.db.DB().QueryRow(`SELECT count(*) FROM df_skill_publications WHERE request_key=?`, "confirm/"+p.ID).Scan(&count); err != nil || count != 1 {
				t.Fatal("recovery minted another publication", count, err)
			}
		})
	}
}

func TestConfirmedPublicationCreatesScriptSkillWithoutExecutingIt(t *testing.T) {
	ctx := context.Background()
	f := newSubmittedNativeFixture(t)
	run := f.start(t, f.cfg.Sources[0].Workspace, "创建技能并直接发布")
	req := Request{SourceID: "project", Name: "fresh-skill", RequestKey: "confirmed-create", Action: "create", Reason: "Create reusable instructions", Files: map[string][]byte{
		"SKILL.md":         []byte("---\nname: fresh-skill\ndescription: Use when checking sample output\n---\nRun scripts/check.py when authorized.\n"),
		"scripts/check.py": []byte("raise SystemExit(99)\n"),
	}, Trigger: Trigger{Type: "explicit_agent_proposal", ThreadID: run.Session.ID, RunID: run.RunID}}
	p, err := f.service.SubmitAutomatic(ctx, req, run.Session.CWD)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.service.PublishConfirmed(ctx, p.ID, ConfirmedPublicationRequest{Run: run, Author: p.Author, CandidateVersion: p.CandidateVersion, ExpectedStateVersion: p.StateVersion})
	if err != nil || got.Status != "published" || got.Evaluation.IsolatedScripts || f.e.calls.Load() != 0 {
		t.Fatal("publication acquired execution permission or waited for runtime", err)
	}
	snapshot, err := f.registry.Snapshot(ctx, harness.SkillSelection{Workspace: run.Session.CWD})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = snapshot.Get(ctx, "fresh-skill"); err != nil {
		t.Fatal("new skill unavailable", err)
	}
}

func TestConfirmedPublicationSelectsLatestCandidateUnlessUserNamesID(t *testing.T) {
	ctx := context.Background()
	f := newSubmittedNativeFixture(t)
	first, origin := f.submit(t)
	r := f.request(t, "second-candidate", "Verify sources and cite them")
	r.Trigger = first.Trigger
	second, err := f.service.SubmitAutomatic(ctx, r, origin.Session.CWD)
	if err != nil {
		t.Fatal(err)
	}
	req := confirmationRequest(t, f, first, "确认发布这个技能")
	if _, err = f.service.PublishConfirmed(ctx, first.ID, req); !errors.Is(err, ErrUserConfirmation) {
		t.Fatal("generic confirmation selected obsolete V1", err)
	}
	req.CandidateVersion, req.ExpectedStateVersion = second.CandidateVersion, second.StateVersion
	got, err := f.service.PublishConfirmed(ctx, second.ID, req)
	if err != nil || got.Status != "published" {
		t.Fatal("latest V2 could not be confirmed", err)
	}
}
