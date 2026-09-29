package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestBackgroundPermissionPreviewOwnerExactBatchAndNoMutation(t *testing.T) {
	f := newBackgroundPermissionFixture(t)
	f.pending(t, "first")
	if err := f.wait(t); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	query := harness.BackgroundPermissionQuery{TaskID: f.binding.TaskID, InteractionID: f.state.ID, TaskVersion: f.state.TaskVersion, IntentID: f.intents[0].ID}
	for _, actor := range []harness.TaskActor{{OwnerID: "stranger", SessionID: f.binding.ParentSessionID}, {OwnerID: f.actor().OwnerID, SessionID: "wrong-parent"}} {
		if got, err := f.permissions.Permission(ctx, actor, f.binding, query); err == nil || len(got.Arguments) > 0 {
			t.Fatalf("unowned preview disclosed request: %+v %v", got, err)
		}
	}
	for _, change := range []func(*harness.BackgroundPermissionQuery){
		func(q *harness.BackgroundPermissionQuery) { q.TaskVersion-- },
		func(q *harness.BackgroundPermissionQuery) { q.InteractionID = "wrong-batch" },
		func(q *harness.BackgroundPermissionQuery) { q.IntentID = "wrong-intent" },
		func(q *harness.BackgroundPermissionQuery) { q.TaskID = "wrong-task" },
	} {
		bad := query
		change(&bad)
		if got, err := f.permissions.Permission(ctx, f.actor(), f.binding, bad); err == nil || len(got.Arguments) > 0 {
			t.Fatalf("stale/mismatched preview disclosed request: %+v %v", got, err)
		}
	}
	got, err := f.permissions.Permission(ctx, f.actor(), f.binding, query)
	if err != nil || !bytes.Equal(got.Arguments, f.requests[0].Arguments) || got.ToolName != f.requests[0].ToolName || got.ToolCallID != f.requests[0].ToolCallID {
		t.Fatalf("exact preview: %+v %v", got, err)
	}
	var grants int
	if err = f.store.db.QueryRowContext(ctx, `SELECT count(*) FROM harness_background_permission_grants`).Scan(&grants); err != nil || grants != 0 {
		t.Fatalf("preview granted execution: %d %v", grants, err)
	}
	current, err := f.permissions.Interaction(ctx, f.actor(), f.binding)
	before, _ := json.Marshal(f.state)
	after, _ := json.Marshal(current)
	if err != nil || !bytes.Equal(before, after) || bytes.Contains(after, []byte("private content")) {
		t.Fatalf("preview changed/leaked public metadata: %s %v", after, err)
	}
	var saved []byte
	if err = f.store.db.QueryRowContext(ctx, `SELECT payload FROM harness_background_permission_manifests WHERE task_id=?`, query.TaskID).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	var changed backgroundPermissionManifest
	if err = json.Unmarshal(saved, &changed); err != nil {
		t.Fatal(err)
	}
	changed.Attempt++
	changedBytes, err := json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.db.ExecContext(ctx, `UPDATE harness_background_permission_manifests SET payload=? WHERE task_id=?`, changedBytes, query.TaskID); err != nil {
		t.Fatal(err)
	}
	if got, err = f.permissions.Permission(ctx, f.actor(), f.binding, query); err == nil || len(got.Arguments) > 0 {
		t.Fatalf("wrong-attempt preview disclosed request: %+v %v", got, err)
	}
	if _, err = f.store.db.ExecContext(ctx, `UPDATE harness_background_permission_manifests SET payload=? WHERE task_id=?`, saved, query.TaskID); err != nil {
		t.Fatal(err)
	}
	if err = f.resume(f.approval(harness.AllowOnce)); err != nil {
		t.Fatal(err)
	}
	if got, err = f.permissions.Permission(ctx, f.actor(), f.binding, query); err == nil || len(got.Arguments) != 0 {
		t.Fatalf("resolved preview accepted: %+v %v", got, err)
	}
}
