package background

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func submitDescendant(t *testing.T, s *Service, ctx context.Context, origin string) harness.BackgroundTask {
	t.Helper()
	worker, err := s.WorkerActor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.Submit(ctx, worker, submission(worker.SessionID, origin))
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func finishTreeFixture(t *testing.T, s *Service, task *bt.Task, state *attemptState) {
	t.Helper()
	state.mu.Lock()
	state.ready, state.joined = true, true
	state.mu.Unlock()
	if _, err := s.tasks.Complete(context.Background(), &bt.CompleteTaskRequest{TaskID: task.Spec.ID, ExpectedVersion: task.Version}); err != nil {
		t.Fatal(err)
	}
}

func TestBackgroundTreeAuthorityAndDepth(t *testing.T) {
	s, _ := newFixture(t, nil)
	ctx := context.Background()
	root := submitFixture(t, s, submission("parent", "root"))
	sibling := submitFixture(t, s, submission("parent", "sibling"))
	_, rootCtx, _ := claimFixture(t, s, root.ID)
	child := submitDescendant(t, s, rootCtx, "child")
	_, childCtx, _ := claimFixture(t, s, child.ID)
	grandchild := submitDescendant(t, s, childCtx, "grandchild")
	_, leafCtx, _ := claimFixture(t, s, grandchild.ID)
	if root.RootSessionID != "parent" || root.Depth != 1 || child.RootSessionID != "parent" || child.ParentTaskID != root.ID || child.Depth != 2 || grandchild.ParentTaskID != child.ID || grandchild.Depth != 3 {
		t.Fatalf("wrong tree: %+v %+v %+v", root, child, grandchild)
	}
	for _, tc := range []struct {
		ctx   context.Context
		actor harness.TaskActor
		want  int
	}{
		{ctx, actor("parent"), 4}, {ctx, actor("other"), 0},
		{rootCtx, mustWorker(t, s, rootCtx), 2},
		{childCtx, mustWorker(t, s, childCtx), 1},
		{leafCtx, mustWorker(t, s, leafCtx), 0},
	} {
		got, err := s.List(tc.ctx, tc.actor, "", 100)
		if err != nil || len(got) != tc.want {
			t.Fatalf("list %+v: %d, %v; want %d", tc.actor, len(got), err, tc.want)
		}
		if len(got) > 1 {
			next, err := s.List(tc.ctx, tc.actor, got[0].ID, 1)
			if err != nil || len(next) != 1 || next[0].ID != got[1].ID {
				t.Fatalf("keyset: %+v %v", next, err)
			}
		}
	}
	worker := mustWorker(t, s, rootCtx)
	for _, id := range []string{root.ID, sibling.ID} {
		if _, err := s.Get(rootCtx, worker, id); !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("worker read ancestor/sibling: %v", err)
		}
		if _, err := s.Cancel(rootCtx, worker, id, "invalid"); !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("worker canceled ancestor/sibling: %v", err)
		}
	}
	if _, err := s.Get(ctx, worker, child.ID); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatalf("copied worker label: %v", err)
	}
	if _, err := s.Get(ctx, actor(root.ChildSessionID), child.ID); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatalf("public attachment to private child: %v", err)
	}
	if _, err := s.Get(rootCtx, actor("parent"), sibling.ID); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatalf("borrowed foreground attachment: %v", err)
	}
	if _, err := s.ResolveApproval(rootCtx, worker, child.ID, harness.TaskApproval{}); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatalf("worker approved child: %v", err)
	}
	leaf := mustWorker(t, s, leafCtx)
	if _, err := s.Submit(leafCtx, leaf, submission(leaf.SessionID, "depth-4")); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatalf("depth 4: %v", err)
	}
	for _, mutate := range []func(*Binding){
		func(b *Binding) { b.RootBudgetID = "different" },
		func(b *Binding) { b.Workspace = "different" },
		func(b *Binding) { b.ParentTaskID = sibling.ID },
		func(b *Binding) { b.RootSessionID = "other" },
		func(b *Binding) { b.Depth = 3 },
	} {
		in := submission(worker.SessionID, "forged")
		mutate(&in.Binding)
		if _, err := s.Submit(rootCtx, worker, in); !errors.Is(err, harness.ErrPermissionDenied) {
			t.Fatalf("forged ancestry: %+v %v", in.Binding, err)
		}
	}
	if countRows(t, s.store, "harness_background_tree") != 4 {
		t.Fatal("rejected submissions changed the tree")
	}
}

func mustWorker(t *testing.T, s *Service, ctx context.Context) harness.TaskActor {
	t.Helper()
	worker, err := s.WorkerActor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return worker
}

func TestBackgroundTreeLegacyMigrationAndAtomicBinding(t *testing.T) {
	s, budget := newFixture(t, nil)
	ctx := context.Background()
	root := submitFixture(t, s, submission("parent", "legacy"))
	var before []byte
	if err := s.store.DB().QueryRow(`SELECT payload FROM harness_background_bindings WHERE task_id=?`, root.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(before, []byte(`"RootSessionID"`)) || bytes.Contains(before, []byte(`"Depth"`)) {
		t.Fatal("legacy serialized identity changed")
	}
	if _, err := s.store.DB().Exec(`DROP TABLE harness_background_tree`); err != nil {
		t.Fatal(err)
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var after []byte
	if err := s.store.DB().QueryRow(`SELECT payload FROM harness_background_bindings WHERE task_id=?`, root.ID).Scan(&after); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("migration rewrote identity: %v", err)
	}
	if replay := submitFixture(t, s, submission("parent", "legacy")); replay.ID != root.ID || replay.Depth != 1 {
		t.Fatalf("legacy replay: %+v", replay)
	}
	_, rootCtx, _ := claimFixture(t, s, root.ID)
	worker := mustWorker(t, s, rootCtx)
	budget.failBind.Store(true)
	if _, err := s.Submit(rootCtx, worker, submission(worker.SessionID, "rollback")); err == nil {
		t.Fatal("expected budget binding failure")
	}
	for _, table := range []string{"harness_background_tree", "harness_background_bindings", "eino_background_tasks", "fixture_budget"} {
		if countRows(t, s.store, table) != 1 {
			t.Fatalf("partial tree creation: %s", table)
		}
	}
}

func TestBackgroundTreeCancelPersistsAcrossNativeFailureAndRestart(t *testing.T) {
	s, _ := newFixture(t, nil)
	ctx := context.Background()
	root := submitFixture(t, s, submission("parent", "root"))
	sibling := submitFixture(t, s, submission("parent", "sibling"))
	native, rootCtx, state := claimFixture(t, s, root.ID)
	child := submitDescendant(t, s, rootCtx, "child")
	childNative, childCtx, childState := claimFixture(t, s, child.ID)
	grandchild := submitDescendant(t, s, childCtx, "grandchild")
	finishTreeFixture(t, s, childNative, childState)
	finishTreeFixture(t, s, native, state)
	// Stopping a completed parent must still stop its pending descendants.
	if _, err := s.store.DB().Exec(`CREATE TRIGGER stop_fault BEFORE UPDATE ON eino_background_tasks BEGIN SELECT RAISE(ABORT,'native cancellation fault'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cancel(ctx, actor("parent"), root.ID, "user stopped subtree"); err == nil {
		t.Fatal("native cancellation fault was hidden")
	}
	if countRows(t, s.store, "harness_background_stops") != 3 {
		t.Fatal("stop intent was not committed for the whole subtree")
	}
	if _, err := s.store.DB().Exec(`DROP TRIGGER stop_fault`); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(ctx, s.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		deadline, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		_ = reopened.DrainAndClose(deadline)
	})
	// StartWorkers performs this same pass before dispatching pending work.
	if err = reopened.applyStops(ctx, ""); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ id, status string }{{root.ID, "completed"}, {child.ID, "completed"}, {grandchild.ID, "canceled"}, {sibling.ID, "pending"}} {
		got, err := reopened.Get(ctx, actor("parent"), tc.id)
		if err != nil || got.Status != tc.status {
			t.Fatalf("restart status: %+v, %v; want %s", got, err, tc.status)
		}
	}
	if _, err = reopened.Cancel(ctx, actor("parent"), root.ID, "repeated"); err != nil {
		t.Fatalf("idempotent subtree cancel: %v", err)
	}
}

func TestBackgroundTreeStopFencesAlreadyPreparedSubmission(t *testing.T) {
	s, _ := newFixture(t, nil)
	root := submitFixture(t, s, submission("parent", "root"))
	_, ctx, _ := claimFixture(t, s, root.ID)
	worker := mustWorker(t, s, ctx)
	in := submission(worker.SessionID, "prepared-child")
	var err error
	in.Binding, err = s.bindSubmissionTree(ctx, nil, in.Binding)
	if err != nil {
		t.Fatal(err)
	}
	in, err = prepareSubmission(worker, in)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a stop committed after authorization but before native creation.
	if _, err = s.store.DB().Exec(`INSERT INTO harness_background_stops(task_id,reason,created_at) VALUES(?, 'stop', 'fixture')`, root.ID); err != nil {
		t.Fatal(err)
	}
	createCtx := context.WithValue(ctx, createContextKey{}, in.Binding)
	if _, err = s.manager.Submit(createCtx, &bt.SubmitRequest{Spec: in.Spec}); !errors.Is(err, bt.ErrLeaseLost) {
		t.Fatalf("stop lost race with child creation: %v", err)
	}
	if err = s.CheckEffect(ctx); !errors.Is(err, bt.ErrLeaseLost) {
		t.Fatalf("stopped worker admitted effects: %v", err)
	}
	if countRows(t, s.store, "harness_background_tree") != 1 || countRows(t, s.store, "eino_background_tasks") != 1 {
		t.Fatal("failed child creation left durable rows")
	}
}

func TestBackgroundTreeInboxDoesNotFollowReusedChildSession(t *testing.T) {
	s, _ := newFixture(t, nil)
	ctx := context.Background()
	first := submitFixture(t, s, submission("parent", "first"))
	native, firstCtx, state := claimFixture(t, s, first.ID)
	oldChild := submitDescendant(t, s, firstCtx, "old-child")
	if err := s.DeliverNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	firstWorker := mustWorker(t, s, firstCtx)
	oldInbox, err := s.ListInbox(firstCtx, firstWorker, 0, 100)
	if err != nil || len(oldInbox) != 1 || oldInbox[0].TaskID != oldChild.ID {
		t.Fatalf("first inbox: %+v %v", oldInbox, err)
	}
	finishTreeFixture(t, s, native, state)
	in := submission("parent", "reuse")
	in.Binding.ChildSessionID = first.ChildSessionID
	second := submitFixture(t, s, in)
	_, secondCtx, _ := claimFixture(t, s, second.ID)
	secondWorker := mustWorker(t, s, secondCtx)
	newChild := submitDescendant(t, s, secondCtx, "new-child")
	if err = s.DeliverNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	current, err := s.ListInbox(secondCtx, secondWorker, 0, 100)
	if err != nil || len(current) != 1 || current[0].TaskID != newChild.ID {
		t.Fatalf("inbox crossed task identity: %+v %v", current, err)
	}
	if err = s.AcknowledgeInbox(secondCtx, secondWorker, oldInbox[0].ID); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("consumed previous task notification: %v", err)
	}
	if _, err = s.Get(secondCtx, secondWorker, oldChild.ID); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("reused child borrowed old descendant: %v", err)
	}
	rootInbox, err := s.ListInbox(ctx, actor("parent"), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range rootInbox {
		if item.TaskID == oldChild.ID || item.TaskID == newChild.ID {
			t.Fatal("descendant result delivered directly to root model")
		}
	}
	if err = s.AcknowledgeInbox(secondCtx, secondWorker, current[0].ID); err != nil {
		t.Fatal(err)
	}
}

func TestBackgroundTreeLiveCascadeWaitsForAllCleanup(t *testing.T) {
	ctx := context.Background()
	started := make(chan TaskScope, 2)
	cleaning := make(chan struct{}, 2)
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	var service *Service
	executor := &fixtureExecutor{run: func(ctx context.Context, _ *bt.Task, runtime bt.ExecutionRuntime) (*bt.ExecutionResult, error) {
		scope, ok := ScopeFromContext(ctx)
		if !ok {
			return nil, errors.New("missing managed worker identity")
		}
		if scope.Binding.TaskDepth() == 1 {
			worker, err := service.WorkerActor(ctx)
			if err != nil {
				return nil, err
			}
			if _, err = service.Submit(ctx, worker, submission(worker.SessionID, "live-child")); err != nil {
				return nil, err
			}
		}
		started <- scope
		select {
		case <-runtime.Controls():
			return &bt.ExecutionResult{Status: bt.StatusCanceled}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	service, _ = newFixture(t, func(c *Config) {
		c.MaxWorkers = 2
		c.AdditionalExecutors = []bt.Executor{executor}
		c.Attempts = &fixtureFactory{open: func(context.Context, TaskScope) (*Attempt, error) {
			return &Attempt{JoinAndClose: func(context.Context) error { cleaning <- struct{}{}; <-release; return nil }}, nil
		}}
	})
	// This defer runs before fixture cleanup, even if an assertion fails.
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	root := submitFixture(t, service, submission("parent", "live-root"))
	if err := service.StartWorkers(ctx); err != nil {
		t.Fatal(err)
	}
	var scopes []TaskScope
	for range 2 {
		select {
		case scope := <-started:
			scopes = append(scopes, scope)
		case <-time.After(5 * time.Second):
			t.Fatal("nested workers did not start")
		}
	}
	if _, err := service.Cancel(ctx, actor("parent"), root.ID, "stop all descendants"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case <-cleaning:
		case <-time.After(5 * time.Second):
			t.Fatal("cascade missed an active descendant")
		}
	}
	for _, scope := range scopes {
		snapshot, err := service.Get(ctx, actor("parent"), scope.Binding.TaskID)
		if err != nil || snapshot.Status != "running" {
			t.Fatalf("terminal state before joined cleanup: %+v %v", snapshot, err)
		}
		var commits int
		if err = service.store.DB().QueryRow(`SELECT commits FROM fixture_budget WHERE task_id=?`, scope.Binding.TaskID).Scan(&commits); err != nil || commits != 0 {
			t.Fatalf("settled budget before cleanup: %d %v", commits, err)
		}
	}
	close(release)
	waitCondition(t, func() bool {
		for _, scope := range scopes {
			task, err := service.Get(ctx, actor("parent"), scope.Binding.TaskID)
			if err != nil || task.Status != "canceled" {
				return false
			}
		}
		return true
	})
	if countRows(t, service.store, "harness_background_child_leases") != 0 {
		t.Fatal("cascade retained child leases after joined cleanup")
	}
}
