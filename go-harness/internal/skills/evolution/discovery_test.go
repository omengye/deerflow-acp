package evolution

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func discoveryFixture(t *testing.T) (*collectorFixture, *fixtureGenerator, *fixtureGenerationValidator) {
	t.Helper()
	f := newCollectorFixture(t)
	v := &fixtureGenerationValidator{id: "discovery-review-v1"}
	f.service.validator = v
	g := &fixtureGenerator{id: "discovery-generation-v1"}
	configureGeneration(t, f.service, g)
	if err := f.service.ConfigureDiscovery(discoveryOptions()); err != nil {
		t.Fatal(err)
	}
	return f, g, v
}
func discoveryOptions() harness.SkillDiscoveryOptions {
	return harness.SkillDiscoveryOptions{RepeatThreshold: 2, RepeatWindowDays: 30, CooldownHours: 24, MaxDailyProposals: 5, MaxPendingProposals: 20}
}

func discoveryCorrection(t *testing.T, f *collectorFixture, root string, key string) harness.SkillFeedback {
	t.Helper()
	run := f.run(t, root, 1, "completed", false)
	f.drain(t, 128)
	return mustSubmitFeedback(t, f.service, feedbackRequest(feedbackEvidence(t, f, run), key)).Feedback
}

func discoverySteps(t *testing.T, s *Service, n int) {
	t.Helper()
	for range n {
		if _, err := s.RunDiscovery(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}
func discoveries(t *testing.T, s *Service) []harness.SkillDiscovery {
	t.Helper()
	page, err := s.QueryDiscoveries(context.Background(), harness.SkillQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return page.Discoveries
}
func withdrawDiscoveryFeedback(t *testing.T, s *Service, id string) {
	t.Helper()
	snap, err := s.Feedback(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	mustSubmitFeedback(t, s, harness.SkillFeedbackRequest{SignalID: id, ExpectedSignalStateVersion: snap.Signal.StateVersion, ExpectedFeedbackVersion: snap.Signal.FeedbackRevision, RequestKey: "withdraw:" + id, Kind: "withdrawn", ReadIDs: []int64{}})
}

func TestDiscoveryDistinctRootsAndActualGenerationEvidence(t *testing.T) {
	f, g, v := discoveryFixture(t)
	ctx := context.Background()
	first := discoveryCorrection(t, f, "", "first")
	for i, key := range []string{"revision-two", "revision-three"} {
		mustSubmitFeedback(t, f.service, harness.SkillFeedbackRequest{SignalID: first.SignalID, ExpectedSignalStateVersion: first.SignalStateVersion, ExpectedFeedbackVersion: int64(i + 1), RequestKey: key, Kind: "correction", Summary: "Add verification; token=supersecret", ReadIDs: first.ReadIDs})
	}
	discoverySteps(t, f.service, 2)
	if got := discoveries(t, f.service); len(got) != 0 {
		t.Fatal("revisions counted as distinct roots", got)
	}
	second := discoveryCorrection(t, f, "", "second")
	before := f.head(t)
	g.body = func(_ context.Context, input skillmaintenance.GenerationInput) (skillmaintenance.Candidate, error) {
		if len(input.Evidence) != 2 || input.Target.Name != "research" || input.Base != before {
			t.Fatalf("generation input %+v", input)
		}
		for _, e := range input.Evidence {
			if !strings.Contains(e.Summary, "causality unassessed") || strings.Contains(e.Summary, "supersecret") {
				t.Fatalf("bad evidence %+v", e)
			}
		}
		return skillmaintenance.Candidate{Reason: "Validate source evidence", Files: skillFiles("Verify sources before summarizing")}, nil
	}
	discoverySteps(t, f.service, 3)
	ds := discoveries(t, f.service)
	if len(ds) != 1 || ds[0].Attribution != "unassessed" || ds[0].Kind != "operator_corrections" || len(ds[0].Evidence) != 2 || ds[0].Evidence[0].RootID == ds[0].Evidence[1].RootID {
		t.Fatalf("discoveries %+v", ds)
	}
	var raw []byte
	if err := f.db.DB().QueryRow(`SELECT payload FROM df_skill_discoveries WHERE id=?`, ds[0].ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "Add verification") || strings.Contains(string(raw), "supersecret") {
		t.Fatalf("snapshot copied summary: %s", raw)
	}
	generationRun(t, f.service)
	job, err := f.service.GetGeneration(ctx, ds[0].GenerationID)
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.service.Get(ctx, job.ProposalID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "completed" || job.DiscoveryID != ds[0].ID || p.Status != "pending_review" || p.Trigger.Type != "operator_corrections" || !strings.Contains(p.Trigger.Summary, ds[0].ID) || f.head(t) != before || g.calls.Load() != 1 || v.calls.Load() != 1 {
		t.Fatalf("job=%+v proposal=%+v", job, p)
	}
	receipts, err := f.service.GenerationReceipts(ctx, job.ID, 0, 100)
	if err != nil || len(receipts) != 3 {
		t.Fatalf("receipts %+v %v", receipts, err)
	}
	for _, signal := range []string{first.SignalID, second.SignalID} {
		snap, err := f.service.Feedback(ctx, signal)
		if err != nil || snap.Signal.Attribution != "unassessed" {
			t.Fatalf("attribution %+v %v", snap, err)
		}
	}
	discoverySteps(t, f.service, 2)
	if len(discoveries(t, f.service)) != 1 {
		t.Fatal("requeued reserved evidence")
	}
}

func TestDiscoveryWithdrawalFencesCallsCandidateAndApproval(t *testing.T) {
	for _, stage := range []string{"before_call", "during_generation", "during_review", "before_approval"} {
		t.Run(stage, func(t *testing.T) {
			f, g, v := discoveryFixture(t)
			ctx := context.Background()
			first := discoveryCorrection(t, f, "", "one")
			discoveryCorrection(t, f, "", "two")
			discoverySteps(t, f.service, 2)
			d := discoveries(t, f.service)[0]
			before := f.head(t)
			if stage == "before_call" {
				withdrawDiscoveryFeedback(t, f.service, first.SignalID)
			}
			if stage == "during_generation" {
				g.body = func(_ context.Context, _ skillmaintenance.GenerationInput) (skillmaintenance.Candidate, error) {
					withdrawDiscoveryFeedback(t, f.service, first.SignalID)
					return skillmaintenance.Candidate{Reason: "Revised", Files: skillFiles("New instructions")}, nil
				}
			}
			if stage == "during_review" {
				v.before = func(context.Context) { withdrawDiscoveryFeedback(t, f.service, first.SignalID) }
			}
			generationRun(t, f.service)
			job, err := f.service.GetGeneration(ctx, d.GenerationID)
			if err != nil {
				t.Fatal(err)
			}
			if stage == "before_approval" {
				p, err := f.service.Get(ctx, job.ProposalID)
				if err != nil {
					t.Fatal(err)
				}
				withdrawDiscoveryFeedback(t, f.service, first.SignalID)
				if _, err = f.service.Approve(ctx, p.ID, Review{ExpectedStateVersion: p.StateVersion, Actor: "test"}); !errors.Is(err, harness.ErrSkillConflict) {
					t.Fatalf("withdrawn approved: %v", err)
				}
				stale, err := f.service.Get(ctx, p.ID)
				if err != nil || stale.Status != "stale" || stale.StateVersion != p.StateVersion+1 {
					t.Fatalf("stale evidence not terminalized %+v %v", stale, err)
				}
				status, err := f.service.DiscoveryStatus(ctx)
				if err != nil || status.Pending != 0 {
					t.Fatalf("stale evidence retained capacity %+v %v", status, err)
				}
			} else if stage == "during_review" {
				p, err := f.service.Get(ctx, job.ProposalID)
				if err != nil || p.Status != "failed" || job.State != "failed" || job.ErrorCode != "evidence_changed" {
					t.Fatalf("invalidated review %+v %+v %v", job, p, err)
				}
				receipts, err := f.service.GenerationReceipts(ctx, job.ID, 0, 100)
				if err != nil || len(receipts) != 1 || receipts[0].Stage != "generation" {
					t.Fatalf("withdrawn review spent budget %+v %v", receipts, err)
				}
			} else if job.State != "failed" || job.ErrorCode != "evidence_changed" || job.CandidateVersion != "" || job.ProposalID != "" {
				t.Fatalf("job %+v", job)
			}
			if stage == "before_call" && g.calls.Load() != 0 {
				t.Fatal("withdrawn evidence incurred call")
			}
			if stage == "during_generation" && g.calls.Load() != 1 {
				t.Fatal("generation not exercised")
			}
			if f.head(t) != before {
				t.Fatal("head changed")
			}
			status, err := f.service.DiscoveryStatus(ctx)
			if err != nil || status.DailyReservations != 1 {
				t.Fatalf("refunded %+v %v", status, err)
			}
		})
	}
}

func TestDiscoveryConcurrentReservationRestartAndLifetimeRootDedup(t *testing.T) {
	f, g, v := discoveryFixture(t)
	ctx := context.Background()
	first := discoveryCorrection(t, f, "", "one")
	discoveryCorrection(t, f, "", "two")
	var dbPath string
	if err := f.db.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&dbPath); err != nil {
		t.Fatal(err)
	}
	otherDB, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer otherDB.Close()
	otherRegistry, err := skills.NewRegistry(ctx, f.cfg, otherDB.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer otherRegistry.Close()
	other, err := New(ctx, otherRegistry, v)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	configureGeneration(t, other, g)
	if err = other.ConfigureDiscovery(discoveryOptions()); err != nil {
		t.Fatal(err)
	}
	barrier := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, service := range []*Service{f.service, other} {
		wg.Go(func() {
			<-barrier
			for range 2 {
				if _, err := service.RunDiscovery(ctx); err != nil {
					results <- err
					return
				}
			}
			results <- nil
		})
	}
	close(barrier)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	ds := discoveries(t, f.service)
	if len(ds) != 1 {
		t.Fatalf("concurrent reservations %+v", ds)
	}
	job, err := other.GetGeneration(ctx, ds[0].GenerationID)
	if err != nil {
		t.Fatal(err)
	}
	if err = other.CancelGeneration(ctx, job.ID, job.StateVersion); err != nil {
		t.Fatal(err)
	}
	// Same DB, new service, unchanged source grants; cancelled choices retain
	// their consumed roots and their original daily/cooldown reservation.
	restarted, err := New(ctx, f.registry, v)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	configureGeneration(t, restarted, g)
	if err = restarted.ConfigureDiscovery(discoveryOptions()); err != nil {
		t.Fatal(err)
	}
	restarted.discovery.now = func() time.Time { return time.Now().UTC().Add(25 * time.Hour) }
	snap, err := restarted.Feedback(ctx, first.SignalID)
	if err != nil {
		t.Fatal(err)
	}
	mustSubmitFeedback(t, restarted, harness.SkillFeedbackRequest{SignalID: first.SignalID, ExpectedSignalStateVersion: snap.Signal.StateVersion, ExpectedFeedbackVersion: snap.Signal.FeedbackRevision, RequestKey: "later-revision", Kind: "correction", Summary: "A new wording of the same root report", ReadIDs: first.ReadIDs})
	discoverySteps(t, restarted, 3)
	if len(discoveries(t, restarted)) != 1 {
		t.Fatal("revision or restart reset root consumption")
	}
	var count int
	if err = f.db.DB().QueryRow(`SELECT sum(reservations) FROM df_skill_discovery_days`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("daily count %d %v", count, err)
	}
}

func TestDiscoveryCooldownDailyAndPendingCaps(t *testing.T) {
	f, _, _ := discoveryFixture(t)
	ctx := context.Background()
	f.service.discovery.options = harness.SkillDiscoveryOptions{RepeatThreshold: 2, RepeatWindowDays: 30, CooldownHours: 1, MaxDailyProposals: 2, MaxPendingProposals: 1}
	now := time.Now().UTC().Add(24 * time.Hour).Truncate(24 * time.Hour).Add(2 * time.Hour)
	f.service.discovery.now = func() time.Time { return now }
	add := func(prefix string) {
		discoveryCorrection(t, f, "", prefix+"1")
		discoveryCorrection(t, f, "", prefix+"2")
		discoverySteps(t, f.service, 3)
	}
	cancel := func() {
		ds := discoveries(t, f.service)
		for _, d := range ds {
			j, err := f.service.GetGeneration(ctx, d.GenerationID)
			if err != nil {
				t.Fatal(err)
			}
			if j.State == "queued" {
				if err = f.service.CancelGeneration(ctx, j.ID, j.StateVersion); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	add("first")
	if len(discoveries(t, f.service)) != 1 {
		t.Fatal("first missing")
	}
	now = now.Add(61 * time.Minute)
	add("second")
	if len(discoveries(t, f.service)) != 1 {
		t.Fatal("pending cap bypass")
	}
	cancel()
	discoverySteps(t, f.service, 3)
	if len(discoveries(t, f.service)) != 2 {
		t.Fatal("freed pending capacity not used")
	}
	cancel()
	now = now.Add(30 * time.Minute)
	add("third")
	if len(discoveries(t, f.service)) != 2 {
		t.Fatal("cooldown bypass")
	}
	now = now.Add(31 * time.Minute)
	discoverySteps(t, f.service, 3)
	if len(discoveries(t, f.service)) != 2 {
		t.Fatal("daily cap bypass")
	}
	now = now.Add(24 * time.Hour)
	discoverySteps(t, f.service, 3)
	if len(discoveries(t, f.service)) != 3 {
		t.Fatal("new day did not release daily cap")
	}
}

func TestDiscoveryCorruptEvidenceDoesNotStarveHealthySignals(t *testing.T) {
	f, _, _ := discoveryFixture(t)
	ctx := context.Background()
	broken := discoveryCorrection(t, f, "", "broken")
	discoveryCorrection(t, f, "", "healthy1")
	discoveryCorrection(t, f, "", "healthy2")
	if _, err := f.db.DB().Exec(`UPDATE df_skill_feedback SET payload_sha=? WHERE signal_id=?`, strings.Repeat("0", 64), broken.SignalID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	f.service.discovery.now = func() time.Time { return now }
	for range 7 {
		_, _ = f.service.RunDiscovery(ctx)
		now = now.Add(6 * time.Second)
	}
	status, err := f.service.DiscoveryStatus(ctx)
	if err != nil || status.FailedSignals != 1 || status.LastErrorCode != "invalid_evidence" || len(discoveries(t, f.service)) != 1 {
		t.Fatalf("status %+v %v", status, err)
	}
	var attempts int
	if err = f.db.DB().QueryRow(`SELECT attempts FROM df_skill_discovery_errors WHERE signal_id=?`, broken.SignalID).Scan(&attempts); err != nil || attempts != 3 {
		t.Fatalf("attempts %d %v", attempts, err)
	}
	for range 3 {
		_, _ = f.service.RunDiscovery(ctx)
	}
	if err = f.db.DB().QueryRow(`SELECT attempts FROM df_skill_discovery_errors WHERE signal_id=?`, broken.SignalID).Scan(&attempts); err != nil || attempts != 3 {
		t.Fatal("unbounded poison retry", attempts, err)
	}
}

func TestDiscoveryQueryCanonicalCursorAndDisabledHistory(t *testing.T) {
	f, _, v := discoveryFixture(t)
	ctx := context.Background()
	discoveryCorrection(t, f, "", "one")
	discoveryCorrection(t, f, "", "two")
	discoverySteps(t, f.service, 2)
	disabled, err := New(ctx, f.registry, v)
	if err != nil {
		t.Fatal(err)
	}
	defer disabled.Close()
	status, err := disabled.DiscoveryStatus(ctx)
	if err != nil || status.Enabled || status.Running || len(discoveries(t, disabled)) != 1 {
		t.Fatalf("disabled status %+v %v", status, err)
	}
	if _, err = disabled.RunDiscovery(ctx); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatal(err)
	}
	_, _, _, cursor, err := disabled.querySources(harness.SkillQuery{Limit: 1}, "skill-discovery-v1")
	if err != nil {
		t.Fatal(err)
	}
	cursor.Key = discoveries(t, disabled)[0].ID
	for _, bad := range []string{encodeCursor(pageCursor{Kind: cursor.Kind, Filter: cursor.Filter, Key: cursor.Key, Revision: 1}), encodeCursor(pageCursor{Kind: cursor.Kind, Filter: cursor.Filter, Key: "sd_missing"})} {
		if _, err = disabled.QueryDiscoveries(ctx, harness.SkillQuery{Limit: 1, Cursor: bad}); err == nil {
			t.Fatal("bad cursor accepted", bad)
		}
	}
	if _, err = disabled.QueryDiscoveries(ctx, harness.SkillQuery{Limit: 1, Cursor: encodeCursor(cursor)}); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(cursor)
	if strings.Contains(string(payload), "Summary") {
		t.Fatal("cursor exposes summaries")
	}
}

func TestDiscoveryDisabledAfterReviewAllowsExplicitApprovalAndTerminalAck(t *testing.T) {
	f, g, v := discoveryFixture(t)
	ctx := context.Background()
	discoveryCorrection(t, f, "", "one")
	discoveryCorrection(t, f, "", "two")
	discoverySteps(t, f.service, 2)
	d := discoveries(t, f.service)[0]
	generationRun(t, f.service)
	job, err := f.service.GetGeneration(ctx, d.GenerationID)
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.service.Get(ctx, job.ProposalID)
	if err != nil {
		t.Fatal(err)
	}
	disabled, err := New(ctx, f.registry, v)
	if err != nil {
		t.Fatal(err)
	}
	defer disabled.Close()
	approved, err := disabled.Approve(ctx, p.ID, Review{ExpectedStateVersion: p.StateVersion, Actor: "operator"})
	if err != nil || approved.Status != "published" {
		t.Fatalf("explicit approval disabled discovery %+v %v", approved, err)
	}
	// Simulate a process losing its final job acknowledgement after the durable
	// review. Recovery sees the published result and spends no new model calls,
	// even after the original report has been withdrawn and opt-in disabled.
	withdrawDiscoveryFeedback(t, f.service, d.Evidence[0].SignalID)
	if _, err = f.db.DB().Exec(`UPDATE df_skill_maintenance_jobs SET state='retry',state_version=state_version+1,deadline_ns=0,lease_ns=0,token='' WHERE id=?`, job.ID); err != nil {
		t.Fatal(err)
	}
	configureGeneration(t, disabled, g)
	before := g.calls.Load()
	generationRun(t, disabled)
	saved, err := disabled.GetGeneration(ctx, job.ID)
	if err != nil || saved.State != "completed" || g.calls.Load() != before {
		t.Fatalf("terminal acknowledgement %+v %v", saved, err)
	}
}

func TestDiscoveryExcludesCancelledUncertainUnsettledAndExpiredReports(t *testing.T) {
	for _, scenario := range []string{"cancelled", "uncertain", "collector_pending", "expired", "disabled_skill", "missing_graph"} {
		t.Run(scenario, func(t *testing.T) {
			f, g, _ := discoveryFixture(t)
			ctx := context.Background()
			discoveryCorrection(t, f, "", "healthy")
			status := "completed"
			uncertain := false
			if scenario == "cancelled" {
				status = "cancelled"
			}
			if scenario == "uncertain" {
				status = "failed"
				uncertain = true
			}
			run := f.run(t, "", 1, status, uncertain)
			f.drain(t, 128)
			request := feedbackRequest(feedbackEvidence(t, f, run), "second")
			mustSubmitFeedback(t, f.service, request)
			switch scenario {
			case "collector_pending":
				// A later child exists but its terminal outbox has not yet been
				// collected. The earlier correction must not be selected.
				f.run(t, run.RootBudgetID, 1, "completed", false)
			case "expired":
				f.service.discovery.now = func() time.Time { return time.Now().UTC().Add(31 * 24 * time.Hour) }
			case "disabled_skill":
				if err := f.registry.SetEnabled(ctx, "project", "research", false); err != nil {
					t.Fatal(err)
				}
			case "missing_graph":
				if _, err := f.db.DB().Exec(`UPDATE budget_members SET origin_id='invalid-root-origin' WHERE id=?`, run.RunID); err != nil {
					t.Fatal(err)
				}
			}
			for range 3 {
				_, _ = f.service.RunDiscovery(ctx)
			}
			if len(discoveries(t, f.service)) != 0 || g.calls.Load() != 0 {
				t.Fatal("ineligible evidence selected")
			}
		})
	}
}

func TestDiscoveryRevisionAndDeletionInvalidateReservedEvidence(t *testing.T) {
	for _, scenario := range []string{"revision", "deletion", "config"} {
		t.Run(scenario, func(t *testing.T) {
			f, g, _ := discoveryFixture(t)
			ctx := context.Background()
			first := discoveryCorrection(t, f, "", "one")
			discoveryCorrection(t, f, "", "two")
			discoverySteps(t, f.service, 2)
			d := discoveries(t, f.service)[0]
			switch scenario {
			case "revision":
				mustSubmitFeedback(t, f.service, harness.SkillFeedbackRequest{SignalID: first.SignalID, ExpectedSignalStateVersion: first.SignalStateVersion, ExpectedFeedbackVersion: first.Revision, RequestKey: "revision", Kind: "correction", Summary: "Replacement observation", ReadIDs: first.ReadIDs})
			case "deletion":
				if _, err := hr.NewService(f.runtime, nil, "fixture").DeleteSession(ctx, first.RootSessionID); err != nil {
					t.Fatal(err)
				}
			case "config":
				f.service.discovery.options.CooldownHours++
			}
			generationRun(t, f.service)
			job, err := f.service.GetGeneration(ctx, d.GenerationID)
			if err != nil || job.State != "failed" || g.calls.Load() != 0 {
				t.Fatalf("stale evidence %+v %v", job, err)
			}
			want := "evidence_changed"
			if scenario == "config" {
				want = "configuration_changed"
			}
			if job.ErrorCode != want {
				t.Fatalf("code %s want %s", job.ErrorCode, want)
			}
		})
	}
}

func TestDiscoveryQueueBackpressureAndTransientFailuresRemainRetryable(t *testing.T) {
	f, _, _ := discoveryFixture(t)
	ctx := context.Background()
	f.service.discovery.options.MaxPendingProposals = 1000
	discoveryCorrection(t, f, "", "one")
	discoveryCorrection(t, f, "", "two")
	var jobs []skillmaintenance.Job
	for i := range 100 {
		job, err := f.service.generation.store.Enqueue(ctx, skillmaintenance.Request{SourceIdentity: f.read.Ref.SourceIdentity, Name: "other" + strconv.Itoa(i), Key: "fixture", Reference: "fixture", InputDigest: strings.Repeat("a", 64)})
		if err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, job)
	}
	discoverySteps(t, f.service, 8)
	status, err := f.service.DiscoveryStatus(ctx)
	if err != nil || status.DailyReservations != 0 || status.FailedSignals != 0 || status.LastErrorCode != "" {
		t.Fatalf("capacity quarantined evidence %+v %v", status, err)
	}
	if err = f.service.generation.store.Cancel(ctx, jobs[0].ID, jobs[0].StateVersion); err != nil {
		t.Fatal(err)
	}
	// Repeated transient errors are persisted with backoff, without using the
	// poison-evidence terminal cap. Current healthy evidence must recover.
	items, err := f.service.discoveryScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) == 0 {
		items, err = f.service.discoveryScan(ctx)
	}
	if err != nil || len(items) == 0 {
		t.Fatal("no items", err)
	}
	now := time.Now().UTC()
	f.service.discovery.now = func() time.Time { return now }
	for range 4 {
		if err = f.service.discoveryRecordError(ctx, items[0], errors.New("transient")); err != nil {
			t.Fatal(err)
		}
		now = now.Add(6 * time.Second)
	}
	discoverySteps(t, f.service, 4)
	if len(discoveries(t, f.service)) != 1 {
		t.Fatal("capacity/transient recovery lost eligible evidence")
	}
	status, err = f.service.DiscoveryStatus(ctx)
	if err != nil || status.FailedSignals != 0 {
		t.Fatalf("transient quarantined %+v %v", status, err)
	}
}

func TestDiscoveryScanRetainsUnattemptedTailAcrossRestart(t *testing.T) {
	f, g, v := discoveryFixture(t)
	ctx := context.Background()
	discoveryCorrection(t, f, "", "one")
	discoveryCorrection(t, f, "", "two")
	discoveryCorrection(t, f, "", "three")
	items, err := f.service.discoveryScan(ctx)
	if err != nil || len(items) != 3 {
		t.Fatalf("scan %+v %v", items, err)
	}
	var cursor string
	if err = f.db.DB().QueryRow(`SELECT cursor FROM df_skill_discovery_scan WHERE id=1`).Scan(&cursor); err != nil || cursor != "" {
		t.Fatal("scan skipped unattempted page", cursor, err)
	}
	if _, err = f.service.discoveryConsider(ctx, items[0]); err != nil {
		t.Fatal(err)
	}
	interrupted, cancel := context.WithCancel(ctx)
	cancel()
	if advanced, err := f.service.advanceDiscoveryScan(interrupted, items[0]); err != nil || !advanced {
		t.Fatal("advance", advanced, err)
	}
	restarted, err := New(ctx, f.registry, v)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	configureGeneration(t, restarted, g)
	if err = restarted.ConfigureDiscovery(discoveryOptions()); err != nil {
		t.Fatal(err)
	}
	tail, err := restarted.discoveryScan(ctx)
	if err != nil || len(tail) != 2 || tail[0].id != items[1].id {
		t.Fatalf("lost tail %+v %v", tail, err)
	}
	if advanced, err := f.service.advanceDiscoveryScan(ctx, items[0]); err != nil || advanced {
		t.Fatal("old scanner rewrote progress", advanced, err)
	}
	discoverySteps(t, restarted, 3)
	if len(discoveries(t, restarted)) != 1 {
		t.Fatal("resumed tail never selected")
	}
}
