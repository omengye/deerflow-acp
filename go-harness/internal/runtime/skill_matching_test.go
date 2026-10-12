package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
	"github.com/omengye/deerflow-acp/go-harness/internal/learning"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

type matchingTestModel struct {
	seen          []harness.SkillExperienceMatchInput
	relation      string
	suggestedName string
	foreign       bool
	mutate        bool
	beforeReserve func()
	during        func()
}

func (m *matchingTestModel) Identity() string { return "matching-fixture-v1" }
func (m *matchingTestModel) Match(ctx context.Context, input harness.SkillExperienceMatchInput) (harness.SkillExperienceMatchDecision, error) {
	m.seen = append(m.seen, input)
	if m.beforeReserve != nil {
		m.beforeReserve()
	}
	if err := consolidationTestCall(ctx, "skill_match"); err != nil {
		return harness.SkillExperienceMatchDecision{}, err
	}
	d := harness.SkillExperienceMatchDecision{Relation: "body_gap", Reason: "Missing complete-text instruction token=PRIVATE_MATCH_REASON"}
	if len(input.Skills) != 0 {
		d.SkillID = input.Skills[0].ID
	} else {
		d.Relation = "new_capability"
	}
	if m.relation != "" {
		d.Relation = m.relation
		if d.Relation == "new_capability" || d.Relation == "ambiguous" || d.Relation == "irrelevant" {
			d.SkillID = ""
		}
	}
	if m.foreign {
		d.Relation, d.SkillID = "body_gap", "choice_foreign"
		if m.mutate && len(input.Skills) != 0 {
			input.Skills[0].ID = d.SkillID
		}
	}
	if d.Relation == "new_capability" {
		d.SuggestedName = m.suggestedName
	}
	if m.during != nil {
		m.during()
	}
	return d, nil
}

func newMatchingTest(t *testing.T) (*Service, harness.Session, *skills.Registry, *consolidationTestExtractor, *matchingTestModel) {
	t.Helper()
	s, x, _ := newLearningTest(t, true)
	_ = s.Store.Learning.Close()
	root := filepath.Join(x.CWD, "skills")
	if err := os.MkdirAll(filepath.Join(root, "json-check"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "json-check", "SKILL.md"), []byte("---\nname: json-check\ndescription: Inspect JSON structured configuration token=PRIVATE_SKILL_MATCH_DESCRIPTION\n---\nValidate required fields token=PRIVATE_SKILL_MATCH_BODY\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := skills.NewRegistry(context.Background(), harness.SkillsConfig{Sources: []harness.SkillSource{{ID: "project", Root: root, Scope: harness.SkillScopeWorkspace, Workspace: x.CWD}}}, s.Store.db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	if _, err = r.Install(context.Background(), "project", "json-check"); err != nil {
		t.Fatal(err)
	}
	if err = r.SetEnabled(context.Background(), "project", "json-check", true); err != nil {
		t.Fatal(err)
	}
	access, err := r.NewMatchAccess(harness.SkillSelection{})
	if err != nil {
		t.Fatal(err)
	}
	options := consolidationTestOptions()
	options.Matching = &harness.SkillGenerationModelConfig{}
	l, err := learning.New(context.Background(), s.Store.db, options)
	if err != nil {
		t.Fatal(err)
	}
	extractor, consolidator, matcher := &consolidationTestExtractor{observed: true}, &consolidationTestModel{}, &matchingTestModel{}
	if err = l.Configure(context.Background(), extractor, nil); err != nil {
		t.Fatal(err)
	}
	if err = l.ConfigureConsolidation(consolidator); err != nil {
		t.Fatal(err)
	}
	if err = l.ConfigureMatching(matcher, access); err != nil {
		t.Fatal(err)
	}
	s.Store.Learning = l
	t.Cleanup(func() { _ = l.Close() })
	return s, x, r, extractor, matcher
}

func matchingQueue(t *testing.T, s *Service) {
	t.Helper()
	worked, err := s.Store.Learning.QueueMatchOne(context.Background())
	if err == nil && !worked {
		worked, err = s.Store.Learning.QueueMatchOne(context.Background())
	}
	if err != nil || !worked {
		t.Fatal("matching did not queue", worked, err)
	}
}

func matchingRun(t *testing.T, s *Service) {
	t.Helper()
	if worked, err := s.Store.Learning.RunOne(context.Background()); err != nil || !worked {
		t.Fatal("matching did not run", worked, err)
	}
}

func matchingJob(t *testing.T, s *Service) (string, string) {
	t.Helper()
	var id, state string
	if err := s.Store.db.QueryRow(`SELECT m.job_id,j.state FROM df_learning_matches m JOIN df_skill_maintenance_jobs j ON j.id=m.job_id ORDER BY m.rowid DESC LIMIT 1`).Scan(&id, &state); err != nil {
		t.Fatal(err)
	}
	return id, state
}

func TestSkillLearningMatchingPinsTargetAndDoesNotFabricateReadsOrGeneration(t *testing.T) {
	s, x, _, _, matcher := newMatchingTest(t)
	ctx := context.Background()
	consolidateExtract(t, s, x, "Inspect JSON fields")
	consolidateRun(t, s)
	matchingQueue(t, s)
	matchingRun(t, s)
	page, err := s.SkillExperienceMatches(ctx, "owner", x.ID, "", 10)
	if err != nil || len(page.Matches) != 1 || len(matcher.seen) != 1 {
		t.Fatal(page, err)
	}
	m := page.Matches[0]
	if m.Target == nil || m.Target.SourceID != "project" || m.Target.Name != "json-check" || m.Target.Workspace != x.CWD || m.Head == nil || m.Head.StateVersion < 1 || !m.Head.Exists || !m.Head.Enabled || m.Relation != "body_gap" || len(m.ActualReads) != 0 || m.SourceState != "not_revalidated" || m.Origin != "model_matched" || m.Quality != "unassessed" || m.EvidenceState != "insufficient_evidence" || strings.Contains(m.Reason, "PRIVATE_MATCH_REASON") {
		t.Fatal("match falsely inferred reads, quality or unfixed target", m)
	}
	view := matcher.seen[0]
	if len(view.Skills) != 1 || view.Skills[0].ActuallyRead || view.Skills[0].CurrentVersionRead || !view.InventoryComplete || strings.Contains(view.Skills[0].Body, "PRIVATE_SKILL_MATCH_BODY") || strings.Contains(view.Skills[0].Description, "PRIVATE_SKILL_MATCH_DESCRIPTION") {
		t.Fatal("matching model saw secrets or fabricated attribution", view)
	}
	if _, err = s.SkillExperienceMatches(ctx, "foreign-owner", x.ID, "", 10); err == nil {
		t.Fatal("foreign owner read matches")
	}
	var generated int
	if err = s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_maintenance_jobs WHERE owner_kind='skill'`).Scan(&generated); err != nil || generated != 0 {
		t.Fatal("matching granted generation", generated, err)
	}
	jobs, err := s.SkillLearningJobs(ctx, "owner", x.ID, "", 10)
	if err != nil || len(jobs.Jobs) != 3 {
		t.Fatal(jobs, err)
	}
	for _, j := range jobs.Jobs {
		if j.Kind == "skill_match" && (j.PatternID != m.PatternID || j.PatternRevision != m.PatternRevision || j.EpisodeID != "") {
			t.Fatal("matching job incorrectly attributed to one episode", j)
		}
	}
	var snapshot []byte
	if err = s.Store.db.QueryRow(`SELECT payload FROM df_learning_matches LIMIT 1`).Scan(&snapshot); err != nil || strings.Contains(string(snapshot), "PRIVATE_SKILL_MATCH_") {
		t.Fatal("derived matcher snapshot duplicated sensitive skill metadata", err)
	}
	status, err := s.Store.Learning.Status(ctx)
	if err != nil || !status.MatchingEnabled || status.Matches != 1 || status.Mode != "shadow" {
		t.Fatal(status, err)
	}
	job, _ := matchingJob(t, s)
	if _, err = s.Store.db.Exec(`UPDATE df_skill_maintenance_jobs SET state='queued',token='',lease_ns=0,retry_ns=0 WHERE id=?`, job); err != nil {
		t.Fatal(err)
	}
	matchingRun(t, s)
	if len(matcher.seen) != 1 {
		t.Fatal("saved matching result called model again")
	}
}

func TestSkillLearningMatchingActualReadsPrioritizedButOldVersionNotCurrent(t *testing.T) {
	s, x, r, _, matcher := newMatchingTest(t)
	ctx := context.Background()
	// Install a lexically better match. A real read still ranks first.
	other := filepath.Join(x.CWD, "skills", "aaa-json")
	if err := os.MkdirAll(other, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "SKILL.md"), []byte("---\nname: aaa-json\ndescription: PowerShell JSON checks structured configuration full text verification\n---\nInspect text\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.InstallConfigured(ctx, "project", "aaa-json", true); err != nil {
		t.Fatal(err)
	}
	req := learningTestRun(t, s, x, "Inspect JSON using the loaded skill")
	snapshot, err := r.Snapshot(ctx, harness.SkillSelection{Workspace: x.CWD})
	if err != nil {
		t.Fatal(err)
	}
	var observation skills.ReadObservation
	if _, err = snapshot.WithReadObserver(func(_ context.Context, e skills.ReadObservation) error { observation = e; return nil }).Get(ctx, "json-check"); err != nil {
		t.Fatal(err)
	}
	tx, err := s.Store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	scope := foregroundBudgetScope(req)
	scope.AttemptID = "matching-native-read-fixture"
	err = recordSkillReadTx(ctx, tx, scope, interaction.SkillRead{RunID: req.RunID, SessionID: x.ID, Read: observation})
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"tool_start", "tool_execute", "tool_end"} {
		event := receiptEvent(req, kind, "read-json", "read_file")
		if kind == "tool_end" {
			event.Content = []harness.Content{{Type: "text", Text: `{"verified":true}`}}
		}
		if _, err = s.Store.Append(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	learningTestFinish(t, s, req, "end_turn")
	learningTestQueue(t, s)
	matchingRun(t, s) // extraction
	consolidateRun(t, s)
	if err = os.WriteFile(filepath.Join(x.CWD, "skills", "json-check", "SKILL.md"), []byte("---\nname: json-check\ndescription: Inspect JSON structured configuration\n---\nA newer body\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Install(ctx, "project", "json-check"); err != nil {
		t.Fatal(err)
	}
	matchingQueue(t, s)
	matchingRun(t, s)
	page, err := s.Store.Learning.Matches(ctx, x.ID, "", 10)
	if err != nil || len(page.Matches) != 1 || len(matcher.seen) != 1 || len(matcher.seen[0].Skills) != 2 {
		t.Fatal(page, err, matcher.seen)
	}
	choice := matcher.seen[0].Skills[0]
	m := page.Matches[0]
	if choice.Name != "json-check" || !choice.ActuallyRead || choice.CurrentVersionRead || len(m.ActualReads) != 1 || m.ActualReads[0] != observation.Ref || m.Head.Version == observation.Ref.Version {
		t.Fatal("old reads conflated with current target", choice, m)
	}
}

func TestSkillLearningMatchingRejectsStaleConsentSourceHeadAndPattern(t *testing.T) {
	for _, mode := range []string{"consent", "consent-before-reserve", "source", "workspace", "head-before-reserve", "head-after-call", "overlay", "revision", "cancel", "foreign", "mutated-shortlist"} {
		t.Run(mode, func(t *testing.T) {
			s, x, r, _, matcher := newMatchingTest(t)
			ctx := context.Background()
			consolidateExtract(t, s, x, "Inspect concrete JSON input")
			consolidateRun(t, s)
			matchingQueue(t, s)
			job, _ := matchingJob(t, s)
			action := func() {
				var err error
				switch mode {
				case "consent", "consent-before-reserve":
					_, err = s.Store.Learning.SetControl(ctx, x.ID, harness.SkillLearningControlRequest{ExpectedEpoch: 1})
				case "workspace":
					_, err = s.Store.db.Exec(`UPDATE harness_sessions SET cwd=? WHERE id=?`, t.TempDir(), x.ID)
				case "source":
					_, err = s.Store.db.Exec(`UPDATE harness_tool_receipts SET version=version+1 WHERE session_id=?`, x.ID)
				case "head-before-reserve", "head-after-call":
					err = r.SetEnabled(ctx, "project", "json-check", false)
					if err == nil {
						err = r.SetEnabled(ctx, "project", "json-check", true)
					}
				case "overlay":
					_, err = s.Store.db.Exec(`UPDATE df_skill_control SET managed=1 WHERE name='json-check';
 INSERT INTO df_skill_overlays SELECT source_key,name,current_version,'fixture' FROM df_skill_installs WHERE name='json-check'`)
				case "revision":
					_, err = s.Store.db.Exec(`UPDATE df_learning_patterns SET current_revision=current_revision+1 WHERE session_id=?`, x.ID)
				case "cancel":
					jobs, getErr := s.Store.Learning.Jobs(ctx, x.ID, "", 10)
					err = getErr
					for _, j := range jobs.Jobs {
						if j.ID == job {
							err = s.Store.Learning.CancelExtraction(ctx, x.ID, job, j.StateVersion)
						}
					}
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "head-before-reserve" || mode == "consent-before-reserve" {
				matcher.beforeReserve = action
			} else if mode == "foreign" || mode == "mutated-shortlist" {
				matcher.foreign, matcher.mutate = true, mode == "mutated-shortlist"
			} else {
				matcher.during = action
			}
			if worked, runErr := s.Store.Learning.RunOne(ctx); !worked || runErr != nil && !errors.Is(runErr, skillmaintenance.ErrConflict) {
				t.Fatal("matching did not finish or honor cancellation", worked, runErr)
			}
			page, err := s.Store.Learning.Matches(ctx, x.ID, "", 10)
			if err != nil || len(page.Matches) != 0 {
				t.Fatal("invalid late match became visible", page, err)
			}
			var calls int
			if err = s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_maintenance_calls WHERE job_id=?`, job).Scan(&calls); err != nil {
				t.Fatal(err)
			}
			want := 1
			if mode == "head-before-reserve" || mode == "consent-before-reserve" {
				want = 0
			}
			if calls != want {
				t.Fatal("call guard or conservative accounting failed", calls, want)
			}
		})
	}
}

func TestSkillLearningMatchingPatternAmbiguityAndConflictSkipModel(t *testing.T) {
	for _, relation := range []string{"ambiguous", "conflicting"} {
		t.Run(relation, func(t *testing.T) {
			s, x, r, _, matcher := newMatchingTest(t)
			ctx := context.Background()
			consolidateExtract(t, s, x, "First concrete JSON check")
			consolidateRun(t, s)
			l, err := learning.New(ctx, s.Store.db, consolidationTestOptions())
			if err != nil {
				t.Fatal(err)
			}
			// Reconfigure deterministic consolidation without mutating persisted
			// pattern projections to fabricate the evidence state under test.
			_ = s.Store.Learning.Close()
			extractor, consolidator := &consolidationTestExtractor{observed: true}, &consolidationTestModel{relation: relation}
			if err = l.Configure(ctx, extractor, nil); err != nil {
				t.Fatal(err)
			}
			if err = l.ConfigureConsolidation(consolidator); err != nil {
				t.Fatal(err)
			}
			s.Store.Learning = l
			consolidateExtract(t, s, x, "Different potentially incompatible method")
			consolidateRun(t, s)
			_ = l.Close()
			options := consolidationTestOptions()
			options.Matching = &harness.SkillGenerationModelConfig{}
			restarted, err := learning.New(ctx, s.Store.db, options)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = restarted.Close() })
			if err = restarted.Configure(ctx, extractor, nil); err != nil {
				t.Fatal(err)
			}
			if err = restarted.ConfigureConsolidation(consolidator); err != nil {
				t.Fatal(err)
			}
			access, err := r.NewMatchAccess(harness.SkillSelection{})
			if err != nil {
				t.Fatal(err)
			}
			if err = restarted.ConfigureMatching(matcher, access); err != nil {
				t.Fatal(err)
			}
			s.Store.Learning = restarted
			// Drain only the bounded metadata scanner, then attempt one claim.
			for range 4 {
				if _, err = restarted.QueueMatchOne(ctx); err != nil {
					t.Fatal(err)
				}
			}
			// An ambiguous new pattern is skipped; the initial, unambiguous
			// pattern may still be matched. Conflicts block both revisions.
			if relation == "ambiguous" {
				matchingRun(t, s)
			} else if worked, err := restarted.RunOne(ctx); err != nil || worked {
				t.Fatal("conflicting pattern called matcher", worked, err)
			}
			want := 0
			if relation == "ambiguous" {
				want = 1
			}
			if len(matcher.seen) != want {
				t.Fatal("blocked semantic evidence sent to model", matcher.seen)
			}
		})
	}
}

func TestSkillLearningMatchingBlockedEvidenceAndIncompleteInventoryRemainNonGenerative(t *testing.T) {
	for _, mode := range []string{"unresolved", "ambiguous", "incomplete", "no-skill"} {
		t.Run(mode, func(t *testing.T) {
			s, x, r, extractor, matcher := newMatchingTest(t)
			ctx := context.Background()
			if mode == "unresolved" {
				extractor.observed = false
			}
			if mode == "no-skill" {
				if err := r.SetEnabled(ctx, "project", "json-check", false); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "incomplete" {
				for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
					root := filepath.Join(x.CWD, "skills", name)
					if err := os.MkdirAll(root, 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: Fixture procedure\n---\nValidate input\n"), 0600); err != nil {
						t.Fatal(err)
					}
					if err := r.InstallConfigured(ctx, "project", name, true); err != nil {
						t.Fatal(err)
					}
				}
				matcher.relation = "new_capability"
			}
			if mode == "ambiguous" {
				matcher.relation = "ambiguous"
			}
			consolidateExtract(t, s, x, "Inspect JSON configuration")
			consolidateRun(t, s)
			matchingQueue(t, s)
			if mode != "unresolved" {
				matchingRun(t, s)
			}
			page, err := s.Store.Learning.Matches(ctx, x.ID, "", 10)
			want := 1
			if mode == "unresolved" || mode == "incomplete" {
				want = 0
			}
			if err != nil || len(page.Matches) != want || mode == "unresolved" && len(matcher.seen) != 0 {
				t.Fatal("blocked evidence progressed", mode, page, matcher.seen, err)
			}
			if mode == "no-skill" && page.Matches[0].Relation != "new_capability" || mode == "ambiguous" && page.Matches[0].Target != nil {
				t.Fatal("untargeted classification guessed a target", page)
			}
		})
	}
}

func TestSkillLearningMatchingResultAtomicRecoveryAndForget(t *testing.T) {
	s, x, _, _, matcher := newMatchingTest(t)
	ctx := context.Background()
	consolidateExtract(t, s, x, "Read complete structured text")
	consolidateRun(t, s)
	matchingQueue(t, s)
	if _, err := s.Store.db.Exec(`CREATE TRIGGER fixture_match_abort BEFORE UPDATE OF result ON df_learning_matches BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	matchingRun(t, s)
	var sha, state string
	if err := s.Store.db.QueryRow(`SELECT m.result_sha,i.state FROM df_learning_matches m JOIN df_learning_match_inputs i ON i.pattern_id=m.pattern_id AND i.revision=m.revision`).Scan(&sha, &state); err != nil || sha != "" || state != "queued" {
		t.Fatal("matching output transaction partly committed", sha, state, err)
	}
	job, _ := matchingJob(t, s)
	if _, err := s.Store.db.Exec(`DROP TRIGGER fixture_match_abort; UPDATE df_skill_maintenance_jobs SET state='queued',token='',lease_ns=0,retry_ns=0 WHERE id=?`, job); err != nil {
		t.Fatal(err)
	}
	matchingRun(t, s)
	if len(matcher.seen) != 2 {
		t.Fatal("fixture failed to retry actual model call")
	}
	if _, err := s.Store.db.Exec(`UPDATE df_learning_matches SET result='corrupt'; UPDATE df_skill_maintenance_jobs SET state='queued',token='',lease_ns=0,retry_ns=0 WHERE id=?`, job); err != nil {
		t.Fatal(err)
	}
	matchingRun(t, s)
	if len(matcher.seen) != 2 {
		t.Fatal("corrupt saved match called model instead of failing closed")
	}
	if _, err := s.Store.Learning.SetControl(ctx, x.ID, harness.SkillLearningControlRequest{ExpectedEpoch: 1}); err != nil {
		t.Fatal(err)
	}
	page, err := s.Store.Learning.Matches(ctx, x.ID, "", 10)
	if err != nil || len(page.Matches) != 0 {
		t.Fatal("forget retained matching bodies/results", page, err)
	}
	var calls, installed int
	if err = s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_maintenance_calls WHERE job_id=?`, job).Scan(&calls); err != nil || calls != 2 {
		t.Fatal("forget refunded model work", calls, err)
	}
	if err = s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_installs WHERE name='json-check'`).Scan(&installed); err != nil || installed != 1 {
		t.Fatal("forget altered installed skills", installed, err)
	}
}

func TestSkillLearningMatchingIndependentOptInAndDisabledHistory(t *testing.T) {
	s, x, _, _, _ := newMatchingTest(t)
	ctx := context.Background()
	consolidateExtract(t, s, x, "Inspect JSON")
	consolidateRun(t, s)
	matchingQueue(t, s)
	matchingRun(t, s)
	_ = s.Store.Learning.Close()
	l, err := learning.New(ctx, s.Store.db, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	page, err := l.Matches(ctx, x.ID, "", 10)
	if err != nil || len(page.Matches) != 1 || page.Matches[0].SourceState != "not_revalidated" {
		t.Fatal("inactive host erased valid history", page, err)
	}
	options := learningTestOptions()
	options.Matching = &harness.SkillGenerationModelConfig{}
	if _, err = learning.New(ctx, s.Store.db, options); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatal("matching enabled without consolidation", err)
	}
}

func TestSkillLearningMatchingRestartRejectsChangedHostSelectionAndModel(t *testing.T) {
	for _, mode := range []string{"same", "selection", "source-identity", "model", "disabled", "missing-input"} {
		t.Run(mode, func(t *testing.T) {
			s, x, r, extractor, matcher := newMatchingTest(t)
			ctx := context.Background()
			consolidateExtract(t, s, x, "Inspect JSON fields")
			consolidateRun(t, s)
			matchingQueue(t, s)
			job, _ := matchingJob(t, s)
			_ = s.Store.Learning.Close()
			options := consolidationTestOptions()
			options.Matching = &harness.SkillGenerationModelConfig{}
			if mode == "disabled" {
				options.Matching = nil
			}
			l, err := learning.New(ctx, s.Store.db, options)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = l.Close() })
			if err = l.Configure(ctx, extractor, nil); err != nil {
				t.Fatal(err)
			}
			if err = l.ConfigureConsolidation(&consolidationTestModel{}); err != nil {
				t.Fatal(err)
			}
			if mode != "disabled" {
				selection := harness.SkillSelection{}
				if mode == "selection" {
					selection.Names = []string{}
				}
				if mode == "source-identity" {
					root := filepath.Join(x.CWD, "different-source")
					if err = os.MkdirAll(root, 0700); err != nil {
						t.Fatal(err)
					}
					r, err = skills.NewRegistry(ctx, harness.SkillsConfig{Sources: []harness.SkillSource{{ID: "project", Root: root, Scope: harness.SkillScopeWorkspace, Workspace: x.CWD}}}, s.Store.db)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = r.Close() })
				}
				access, err := r.NewMatchAccess(selection)
				if err != nil {
					t.Fatal(err)
				}
				var adapter harness.SkillExperienceMatcher = matcher
				if mode == "model" {
					adapter = &matchingChangedModel{matcher}
				}
				if err = l.ConfigureMatching(adapter, access); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "missing-input" {
				if _, err = s.Store.db.Exec(`DELETE FROM df_learning_match_inputs`); err != nil {
					t.Fatal(err)
				}
			}
			s.Store.Learning = l
			worked, err := l.RunOne(ctx)
			if err != nil || worked != (mode != "disabled") {
				t.Fatal("restart producer selection failed", worked, err)
			}
			want := 0
			if mode == "same" {
				want = 1
			}
			page, err := l.Matches(ctx, x.ID, "", 10)
			if err != nil || len(page.Matches) != want || len(matcher.seen) != want {
				t.Fatal("changed host authority called model or restored match", mode, page, err)
			}
			var state string
			if err = s.Store.db.QueryRow(`SELECT state FROM df_skill_maintenance_jobs WHERE id=?`, job).Scan(&state); err != nil || mode == "disabled" && state != "cancelled" {
				t.Fatal("disabled match claim survived restart", state, err)
			}
		})
	}
}

type matchingChangedModel struct{ *matchingTestModel }

func (m *matchingChangedModel) Identity() string { return "changed-matching-model-v2" }

func TestSkillLearningMatchingMigrationDoesNotResurrectMissingStageInput(t *testing.T) {
	s, x, _, _, _ := newMatchingTest(t)
	ctx := context.Background()
	consolidateExtract(t, s, x, "Prior P2 pattern")
	consolidateRun(t, s)
	if _, err := s.Store.db.Exec(`DELETE FROM df_learning_match_inputs`); err != nil {
		t.Fatal(err)
	}
	migrate := func() {
		t.Helper()
		tx, err := s.Store.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = learning.Migrate(ctx, tx); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	migrate()
	var count int
	if err := s.Store.db.QueryRow(`SELECT count(*) FROM df_learning_match_inputs`).Scan(&count); err != nil || count != 0 {
		t.Fatal("startup recreated erased stage records", count, err)
	}
	if _, err := s.Store.db.Exec(`DELETE FROM df_learning_migrations WHERE name='skill-match-inputs-v1'`); err != nil {
		t.Fatal(err)
	}
	migrate()
	if err := s.Store.db.QueryRow(`SELECT count(*) FROM df_learning_match_inputs`).Scan(&count); err != nil || count != 1 {
		t.Fatal("one-time historical pattern seed missing", count, err)
	}
}

func TestSkillLearningMatchingQueueBackpressurePreservesEvidenceForLater(t *testing.T) {
	s, x, _, _, matcher := newMatchingTest(t)
	ctx := context.Background()
	consolidateExtract(t, s, x, "A verified JSON check")
	consolidateRun(t, s)
	queue, err := skillmaintenance.New(ctx, s.Store.db, consolidationTestOptions().Budget)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		if _, err = queue.Enqueue(ctx, skillmaintenance.Request{Kind: skillmaintenance.SkillMatch, OwnerKind: skillmaintenance.WorkspaceOwner, OwnerKey: strings.Repeat("f", 64), Key: "backlog-" + string(rune('a'+i)), Reference: "fixture-backlog", InputDigest: strings.Repeat("e", 64)}); err != nil {
			t.Fatal(err)
		}
	}
	if worked, err := s.Store.Learning.QueueMatchOne(ctx); err != nil || worked {
		t.Fatal("matching bypassed learning pending cap", worked, err)
	}
	var state, reason string
	if err = s.Store.db.QueryRow(`SELECT state,reason FROM df_learning_match_inputs`).Scan(&state, &reason); err != nil || state != "pending" || reason != "queue_backpressure" || len(matcher.seen) != 0 {
		t.Fatal("backpressure lost input or called model", state, reason, err)
	}
	if _, err = s.Store.db.Exec(`UPDATE df_skill_maintenance_jobs SET state='cancelled' WHERE reference='fixture-backlog'`); err != nil {
		t.Fatal(err)
	}
	matchingQueue(t, s)
	matchingRun(t, s)
	page, err := s.Store.Learning.Matches(ctx, x.ID, "", 10)
	if err != nil || len(page.Matches) != 1 {
		t.Fatal("pending evidence did not recover", page, err)
	}
}
