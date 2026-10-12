package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/learning"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

func newWorkspaceLearningTest(t *testing.T, matching bool) (*Service, harness.Session, *consolidationTestModel, *matchingTestModel) {
	t.Helper()
	var s *Service
	var x harness.Session
	var registry *skills.Registry
	if matching {
		s, x, registry, _, _ = newMatchingTest(t)
	} else {
		s, x, _, _ = newConsolidationTest(t)
	}
	_ = s.Store.Learning.Close()
	options := consolidationTestOptions()
	options.CrossSession = true
	if matching {
		options.Matching = &harness.SkillGenerationModelConfig{}
	}
	l, err := learning.New(context.Background(), s.Store.db, options)
	if err != nil {
		t.Fatal(err)
	}
	model, matcher := &consolidationTestModel{}, &matchingTestModel{}
	if err = l.Configure(context.Background(), &consolidationTestExtractor{observed: true}, nil); err != nil {
		t.Fatal(err)
	}
	if err = l.ConfigureConsolidation(model); err != nil {
		t.Fatal(err)
	}
	if matching {
		access, err := registry.NewMatchAccess(harness.SkillSelection{})
		if err != nil {
			t.Fatal(err)
		}
		if err = l.ConfigureMatching(matcher, access); err != nil {
			t.Fatal(err)
		}
	}
	s.Store.Learning = l
	t.Cleanup(func() { _ = l.Close() })
	return s, x, model, matcher
}

func workspaceLearningConsent(t *testing.T, s *Service, x harness.Session, shared bool) harness.SkillLearningControl {
	t.Helper()
	ctx := context.Background()
	control, err := s.Store.Learning.Control(ctx, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	control, err = s.SetSkillLearningControl(ctx, "owner", x.ID, harness.SkillLearningControlRequest{Contribute: true, ExpectedEpoch: control.Epoch, WorkspaceShared: &shared})
	if err != nil || control.WorkspaceShared != shared {
		t.Fatal(control, err)
	}
	return control
}

func workspaceLearningSession(t *testing.T, s *Service, cwd string) harness.Session {
	t.Helper()
	x, err := s.NewSession(context.Background(), "owner", cwd)
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func TestSkillLearningWorkspaceConsentIsolationAndIndependentEpisodes(t *testing.T) {
	s, x, model, _ := newWorkspaceLearningTest(t, false)
	ctx := context.Background()
	if _, err := s.Store.Learning.WorkspacePatterns(ctx, x.ID, "", 10); err == nil {
		t.Fatal("unconsented workspace query accepted")
	}
	first := workspaceLearningConsent(t, s, x, true)
	again := workspaceLearningConsent(t, s, x, true)
	if first.Epoch != again.Epoch {
		t.Fatal("identical sharing change revoked consent")
	}
	consolidateExtract(t, s, x, "First independent structured configuration task")
	consolidateRun(t, s)
	private := workspaceLearningSession(t, s, x.CWD)
	consolidateExtract(t, s, private, "Private structured configuration task")
	consolidateRun(t, s)
	if len(model.seen[1].Patterns) != 0 {
		t.Fatal("same directory silently granted cross-chat text")
	}
	privatePage, err := s.SkillExperiencePatterns(ctx, "owner", private.ID, "", 10)
	if err != nil || len(privatePage.Patterns) != 1 {
		t.Fatal(privatePage, err)
	}
	y := workspaceLearningSession(t, s, x.CWD)
	workspaceLearningConsent(t, s, y, true)
	consolidateExtract(t, s, y, "Second independent structured configuration task")
	consolidateRun(t, s)
	page, err := s.Store.Learning.WorkspacePatterns(ctx, y.ID, "", 10)
	if err != nil || len(page.Patterns) != 1 || page.Patterns[0].Revision != 2 || page.Patterns[0].SupportEpisodes != 2 || page.Patterns[0].ObservedEpisodes != 2 || page.Patterns[0].EvidenceState != "observed_execution" || page.Patterns[0].Quality != "unassessed" {
		t.Fatal("authorized tasks did not merge", page, err)
	}
	for _, session := range []harness.Session{x, y} {
		ordinary, err := s.SkillExperiencePatterns(ctx, "owner", session.ID, "", 10)
		if err != nil || len(ordinary.Patterns) != 0 {
			t.Fatal("shared text leaked through ordinary session history", ordinary, err)
		}
	}
	if len(model.seen[2].Patterns) != 1 || model.seen[2].Patterns[0].ID != page.Patterns[0].ID {
		t.Fatal("shared shortlist contains a private pattern")
	}
	foreign := workspaceLearningSession(t, s, t.TempDir())
	workspaceLearningConsent(t, s, foreign, true)
	consolidateExtract(t, s, foreign, "Foreign workspace configuration task")
	consolidateRun(t, s)
	if len(model.seen[3].Patterns) != 0 {
		t.Fatal("shared shortlist crossed the workspace boundary")
	}
	if _, err = s.SetSkillLearningControl(ctx, "owner", y.ID, harness.SkillLearningControlRequest{Contribute: true, ExpectedEpoch: 0}); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatal("stale sharing epoch accepted", err)
	}
}

func TestSkillLearningWorkspaceCopiedTaskLineageAndResumeDoNotAddVotes(t *testing.T) {
	s, x, _, _ := newWorkspaceLearningTest(t, false)
	ctx := context.Background()
	sourceControl := workspaceLearningConsent(t, s, x, true)
	consolidateExtract(t, s, x, "Identical accepted task")
	consolidateRun(t, s)
	page, err := s.Store.Learning.WorkspacePatterns(ctx, x.ID, "", 10)
	if err != nil || len(page.Patterns) != 1 {
		t.Fatal(page, err)
	}
	copySession := workspaceLearningSession(t, s, x.CWD)
	workspaceLearningConsent(t, s, copySession, true)
	consolidateExtract(t, s, copySession, "Identical accepted task")
	consolidateRun(t, s)
	page, err = s.Store.Learning.WorkspacePatterns(ctx, x.ID, "", 10)
	if err != nil || page.Patterns[0].SupportEpisodes != 1 || page.Patterns[0].ObservedEpisodes != 1 {
		t.Fatal("copied task inflated support", page, err)
	}
	linked := workspaceLearningSession(t, s, x.CWD)
	linkedControl := workspaceLearningConsent(t, s, linked, true)
	request := harness.SkillLearningLineageRequest{SourceSessionID: x.ID, SourceEpisodeID: page.Patterns[0].EpisodeIDs[0], ExpectedEpoch: linkedControl.Epoch, ExpectedSourceEpoch: sourceControl.Epoch}
	var sourceEpisode string
	if err = s.Store.db.QueryRow(`SELECT id FROM df_learning_episodes WHERE session_id=?`, x.ID).Scan(&sourceEpisode); err != nil {
		t.Fatal(err)
	}
	request.SourceEpisodeID = sourceEpisode
	if _, err = s.Store.Learning.LinkEpisodeLineage(ctx, linked.ID, request); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Store.Learning.LinkEpisodeLineage(ctx, linked.ID, request); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatal("lineage silently rebound", err)
	}
	consolidateExtract(t, s, linked, "Rephrased copied task still has the same origin")
	consolidateRun(t, s)
	if _, err = s.Load(ctx, "owner", linked.ID, linked.CWD, false, nil); err != nil {
		t.Fatal(err)
	}
	consolidateExtract(t, s, linked, "Resume and correct this task")
	consolidateRun(t, s)
	page, err = s.Store.Learning.WorkspacePatterns(ctx, linked.ID, "", 10)
	if err != nil || page.Patterns[0].SupportEpisodes != 1 || len(page.Patterns[0].ExperienceIDs) != 4 {
		t.Fatal("linked/resumed task inflated support", page, err)
	}
	if _, err = s.StartSkillLearningEpisode(ctx, "owner", linked.ID); err != nil {
		t.Fatal(err)
	}
	consolidateExtract(t, s, linked, "Explicit independent task")
	consolidateRun(t, s)
	page, err = s.Store.Learning.WorkspacePatterns(ctx, linked.ID, "", 10)
	if err != nil || page.Patterns[0].SupportEpisodes != 2 {
		t.Fatal("explicit independent task failed to add support", page, err)
	}
	foreign := workspaceLearningSession(t, s, t.TempDir())
	foreignControl := workspaceLearningConsent(t, s, foreign, true)
	request.ExpectedEpoch = foreignControl.Epoch
	if _, err = s.Store.Learning.LinkEpisodeLineage(ctx, foreign.ID, request); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("lineage crossed workspace", err)
	}
}

func TestSkillLearningWorkspaceRevocationCleansTransitiveDerivedTextAndFencesWorker(t *testing.T) {
	for _, stage := range []string{"consolidation", "matching"} {
		t.Run(stage, func(t *testing.T) {
			s, x, model, matcher := newWorkspaceLearningTest(t, stage == "matching")
			ctx := context.Background()
			source := workspaceLearningConsent(t, s, x, true)
			consolidateExtract(t, s, x, "Revocable task evidence")
			consolidateRun(t, s)
			y := workspaceLearningSession(t, s, x.CWD)
			workspaceLearningConsent(t, s, y, true)
			consolidateExtract(t, s, y, "Retained independent task")
			var job string
			revoke := func() {
				shared := false
				if _, err := s.Store.Learning.SetControl(ctx, x.ID, harness.SkillLearningControlRequest{Contribute: true, ExpectedEpoch: source.Epoch, WorkspaceShared: &shared}); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "consolidation" {
				consolidateQueued(t, s)
				if err := s.Store.db.QueryRow(`SELECT job_id FROM df_learning_consolidations WHERE session_id=?`, y.ID).Scan(&job); err != nil {
					t.Fatal(err)
				}
				model.during = revoke
				if _, err := s.Store.Learning.RunOne(ctx); err != nil && !errors.Is(err, skillmaintenance.ErrConflict) {
					t.Fatal(err)
				}
			} else {
				consolidateRun(t, s)
				matchingQueue(t, s)
				matchingQueue(t, s)
				job, _ = matchingJob(t, s)
				matcher.during = revoke
				if worked, err := s.Store.Learning.RunOne(ctx); !worked || err != nil && !errors.Is(err, skillmaintenance.ErrConflict) {
					t.Fatal("matching did not reach revocation fence", worked, err)
				}
			}
			var state string
			if err := s.Store.db.QueryRow(`SELECT state FROM df_skill_maintenance_jobs WHERE id=?`, job).Scan(&state); err != nil || state != "cancelled" {
				t.Fatal("stale worker retained authority", state, err)
			}
			for _, table := range []string{"df_learning_patterns", "df_learning_pattern_revisions", "df_learning_pattern_members", "df_learning_consolidations", "df_learning_matches", "df_learning_consolidation_sources", "df_learning_consolidation_patterns"} {
				var count int
				if err := s.Store.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
					t.Fatal("revoked text remains in "+table, count, err)
				}
			}
			experiences, err := s.Store.Learning.Experiences(ctx, y.ID, "", 10)
			if err != nil || len(experiences.Experiences) != 1 {
				t.Fatal("revocation erased unrelated retained source", experiences, err)
			}
			var calls int
			if err = s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_maintenance_calls`).Scan(&calls); err != nil || calls < 4 {
				t.Fatal("revocation refunded maintenance accounting", calls, err)
			}
		})
	}
}

func TestSkillLearningWorkspaceSavedMergeMatchingAndRestartPolicy(t *testing.T) {
	s, x, model, matcher := newWorkspaceLearningTest(t, true)
	ctx := context.Background()
	workspaceLearningConsent(t, s, x, true)
	consolidateExtract(t, s, x, "First configuration procedure")
	consolidateRun(t, s)
	y := workspaceLearningSession(t, s, x.CWD)
	workspaceLearningConsent(t, s, y, true)
	consolidateExtract(t, s, y, "Second configuration procedure")
	consolidateRun(t, s)
	var mergeJob string
	if err := s.Store.db.QueryRow(`SELECT job_id FROM df_learning_consolidations WHERE session_id=?`, y.ID).Scan(&mergeJob); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.db.Exec(`UPDATE df_skill_maintenance_jobs SET state='queued',token='',lease_ns=0,retry_ns=0 WHERE id=?`, mergeJob); err != nil {
		t.Fatal(err)
	}
	if worked, err := s.Store.Learning.RunOne(ctx); err != nil || !worked || len(model.seen) != 2 {
		t.Fatal("shared saved merge did not recover", worked, err)
	}
	matchingQueue(t, s)
	matchingQueue(t, s)
	matchingRun(t, s)
	page, err := s.Store.Learning.WorkspaceMatches(ctx, y.ID, "", 10)
	if err != nil || len(page.Matches) != 1 || page.Matches[0].PatternRevision != 2 || page.Matches[0].Target == nil || page.Matches[0].Quality != "unassessed" || page.Matches[0].SourceState != "not_revalidated" || len(matcher.seen) != 1 {
		t.Fatal("workspace matching lost fixed target", page, err)
	}
	ordinary, err := s.SkillExperienceMatches(ctx, "owner", x.ID, "", 10)
	if err != nil || len(ordinary.Matches) != 0 {
		t.Fatal("workspace matching leaked through single-session query", ordinary, err)
	}
	_ = s.Store.Learning.Close()
	options := consolidationTestOptions()
	l, err := learning.New(ctx, s.Store.db, options)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err = l.WorkspacePatterns(ctx, y.ID, "", 10); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("disabled host still grants workspace access", err)
	}
	var raw []byte
	if err = s.Store.db.QueryRow(`SELECT payload FROM df_learning_pattern_revisions ORDER BY revision DESC LIMIT 1`).Scan(&raw); err != nil || !json.Valid(raw) {
		t.Fatal("host disable destroyed saved lawful history", err)
	}
}

func TestSkillLearningWorkspaceAuthorizationBeforeCallAndSourceWorkspaceGuard(t *testing.T) {
	for _, change := range []string{"revoke-before-call", "workspace-after-call", "source-before-call"} {
		t.Run(change, func(t *testing.T) {
			s, x, model, _ := newWorkspaceLearningTest(t, false)
			ctx := context.Background()
			control := workspaceLearningConsent(t, s, x, true)
			req := consolidateExtract(t, s, x, "Source task")
			consolidateRun(t, s)
			y := workspaceLearningSession(t, s, x.CWD)
			workspaceLearningConsent(t, s, y, true)
			consolidateExtract(t, s, y, "Candidate independent task")
			consolidateQueued(t, s)
			switch change {
			case "revoke-before-call":
				if _, err := s.Store.Learning.SetControl(ctx, x.ID, harness.SkillLearningControlRequest{ExpectedEpoch: control.Epoch}); err != nil {
					t.Fatal(err)
				}
			case "source-before-call":
				if _, err := s.Store.db.Exec(`UPDATE harness_inputs SET content='[]' WHERE id=?`, req.InputID); err != nil {
					t.Fatal(err)
				}
			case "workspace-after-call":
				model.during = func() {
					if _, err := s.Store.db.Exec(`UPDATE harness_sessions SET cwd=? WHERE id=?`, t.TempDir(), x.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := s.Store.Learning.RunOne(ctx); err != nil {
				t.Fatal(err)
			}
			var committed int
			if err := s.Store.db.QueryRow(`SELECT count(*) FROM df_learning_consolidations WHERE session_id=? AND result_sha<>''`, y.ID).Scan(&committed); err != nil || committed != 0 {
				t.Fatal("changed source accepted output", committed, err)
			}
			if change != "workspace-after-call" && len(model.seen) != 1 {
				t.Fatal("revoked or changed source reached model")
			}
		})
	}
}

func TestSkillLearningWorkspaceScopeSerializesConsolidationAndPinsProvenance(t *testing.T) {
	s, x, _, _ := newWorkspaceLearningTest(t, false)
	ctx := context.Background()
	workspaceLearningConsent(t, s, x, true)
	consolidateExtract(t, s, x, "Initial shared procedure")
	consolidateRun(t, s)
	y := workspaceLearningSession(t, s, x.CWD)
	workspaceLearningConsent(t, s, y, true)
	consolidateExtract(t, s, y, "Concurrent independent procedure")
	z := workspaceLearningSession(t, s, x.CWD)
	workspaceLearningConsent(t, s, z, true)
	consolidateExtract(t, s, z, "Third independent procedure")
	consolidateQueued(t, s)
	for range 2 {
		if worked, err := s.Store.Learning.QueueConsolidationOne(ctx); err != nil || worked {
			t.Fatal("same workspace queued competing consolidation", worked, err)
		}
	}
	var job string
	if err := s.Store.db.QueryRow(`SELECT c.job_id FROM df_learning_consolidations c JOIN df_skill_maintenance_jobs j ON j.id=c.job_id WHERE j.state='queued'`).Scan(&job); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.db.Exec(`DELETE FROM df_learning_consolidation_sources WHERE job_id=? AND session_id=?`, job, x.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.Learning.RunOne(ctx); err != nil {
		t.Fatal(err)
	}
	var committed int
	if err := s.Store.db.QueryRow(`SELECT count(*) FROM df_learning_consolidations WHERE job_id=? AND result_sha<>''`, job).Scan(&committed); err != nil || committed != 0 {
		t.Fatal("incomplete revocation dependencies admitted output", committed, err)
	}
}

func TestSkillLearningWorkspaceRevocationCleansDistinctShortlistOutputs(t *testing.T) {
	s, x, model, _ := newWorkspaceLearningTest(t, false)
	ctx := context.Background()
	control := workspaceLearningConsent(t, s, x, true)
	consolidateExtract(t, s, x, "Initial evidence")
	consolidateRun(t, s)
	model.relation = "distinct"
	y := workspaceLearningSession(t, s, x.CWD)
	workspaceLearningConsent(t, s, y, true)
	consolidateExtract(t, s, y, "Distinct recipe compared with initial evidence")
	consolidateRun(t, s)
	z := workspaceLearningSession(t, s, x.CWD)
	workspaceLearningConsent(t, s, z, true)
	consolidateExtract(t, s, z, "Third recipe compared with both prior candidates")
	consolidateRun(t, s)
	if len(model.seen[2].Patterns) != 2 {
		t.Fatal("fixture did not establish transitive shortlist provenance")
	}
	if _, err := s.Store.Learning.SetControl(ctx, x.ID, harness.SkillLearningControlRequest{ExpectedEpoch: control.Epoch}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"df_learning_patterns", "df_learning_consolidations"} {
		var count int
		if err := s.Store.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("unselected compared text survived revocation", table, count, err)
		}
	}
}

func TestSkillLearningWorkspaceRestartDisableCancelsSharedClaimsWithoutErasingHistory(t *testing.T) {
	s, x, _, _ := newWorkspaceLearningTest(t, true)
	ctx := context.Background()
	workspaceLearningConsent(t, s, x, true)
	consolidateExtract(t, s, x, "Completed shared configuration")
	consolidateRun(t, s)
	matchingQueue(t, s)
	match, _ := matchingJob(t, s)
	y := workspaceLearningSession(t, s, x.CWD)
	workspaceLearningConsent(t, s, y, true)
	matchingRun(t, s)
	consolidateExtract(t, s, y, "Second shared configuration")
	consolidateQueued(t, s)
	var consolidation string
	if err := s.Store.db.QueryRow(`SELECT job_id FROM df_learning_consolidations WHERE session_id=?`, y.ID).Scan(&consolidation); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.db.Exec(`UPDATE df_skill_maintenance_jobs SET state='queued',token='',lease_ns=0,retry_ns=0 WHERE id=?`, match); err != nil {
		t.Fatal(err)
	}
	_ = s.Store.Learning.Close()
	options := consolidationTestOptions()
	options.Matching = &harness.SkillGenerationModelConfig{}
	l, err := learning.New(ctx, s.Store.db, options)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, job := range []string{match, consolidation} {
		var state string
		if err = s.Store.db.QueryRow(`SELECT state FROM df_skill_maintenance_jobs WHERE id=?`, job).Scan(&state); err != nil || state != "cancelled" {
			t.Fatal("disabled cross-session claim survived restart", state, err)
		}
	}
	var count int
	if err = s.Store.db.QueryRow(`SELECT count(*) FROM df_learning_patterns`).Scan(&count); err != nil || count != 1 {
		t.Fatal("host gate erased lawful shared history", count, err)
	}
	if _, err = l.WorkspaceMatches(ctx, x.ID, "", 10); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("disabled cross-session gate still grants text", err)
	}
}

func TestSkillLearningWorkspaceSessionDeletionCleansForeignDerivedRows(t *testing.T) {
	s, x, _, _ := newWorkspaceLearningTest(t, true)
	ctx := context.Background()
	workspaceLearningConsent(t, s, x, true)
	consolidateExtract(t, s, x, "Source session to delete")
	consolidateRun(t, s)
	y := workspaceLearningSession(t, s, x.CWD)
	workspaceLearningConsent(t, s, y, true)
	consolidateExtract(t, s, y, "Retained source session")
	consolidateRun(t, s)
	matchingQueue(t, s)
	matchingQueue(t, s)
	job, _ := matchingJob(t, s)
	if err := s.Coordinator.Detach(ctx, x.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if already, err := s.DeleteSession(ctx, x.ID); err != nil || already {
		t.Fatal("shared source purge failed", already, err)
	}
	var state string
	if err := s.Store.db.QueryRow(`SELECT state FROM df_skill_maintenance_jobs WHERE id=?`, job).Scan(&state); err != nil || state != "cancelled" {
		t.Fatal("shared source deletion left a match claim", state, err)
	}
	page, err := s.Store.Learning.WorkspacePatterns(ctx, y.ID, "", 10)
	if err != nil || len(page.Patterns) != 0 {
		t.Fatal("deleted source left foreign shared text", page, err)
	}
	experiences, err := s.Store.Learning.Experiences(ctx, y.ID, "", 10)
	if err != nil || len(experiences.Experiences) != 1 {
		t.Fatal("shared purge deleted unrelated experiences", experiences, err)
	}
}
