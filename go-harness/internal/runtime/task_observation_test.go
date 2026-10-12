package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

func TestPriorRootTaskObservationPreservesExecutionScope(t *testing.T) {
	for _, scenario := range []string{"prior-root", "private-role", "foreign-session", "forged-tree", "foreign-run", "forged-root", "stale-version", "future-after-version", "missing-call"} {
		t.Run(scenario, func(t *testing.T) {
			f := newExecutionWaitFixture(t)
			f.suspend(t, false)
			f.complete(t)
			lease := f.resume(t, "owner")
			a := &executionAttempt{service: automaticService(t, f, &automaticWaitEngine{}), lease: lease}
			task, err := f.child.tasks.Get(context.Background(), f.child.binding.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			binding := f.child.binding
			binding.RootBudgetID = "earlier-root"
			d := interaction.TaskResultDelivery{Wait: f.wait.Wait, TaskVersion: task.Version}
			want := harness.ErrPermissionDenied
			switch scenario {
			case "prior-root":
				want = nil
			case "private-role":
				d.Wait.RoleID = "private-role"
			case "foreign-session":
				binding.RootSessionID = "foreign-session"
				if _, err = f.s.db.Exec(`UPDATE harness_background_tree SET root_session_id=? WHERE task_id=?`, binding.RootSessionID, binding.TaskID); err != nil {
					t.Fatal(err)
				}
			case "forged-tree":
				if _, err = f.s.db.Exec(`UPDATE harness_background_tree SET root_session_id='foreign' WHERE task_id=?`, binding.TaskID); err != nil {
					t.Fatal(err)
				}
			case "foreign-run":
				d.Wait.RunID = "foreign-run"
			case "forged-root":
				d.Wait.RootBudgetID = "earlier-root"
			case "stale-version":
				d.TaskVersion--
				want = harness.ErrExecutionConflict
			case "future-after-version":
				d.Wait.AfterVersion = task.Version + 1
			case "missing-call":
				d.Wait.CallID = ""
			}
			data, err := json.Marshal(binding)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.s.db.Exec(`UPDATE harness_background_bindings SET payload=? WHERE task_id=?`, data, binding.TaskID); err != nil {
				t.Fatal(err)
			}
			if err = a.ObserveTaskResult(context.Background(), d); !errors.Is(err, want) {
				t.Fatalf("observation=%v want=%v", err, want)
			}
			if len(a.taskResults) != 0 {
				t.Fatal("inspection staged another execution's delivery")
			}
			var count int
			if err = f.s.db.QueryRow(`SELECT count(*) FROM harness_task_deliveries`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("inspection published delivery: %d %v", count, err)
			}
		})
	}
}
