package skillmaintenance

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

type fixture struct {
	store *Store
	db    *sqlite.Store
	now   atomic.Int64
	path  string
}

func newFixture(t *testing.T, p Policy) *fixture {
	t.Helper()
	f := &fixture{path: filepath.Join(t.TempDir(), "maintenance.db")}
	var err error
	f.db, err = sqlite.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.store, err = New(context.Background(), f.db.DB(), p)
	if err != nil {
		t.Fatal(err)
	}
	f.now.Store(time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC).UnixNano())
	f.store.now = func() time.Time { return time.Unix(0, f.now.Load()).UTC() }
	t.Cleanup(func() { _ = f.db.Close() })
	return f
}
func policy() Policy {
	return Policy{MaxDailyModelCalls: 10, MaxDailyTokens: 10000, MaxJobSeconds: 300, Workers: 2}
}
func request(key string) Request {
	return Request{SourceIdentity: strings.Repeat("a", 64), Name: "research", Key: key, Reference: "input/" + key, InputDigest: strings.Repeat("b", 64)}
}
func call(stage string) ModelCall {
	return ModelCall{Stage: stage, ModelIdentity: strings.Repeat("c", 64), InputDigest: strings.Repeat("d", 64), InputTokens: 100, MaxOutputTokens: 200}
}
func enqueue(t *testing.T, s *Store, key string) Job {
	t.Helper()
	j, e := s.Enqueue(context.Background(), request(key))
	if e != nil {
		t.Fatal(e)
	}
	return j
}
func claim(t *testing.T, s *Store) Claim {
	t.Helper()
	c, ok, e := s.Claim(context.Background())
	if e != nil || !ok {
		t.Fatalf("claim %v %v", ok, e)
	}
	return c
}
func reserve(t *testing.T, s *Store, c Claim, stage string) Receipt {
	t.Helper()
	r, e := s.Broker(c).Reserve(context.Background(), call(stage))
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func usage(t *testing.T, s *Store, calls, tokens int64) {
	t.Helper()
	u, e := s.DayUsage(context.Background(), "2026-10-02")
	if e != nil || u.ModelCalls != calls || u.ChargedTokens != tokens {
		t.Fatalf("usage %+v %v; want %d/%d", u, e, calls, tokens)
	}
}

func TestDurableMaintenanceSharedQuotaAndRestart(t *testing.T) {
	ctx := context.Background()
	p := policy()
	p.MaxDailyModelCalls = 2
	f := newFixture(t, p)
	j := enqueue(t, f.store, "one")
	if dup, e := f.store.Enqueue(ctx, j.Request); e != nil || dup.ID != j.ID {
		t.Fatalf("dedupe %+v %v", dup, e)
	}
	changed := j.Request
	changed.InputDigest = strings.Repeat("e", 64)
	if _, e := f.store.Enqueue(ctx, changed); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	c := claim(t, f.store)
	r := reserve(t, f.store, c, "generation")
	usage(t, f.store, 1, 300)
	if e := f.store.Broker(c).Settle(ctx, r, Usage{InputTokens: 20, OutputTokens: 10, Known: true}); e != nil {
		t.Fatal(e)
	}
	if e := f.store.Broker(c).Settle(ctx, r, Usage{InputTokens: 20, OutputTokens: 10, Known: true}); e != nil {
		t.Fatal(e)
	}
	if e := f.store.Broker(c).Settle(ctx, r, Usage{Known: true}); !errors.Is(e, ErrConflict) {
		t.Fatalf("conflicting settle %v", e)
	}
	second := reserve(t, f.store, c, "security")
	if e := f.store.Broker(c).Settle(ctx, second, Usage{}); e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.Broker(c).Reserve(ctx, call("quality")); !errors.Is(e, ErrBudget) {
		t.Fatal(e)
	}
	usage(t, f.store, 2, 330)
	if e := f.store.Finish(ctx, c, "model_failed", false); e != nil {
		t.Fatal(e)
	}
	// Opening another connection/service cannot reset the global daily budget.
	db, e := sqlite.Open(f.path)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	s, e := New(ctx, db.DB(), p)
	if e != nil {
		t.Fatal(e)
	}
	s.now = f.store.now
	r2 := request("two")
	r2.SourceIdentity = strings.Repeat("f", 64)
	if _, e = s.Enqueue(ctx, r2); e != nil {
		t.Fatal(e)
	}
	c2 := claim(t, s)
	if _, e = s.Broker(c2).Reserve(ctx, call("generation")); !errors.Is(e, ErrBudget) {
		t.Fatal(e)
	}
	receipts, e := s.Receipts(ctx, j.ID, 0, 1)
	if e != nil || len(receipts) != 1 || receipts[0].ChargedTokens != 30 || receipts[0].Estimated {
		t.Fatalf("receipts %+v %v", receipts, e)
	}
	page, e := s.Receipts(ctx, j.ID, receipts[0].Sequence, 100)
	if e != nil || len(page) != 1 || !page[0].Estimated || page[0].ChargedTokens != 300 {
		t.Fatalf("page %+v %v", page, e)
	}
}

func TestMaintenanceUnknownCallsFencingAndDeadline(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, policy())
	j := enqueue(t, f.store, "crash")
	old := claim(t, f.store)
	r := reserve(t, f.store, old, "generation")
	if e := f.store.Finish(ctx, old, "", false); !errors.Is(e, ErrConflict) {
		t.Fatalf("completed unsettled call: %v", e)
	}
	f.now.Add(int64(leaseDuration + time.Second))
	fresh := claim(t, f.store)
	if fresh.Job.ID != j.ID || fresh.Job.Attempts != 2 || fresh.Job.Deadline != old.Job.Deadline {
		t.Fatalf("retry reset identity/deadline: %+v", fresh.Job)
	}
	if e := f.store.Broker(old).Settle(ctx, r, Usage{Known: true}); !errors.Is(e, ErrConflict) {
		t.Fatalf("stale refund %v", e)
	}
	if e := f.store.Finish(ctx, old, "model_failed", true); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	if _, e := f.store.Broker(old).Reserve(ctx, call("quality")); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	next := reserve(t, f.store, fresh, "generation")
	if e := f.store.Broker(fresh).Settle(ctx, next, Usage{}); e != nil {
		t.Fatal(e)
	}
	usage(t, f.store, 2, 600)
	if e := f.store.Finish(ctx, fresh, "model_failed", true); e != nil {
		t.Fatal(e)
	}
	if _, ok, e := f.store.Claim(ctx); e != nil || ok {
		t.Fatalf("backoff skipped %v %v", ok, e)
	}
	f.now.Add(int64(5 * time.Second))
	third := claim(t, f.store)
	if third.Job.Attempts != 3 {
		t.Fatal(third.Job)
	}
	if e := f.store.Finish(ctx, third, "model_failed", true); e != nil {
		t.Fatal(e)
	}
	failed, e := f.store.Get(ctx, j.ID)
	if e != nil || failed.State != "failed" || failed.Attempts != 3 {
		t.Fatalf("failure %+v %v", failed, e)
	}
	if _, ok, e := f.store.Claim(ctx); e != nil || ok {
		t.Fatalf("unbounded retry %v %v", ok, e)
	}
	j2 := enqueue(t, f.store, "deadline")
	active := claim(t, f.store)
	f.now.Store(active.Job.Deadline.UnixNano())
	if _, e := f.store.Broker(active).Reserve(ctx, call("generation")); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	if e := f.store.Finish(ctx, active, "model_failed", true); e != nil {
		t.Fatal(e)
	}
	ended, e := f.store.Get(ctx, j2.ID)
	if e != nil || ended.ErrorCode != "job_deadline" || ended.State != "failed" {
		t.Fatalf("deadline %+v %v", ended, e)
	}
}

func TestMaintenanceTransactionalFaultsAndCancellation(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, policy())
	j := enqueue(t, f.store, "faults")
	c := claim(t, f.store)
	_, e := f.db.DB().Exec(`CREATE TRIGGER fail_call BEFORE INSERT ON df_skill_maintenance_calls BEGIN SELECT RAISE(ABORT,'private-storage-text'); END`)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.store.Broker(c).Reserve(ctx, call("generation")); e == nil {
		t.Fatal("expected fault")
	}
	usage(t, f.store, 0, 0)
	if _, e = f.db.DB().Exec(`DROP TRIGGER fail_call`); e != nil {
		t.Fatal(e)
	}
	r := reserve(t, f.store, c, "generation")
	_, e = f.db.DB().Exec(`CREATE TRIGGER fail_settle BEFORE UPDATE ON df_skill_maintenance_calls BEGIN SELECT RAISE(ABORT,'private-storage-text'); END`)
	if e != nil {
		t.Fatal(e)
	}
	if e = f.store.Broker(c).Settle(ctx, r, Usage{InputTokens: 1, OutputTokens: 1, Known: true}); e == nil {
		t.Fatal("expected fault")
	}
	usage(t, f.store, 1, 300)
	if _, e = f.store.Broker(c).Reserve(ctx, call("security")); !errors.Is(e, ErrConflict) {
		t.Fatalf("unsettled allowed another call %v", e)
	}
	if _, e = f.db.DB().Exec(`DROP TRIGGER fail_settle`); e != nil {
		t.Fatal(e)
	}
	if e = f.store.Cancel(ctx, j.ID, j.StateVersion); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	if e = f.store.Cancel(ctx, j.ID, c.Job.StateVersion); e != nil {
		t.Fatal(e)
	}
	if e = f.store.Broker(c).Settle(ctx, r, Usage{Known: true}); !errors.Is(e, ErrConflict) {
		t.Fatalf("cancel refunded %v", e)
	}
	usage(t, f.store, 1, 300)
	if e = f.store.Heartbeat(ctx, c); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	if e = f.store.Cancel(ctx, j.ID, c.Job.StateVersion); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
}

func TestMaintenanceDayBoundaryAndActualOverrun(t *testing.T) {
	ctx := context.Background()
	p := policy()
	p.MaxDailyTokens = 300
	f := newFixture(t, p)
	f.now.Store(time.Date(2026, 10, 2, 23, 59, 58, 0, time.UTC).UnixNano())
	enqueue(t, f.store, "midnight")
	c := claim(t, f.store)
	r := reserve(t, f.store, c, "generation")
	f.now.Add(int64(3 * time.Second))
	if e := f.store.Broker(c).Settle(ctx, r, Usage{InputTokens: 250, OutputTokens: 100, Known: true}); e != nil {
		t.Fatal(e)
	}
	usage(t, f.store, 1, 350)
	// A new UTC day gets its own allowance. The previous reservation's overrun
	// is never silently clamped, and midnight does not change its billed day.
	r2 := reserve(t, f.store, c, "quality")
	if r2.Day != "2026-10-03" {
		t.Fatal(r2)
	}
	if e := f.store.Broker(c).Settle(ctx, r2, Usage{InputTokens: 400, OutputTokens: 100, Known: true}); e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.Broker(c).Reserve(ctx, call("security")); !errors.Is(e, ErrBudget) {
		t.Fatal(e)
	}
	f.now.Add(-int64(time.Hour))
	if _, e := f.store.Broker(c).Reserve(ctx, call("security")); !errors.Is(e, ErrClock) {
		t.Fatal(e)
	}
}

func TestMaintenanceConcurrentAdmissionAndPolicyPinning(t *testing.T) {
	ctx := context.Background()
	p := policy()
	p.Workers = 1
	p.MaxDailyTokens = 300
	f := newFixture(t, p)
	enqueue(t, f.store, "a")
	enqueue(t, f.store, "b")
	var wg sync.WaitGroup
	var successes atomic.Int32
	claims := make(chan Claim, 8)
	for range 8 {
		wg.Go(func() {
			c, ok, e := f.store.Claim(ctx)
			if e != nil {
				t.Error(e)
			}
			if ok {
				successes.Add(1)
				claims <- c
			}
		})
	}
	wg.Wait()
	close(claims)
	if successes.Load() != 1 {
		t.Fatalf("concurrency %d", successes.Load())
	}
	c := <-claims
	for range 8 {
		wg.Go(func() {
			if _, e := f.store.Broker(c).Reserve(ctx, call("generation")); e == nil {
				successes.Add(1)
			} else if !errors.Is(e, ErrConflict) && !errors.Is(e, ErrBudget) {
				t.Error(e)
			}
		})
	}
	wg.Wait()
	if successes.Load() != 2 {
		t.Fatalf("reservation concurrency %d", successes.Load())
	}
	usage(t, f.store, 1, 300)
	if e := f.store.Finish(ctx, c, "model_failed", false); e != nil {
		t.Fatal(e)
	}
	// Loosening the host config cannot enlarge an already queued job's policy.
	looser := policy()
	s, e := New(ctx, f.db.DB(), looser)
	if e != nil {
		t.Fatal(e)
	}
	s.now = f.store.now
	c2 := claim(t, s)
	if _, e = s.Broker(c2).Reserve(ctx, call("generation")); !errors.Is(e, ErrBudget) {
		t.Fatal(e)
	}
}

func TestMaintenanceRunOneJoinsCancellation(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, policy())
	f.store.now = time.Now
	j := enqueue(t, f.store, "shutdown")
	work, cancel := context.WithCancel(ctx)
	started, returned := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, e := f.store.RunOne(work, func(ctx context.Context, j Job) (string, bool) {
			b, ok := BrokerFromContext(ctx)
			if !ok {
				t.Error("no broker")
			}
			_, e := b.Reserve(ctx, call("generation"))
			if e != nil {
				t.Error(e)
			}
			close(started)
			<-ctx.Done()
			close(returned)
			return "", false
		})
		done <- e
	}()
	<-started
	cancel()
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	select {
	case <-returned:
	default:
		t.Fatal("handler not joined")
	}
	state, e := f.store.Get(ctx, j.ID)
	if e != nil || state.State != "retry" || state.Attempts != 1 || state.ErrorCode != "cancelled" {
		t.Fatalf("cancel %+v %v", state, e)
	}
	r, e := f.store.Receipts(ctx, j.ID, 0, 10)
	if e != nil || len(r) != 1 || r[0].ChargedTokens != 300 {
		t.Fatalf("lost shutdown charge %+v %v", r, e)
	}
}
