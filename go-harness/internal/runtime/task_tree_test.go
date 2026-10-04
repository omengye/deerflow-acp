package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
)

func TestTaskTreeProjectionPaginationAuthorityAndUnknownHistory(t *testing.T) {
	f, attempt, state := newRoleContextFixture(t)
	ctx := context.Background()
	s := attempt.service
	actor := harness.TaskActor{SessionID: f.req.Session.ID, OwnerID: "owner"}
	controller, err := agents.NewController(f.s.db)
	if err != nil {
		t.Fatal(err)
	}
	child := state.Invocation
	child.ID = "child-role"
	child.ParentID = state.Invocation.ID
	child.Depth = 2
	if err = controller.Register(ctx, child, 1, 3); err != nil {
		t.Fatal(err)
	}
	a, err := controller.BeginAttempt(ctx, child.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.RecordState(ctx, "running", nil); err != nil {
		t.Fatal(err)
	}
	if err = a.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	text := "final child answer"
	if err = a.RecordState(ctx, "completed", &text); err != nil {
		t.Fatal(err)
	}
	var cursor string
	nodes := map[string]harness.TaskTreeNode{}
	for i := 0; i < 4; i++ {
		page, err := s.TaskTree(ctx, actor, harness.TaskTreeQuery{RootID: f.req.RootBudgetID, Cursor: cursor, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if page.Consistency != "live_keyset" || len(page.Nodes) != 1 {
			t.Fatalf("bad page %+v", page)
		}
		node := page.Nodes[0]
		if _, ok := nodes[node.ID]; ok {
			t.Fatalf("duplicate logical node %s", node.ID)
		}
		nodes[node.ID] = node
		if i == 0 {
			wrong := actor
			wrong.OwnerID = "intruder"
			if _, err = s.TaskTree(ctx, wrong, harness.TaskTreeQuery{Cursor: page.NextCursor}); err == nil {
				t.Fatal("wrong owner read tree")
			}
			if _, err = s.TaskTree(ctx, actor, harness.TaskTreeQuery{Cursor: page.NextCursor, Limit: 1}); !errors.Is(err, harness.ErrInvalidInput) {
				t.Fatalf("cursor filter escaped: %v", err)
			}
		}
		if !page.HasMore {
			break
		}
		cursor = page.NextCursor
	}
	if len(nodes) != 3 {
		t.Fatalf("nodes=%+v", nodes)
	}
	old := nodes[state.Invocation.ID]
	if old.Status != "unavailable" || old.Version != 0 || old.Usage != nil || old.Result != nil || old.AllowedActions == nil || len(old.AllowedActions) != 0 {
		t.Fatalf("invented historical result: %+v", old)
	}
	got := nodes[child.ID]
	if got.Mode != "foreground" || got.ParentID != state.Invocation.ID || got.Depth != 2 || got.Status != "completed" || got.Result == nil || got.Result.Text != text || got.Usage != nil {
		t.Fatalf("child=%+v", got)
	}
	root := nodes["root/"+f.req.RootBudgetID]
	if root.Mode != "root" || root.Usage == nil || root.Usage.Scope != "root_total" || root.Model != f.req.Session.Model {
		t.Fatalf("root=%+v", root)
	}
	data, _ := json.Marshal(nodes)
	if strings.Contains(string(data), "private task") || strings.Contains(string(data), "first summary") || strings.Contains(string(data), `"allowedActions":null`) {
		t.Fatalf("private transcript or null action leaked: %s", data)
	}
	if _, err = s.TaskTreeNode(ctx, actor, "unrelated-root", child.ID); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("wrong root=%v", err)
	}
}

func TestTaskTreeCancelVersionSubtreeAndIdleRoot(t *testing.T) {
	f, a, state := newRoleContextFixture(t)
	ctx := context.Background()
	s := a.service
	actor := harness.TaskActor{SessionID: f.req.Session.ID, OwnerID: "owner"}
	c, err := agents.NewController(f.s.db)
	if err != nil {
		t.Fatal(err)
	}
	role, err := c.BeginAttempt(ctx, state.Invocation.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = role.RecordState(ctx, "running", nil); err != nil {
		t.Fatal(err)
	}
	if err = role.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	if err = role.RecordState(ctx, "waiting_input", nil); err != nil {
		t.Fatal(err)
	}
	node, err := s.TaskTreeNode(ctx, actor, f.req.RootBudgetID, state.Invocation.ID)
	if err != nil {
		t.Fatal(err)
	}
	req := harness.TaskTreeCancelRequest{RootID: node.RootID, NodeID: node.ID, ExpectedVersion: node.Version + 1}
	if _, err = s.CancelTaskTree(ctx, actor, req); !errors.Is(err, harness.ErrExecutionConflict) {
		t.Fatalf("stale version accepted: %v", err)
	}
	req.ExpectedVersion = node.Version
	got, err := s.CancelTaskTree(ctx, actor, req)
	if err != nil || got.Status != "cancelling" || len(got.AllowedActions) != 0 {
		t.Fatalf("cancel=%+v %v", got, err)
	}
	if _, err = s.CancelTaskTree(ctx, actor, req); err != nil {
		t.Fatalf("retry=%v", err)
	}
	sibling := state.Invocation
	sibling.ID = "sibling"
	if err = c.Register(ctx, sibling, 1, 3); err != nil {
		t.Fatalf("sibling blocked: %v", err)
	}
	if _, err = c.BeginAttempt(ctx, state.Invocation.ID, time.Minute); !errors.Is(err, agents.ErrStopped) {
		t.Fatalf("stopped role resumed=%v", err)
	}
	f.pending(t)
	f.suspend(t)
	root, err := s.TaskTreeNode(ctx, actor, f.req.RootBudgetID, "root/"+f.req.RootBudgetID)
	if err != nil {
		t.Fatal(err)
	}
	got, err = s.CancelTaskTree(ctx, actor, harness.TaskTreeCancelRequest{RootID: root.RootID, NodeID: root.ID, ExpectedVersion: root.Version})
	if err != nil || got.Status != "cancelled" {
		t.Fatalf("idle root cancel=%+v %v", got, err)
	}
	if _, err = c.BeginAttempt(ctx, sibling.ID, time.Minute); !errors.Is(err, agents.ErrStopped) {
		t.Fatalf("root descendant resumed=%v", err)
	}
}

func TestTaskTreeLegacyBackgroundIdentityPrivateChildAndCancellation(t *testing.T) {
	f := newBackgroundHostFixture(t)
	if err := f.create(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := agents.NewController(f.store.db); err != nil {
		t.Fatal(err)
	}
	s := NewService(f.store, nil, f.spec.Parent.Model)
	actor := harness.TaskActor{SessionID: f.binding.RootSession(), OwnerID: "owner"}
	if _, err := s.Coordinator.Attach(actor.SessionID, actor.OwnerID); err != nil {
		t.Fatal(err)
	}
	id := "background/" + f.binding.TaskID
	n, err := s.TaskTreeNode(ctx, actor, f.binding.RootBudgetID, id)
	if err != nil || n.Mode != "background" || n.TaskID != f.binding.TaskID || n.ParentID != "root/"+f.binding.RootBudgetID {
		t.Fatalf("legacy projection=%+v %v", n, err)
	}
	child := harness.TaskActor{SessionID: f.binding.ChildSessionID, OwnerID: "child-owner"}
	if _, err = s.Coordinator.Attach(child.SessionID, child.OwnerID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.TaskTree(ctx, child, harness.TaskTreeQuery{}); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatalf("private child exposed=%v", err)
	}
	got, err := s.CancelTaskTree(ctx, actor, harness.TaskTreeCancelRequest{RootID: n.RootID, NodeID: n.ID, ExpectedVersion: n.Version})
	if err != nil || got.Status != "cancelling" {
		t.Fatalf("legacy cancel=%+v %v", got, err)
	}
	var stopped int
	if err = f.store.db.QueryRow(`SELECT count(*) FROM harness_background_stops WHERE task_id=?`, f.binding.TaskID).Scan(&stopped); err != nil || stopped != 1 {
		t.Fatalf("legacy native stop missing=%d %v", stopped, err)
	}
}
