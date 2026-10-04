package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func admitContinuationFixture(ctx context.Context, f *backgroundTargetFixture, c *BackgroundContinuation) error {
	b := c.Binding
	tasks := f.owner.native.Tasks().WithHooks(sqlite.TaskHooks{Create: func(ctx context.Context, tx *sql.Tx, _ *bt.Task) error {
		if err := c.CheckTx(ctx, tx, b); err != nil {
			return err
		}
		data, err := json.Marshal(b)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO harness_background_bindings(task_id,parent_session_id,child_session_id,origin_run_id,origin_tool_call_id,intent_hash,payload) VALUES(?,?,?,?,?,?,?)`, b.TaskID, b.ParentSessionID, b.ChildSessionID, b.OriginRunID, b.OriginToolCallID, b.IntentHash, data); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO harness_background_tree(task_id,root_session_id,parent_task_id,depth) VALUES(?,?,?,?)`, b.TaskID, b.RootSession(), b.ParentTaskID, b.TaskDepth()); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO harness_background_continuation_links(task_id,previous_task_id) VALUES(?,?)`, b.TaskID, b.ContinuationOf); err != nil {
			return err
		}
		if err = f.owner.host.BindTx(ctx, tx, b, c.Spec); err != nil {
			return err
		}
		if err = c.CommitTx(ctx, tx, b); err != nil {
			return err
		}
		return f.owner.adapter.BindTaskTx(ctx, tx, b)
	}})
	_, err := tasks.Create(c.Context(ctx), &bt.CreateTaskRequest{Spec: bt.Spec{ID: b.TaskID, SessionID: b.ParentSessionID, ExecutorKey: "fixture", NotifySession: true}, LeaseExpiryPolicy: bt.LeaseExpiryFail})
	return err
}

func TestBackgroundContinuationNativeAdmissionIsAtomic(t *testing.T) {
	for _, fault := range []string{"source", "budget", "none"} {
		t.Run(fault, func(t *testing.T) {
			f := newBackgroundTargetFixture(t, false)
			f.completeParent(t)
			ctx := context.WithValue(context.Background(), automaticNotificationKey{}, true)
			c, existing, err := f.service.PrepareBackgroundContinuation(ctx, "owner", f.owner.binding.RootSession(), f.note.ID)
			if err != nil || existing != "" || c == nil {
				t.Fatalf("prepare %q %v", existing, err)
			}
			var trigger string
			switch fault {
			case "source":
				trigger = `CREATE TRIGGER cont_fault BEFORE INSERT ON harness_background_continuation_sources BEGIN SELECT RAISE(ABORT,'source failure'); END`
			case "budget":
				trigger = `CREATE TRIGGER cont_fault BEFORE INSERT ON budget_members WHEN NEW.kind='background_continuation' BEGIN SELECT RAISE(ABORT,'ledger failure'); END`
			}
			if trigger != "" {
				if _, err = f.owner.store.db.Exec(trigger); err != nil {
					t.Fatal(err)
				}
			}
			err = admitContinuationFixture(ctx, f, c)
			if fault != "none" {
				if err == nil {
					t.Fatal("fault accepted continuation")
				}
				for _, q := range []string{
					`SELECT count(*) FROM eino_background_tasks WHERE id=?`,
					`SELECT count(*) FROM harness_background_bindings WHERE task_id=?`,
					`SELECT count(*) FROM harness_background_specs WHERE task_id=?`,
					`SELECT count(*) FROM harness_background_continuation_sources WHERE task_id=?`,
					`SELECT count(*) FROM harness_background_continuation_links WHERE task_id=?`,
					`SELECT count(*) FROM harness_runs WHERE id=?`,
					`SELECT count(*) FROM harness_inputs WHERE run_id=?`,
					`SELECT count(*) FROM budget_members WHERE id=?`,
				} {
					var count int
					if err = f.owner.store.db.QueryRow(q, c.Binding.TaskID).Scan(&count); err != nil || count != 0 {
						t.Fatalf("partial continuation in %s: %d %v", q, count, err)
					}
				}
				var used int
				if err = f.owner.store.db.QueryRow(`SELECT COALESCE(sum(used),0) FROM harness_automatic_continuations`).Scan(&used); err != nil || used != 0 {
					t.Fatalf("rollback spent auto quota %d %v", used, err)
				}
				if _, err = f.owner.store.db.Exec(`DROP TRIGGER cont_fault`); err != nil {
					t.Fatal(err)
				}
				if err = admitContinuationFixture(ctx, f, c); err != nil {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			_, existing, err = f.service.PrepareBackgroundContinuation(ctx, "owner", f.owner.binding.RootSession(), f.note.ID)
			if err != nil || existing != c.Binding.TaskID {
				t.Fatalf("duplicate source lost identity %q %v", existing, err)
			}
			var used int
			if err = f.owner.store.db.QueryRow(`SELECT used FROM harness_automatic_continuations WHERE root_id=?`, c.Binding.RootBudgetID).Scan(&used); err != nil || used != 1 {
				t.Fatalf("auto quota %d %v", used, err)
			}
			var root, kind, session string
			if err = f.owner.store.db.QueryRow(`SELECT root_id,kind,session_id FROM budget_members WHERE id=?`, c.Binding.TaskID).Scan(&root, &kind, &session); err != nil || root != f.owner.binding.RootBudgetID || kind != "background_continuation" || session == f.owner.binding.ChildSessionID {
				t.Fatalf("new execution did not keep private budget: %s %s %s %v", root, kind, session, err)
			}
		})
	}
}

func TestBackgroundContinuationAdmissionRejectsChangedRecipientAndUntrustedContext(t *testing.T) {
	for _, fault := range []string{"recipient", "quota", "stopped", "context", "host_version", "missing_host_context"} {
		t.Run(fault, func(t *testing.T) {
			f := newBackgroundTargetFixture(t, true)
			f.completeParent(t)
			ctx := context.WithValue(context.Background(), automaticNotificationKey{}, true)
			c, _, err := f.service.PrepareBackgroundContinuation(ctx, "owner", f.owner.binding.RootSession(), f.note.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "recipient":
				c.Binding.InvocationID = "different-role"
			case "quota":
				_, err = f.owner.store.db.Exec(`INSERT INTO harness_automatic_continuations(root_id,session_id,policy_digest,max_runs,used,next_at) VALUES(?,?,?,?,?,0)`, c.Binding.RootBudgetID, c.Binding.RootSession(), c.Spec.Roles.Digest, c.Spec.Automatic.Limit, c.Spec.Automatic.Limit)
			case "stopped":
				_, err = f.owner.store.db.Exec(`INSERT INTO harness_background_stops VALUES(?,'stop','fixture')`, f.owner.binding.TaskID)
			case "context":
				_, err = f.owner.store.db.Exec(`UPDATE harness_role_contexts SET payload='{}'`)
			case "host_version":
				c.source.Target.HostVersion++
			case "missing_host_context":
				err = executionTx(ctx, f.owner.store, func(tx *sql.Tx) error { return f.owner.host.BindTx(ctx, tx, c.Binding, c.Spec) })
				if !errors.Is(err, harness.ErrPermissionDenied) {
					t.Fatalf("untrusted continuation context accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = admitContinuationFixture(ctx, f, c); err == nil {
				t.Fatal("changed continuation accepted")
			}
			var count int
			if err = f.owner.store.db.QueryRow(`SELECT count(*) FROM harness_background_continuation_sources`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("invalid source published %d %v", count, err)
			}
		})
	}
}

func TestBackgroundContinuationStoredIdentity(t *testing.T) {
	for _, fault := range []string{"none", "missing_link", "wrong_link", "previous_binding", "source", "stopped"} {
		t.Run(fault, func(t *testing.T) {
			f := newBackgroundTargetFixture(t, false)
			f.completeParent(t)
			ctx := context.WithValue(context.Background(), automaticNotificationKey{}, true)
			c, _, err := f.service.PrepareBackgroundContinuation(ctx, "owner", f.owner.binding.RootSession(), f.note.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err = admitContinuationFixture(ctx, f, c); err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "missing_link":
				_, err = f.owner.store.db.Exec(`DELETE FROM harness_background_continuation_links WHERE task_id=?`, c.Binding.TaskID)
			case "wrong_link":
				_, err = f.owner.store.db.Exec(`UPDATE harness_background_continuation_links SET previous_task_id=? WHERE task_id=?`, f.child.binding.TaskID, c.Binding.TaskID)
			case "previous_binding":
				_, err = f.owner.store.db.Exec(`UPDATE harness_background_bindings SET payload='{}' WHERE task_id=?`, c.Binding.ContinuationOf)
			case "source":
				_, err = f.owner.store.db.Exec(`UPDATE harness_background_continuation_sources SET payload='{}' WHERE task_id=?`, c.Binding.TaskID)
			case "stopped":
				_, err = f.owner.store.db.Exec(`INSERT INTO harness_background_stops VALUES(?,'stopped','fixture')`, c.Binding.ContinuationOf)
			}
			if err != nil {
				t.Fatal(err)
			}
			err = executionTx(ctx, f.owner.store, func(tx *sql.Tx) error {
				_, err := readBackgroundContinuationRecordTx(ctx, tx, c.Binding, c.Spec)
				return err
			})
			if fault == "none" || fault == "stopped" {
				if err != nil {
					t.Fatalf("valid historical record cannot be read for cleanup: %v", err)
				}
			} else if !errors.Is(err, harness.ErrExecutionUnresumable) {
				t.Fatalf("damaged stored identity was not rejected: %v", err)
			}
			if fault == "stopped" {
				err = executionTx(ctx, f.owner.store, func(tx *sql.Tx) error {
					_, err := readBackgroundContinuationSourceTx(ctx, tx, c.Binding, c.Spec)
					return err
				})
				if !errors.Is(err, errBackgroundParentStopped) || !errors.Is(err, harness.ErrPermissionDenied) || blockedContinuationSource(err) {
					t.Fatalf("stop did not preserve execution denial and scheduler classification: %v", err)
				}
			}
		})
	}
}
