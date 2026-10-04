package background

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestMixedRolePlacementRechecksLiveParentInsideCreate(t *testing.T) {
	active := true
	checkedTx := false
	s, _ := newFixture(t, func(c *Config) {
		c.RolePlacement = func(_ context.Context, tx *sql.Tx) (*RolePlacement, error) {
			if !active {
				return nil, harness.ErrPermissionDenied
			}
			checkedTx = checkedTx || tx != nil
			return &RolePlacement{ID: "foreground-role", RootBudgetID: "root-1", Depth: 1}, nil
		}
	})
	in := submission("parent", "mixed")
	in.Binding.ParentInvocationID = "foreground-role"
	ctx := context.Background()
	prepared, err := s.bindSubmissionTree(ctx, nil, in.Binding)
	if err != nil || prepared.TaskDepth() != 2 || prepared.ParentTaskID != "" {
		t.Fatalf("placement=%+v %v", prepared, err)
	}
	in.Binding = prepared
	in, err = prepareSubmission(actor("parent"), in)
	if err != nil {
		t.Fatal(err)
	}
	active = false
	if _, err = s.manager.Submit(context.WithValue(ctx, createContextKey{}, in.Binding), &bt.SubmitRequest{Spec: in.Spec}); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatalf("inactive role accepted: %v", err)
	}
	for _, table := range []string{"eino_background_tasks", "harness_background_bindings", "harness_background_tree", "fixture_budget"} {
		if countRows(t, s.store, table) != 0 {
			t.Fatalf("partial rejected role task: %s", table)
		}
	}
	active = true
	for _, mutate := range []func(*Binding){
		func(b *Binding) { b.ParentInvocationID = "sibling" },
		func(b *Binding) { b.Depth = 1 },
		func(b *Binding) { b.RootBudgetID = "other" },
		func(b *Binding) { b.ParentTaskID = "made-up-parent" },
	} {
		forged := submission("parent", "forged")
		forged.Binding.ParentInvocationID = "foreground-role"
		mutate(&forged.Binding)
		if _, err = s.Submit(ctx, actor("parent"), forged); !errors.Is(err, harness.ErrPermissionDenied) {
			t.Fatalf("forged placement accepted: %+v %v", forged.Binding, err)
		}
	}
	valid := submission("parent", "valid")
	valid.Binding.ParentInvocationID = "foreground-role"
	got, err := s.Submit(ctx, actor("parent"), valid)
	if err != nil || !checkedTx || got.Depth != 2 || got.ParentInvocationID != "foreground-role" {
		t.Fatalf("valid placement: %+v checkedTx=%v %v", got, checkedTx, err)
	}
}
