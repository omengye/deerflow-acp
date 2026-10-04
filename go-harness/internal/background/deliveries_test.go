package background

import (
	"context"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func recordDelivery(t *testing.T, s *Service, d TaskDelivery) error {
	t.Helper()
	tx, err := s.store.DB().BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = RecordTaskDeliveryTx(context.Background(), tx, d); err != nil {
		return err
	}
	return tx.Commit()
}

func TestInboxConsumedDeliveryArrivalOrderAndRecipient(t *testing.T) {
	for _, private := range []bool{false, true} {
		for _, late := range []bool{false, true} {
			name := "root"
			if private {
				name = "private"
			}
			if late {
				name += "_late_notification"
			}
			t.Run(name, func(t *testing.T) {
				s, _ := newFixture(t, nil)
				ctx := context.Background()
				inboxCtx, recipient := ctx, actor("parent")
				var child harness.BackgroundTask
				role := ""
				if private {
					parent := submitFixture(t, s, submission("parent", "parent"))
					_, inboxCtx, _ = claimFixture(t, s, parent.ID)
					recipient = mustWorker(t, s, inboxCtx)
					child = submitDescendant(t, s, inboxCtx, "child")
					role = "background/" + parent.ID
				} else {
					child = submitFixture(t, s, submission("parent", "child"))
				}
				task, _, attempt := claimFixture(t, s, child.ID)
				finishTreeFixture(t, s, task, attempt)
				task, err := s.tasks.Get(ctx, child.ID)
				if err != nil {
					t.Fatal(err)
				}
				binding, _, err := loadBinding(ctx, s.store.DB(), child.ID)
				if err != nil {
					t.Fatal(err)
				}
				d := TaskDelivery{TaskID: task.Spec.ID, Version: task.Version, RecipientRole: role, SessionID: binding.ParentSessionID, RootBudgetID: binding.RootBudgetID, RunID: binding.OriginRunID}
				if !late {
					if err = s.DeliverNotifications(ctx); err != nil {
						t.Fatal(err)
					}
				}
				// A different authorized role's result lookup does not consume
				// the original recipient's notification.
				ancestor := d
				ancestor.RecipientRole = "another-authorized-role"
				if err = recordDelivery(t, s, ancestor); err != nil {
					t.Fatal(err)
				}
				var count int
				if err = s.store.DB().QueryRow(`SELECT COUNT(*) FROM harness_notification_work WHERE task_id=? AND state='consumed'`, child.ID).Scan(&count); err != nil || count != 0 {
					t.Fatalf("ancestor consumed private result: %d %v", count, err)
				}
				if err = recordDelivery(t, s, d); err != nil {
					t.Fatal(err)
				}
				if err = s.DeliverNotifications(ctx); err != nil {
					t.Fatal(err)
				}
				// Replays preserve one record and do not turn model delivery
				// into UI-read ACK or erase the durable recipient.
				if err = recordDelivery(t, s, d); err != nil {
					t.Fatal(err)
				}
				items, err := s.ListInbox(inboxCtx, recipient, 0, 100)
				if err != nil {
					t.Fatal(err)
				}
				found := 0
				for _, item := range items {
					if item.TaskID == child.ID && item.Kind == "completed" {
						found++
						if item.Delivery == nil || item.Delivery.Status != "consumed" || item.Delivery.RecipientRole != role || item.Delivery.RunID != d.RunID {
							t.Fatalf("terminal inbox did not retain delivery: %+v", item)
						}
						if err = s.AcknowledgeInbox(inboxCtx, recipient, item.ID); err != nil {
							t.Fatal(err)
						}
					} else if item.Delivery != nil {
						t.Fatalf("nonterminal notification marked consumed: %+v", item)
					}
				}
				if found != 1 || countRows(t, s.store, "harness_task_deliveries") != 2 {
					t.Fatalf("lost or duplicated delivery: terminal=%d", found)
				}
				if err = s.store.DB().QueryRow(`SELECT COUNT(*) FROM harness_notification_work WHERE task_id=? AND state='consumed'`, child.ID).Scan(&count); err != nil || count != 1 {
					t.Fatalf("UI ACK lost model delivery: %d %v", count, err)
				}
			})
		}
	}
}

func TestInboxConsumedProjectionRollsBackWithResult(t *testing.T) {
	s, _ := newFixture(t, nil)
	ctx := context.Background()
	child := submitFixture(t, s, submission("parent", "child"))
	task, _, attempt := claimFixture(t, s, child.ID)
	finishTreeFixture(t, s, task, attempt)
	if err := s.DeliverNotifications(ctx); err != nil {
		t.Fatal(err)
	}
	b, _, err := loadBinding(ctx, s.store.DB(), child.ID)
	if err != nil {
		t.Fatal(err)
	}
	d := TaskDelivery{TaskID: child.ID, Version: task.Version + 1, SessionID: b.ParentSessionID, RootBudgetID: b.RootBudgetID, RunID: b.OriginRunID}
	if _, err = s.store.DB().Exec(`CREATE TRIGGER inbox_delivery_fault BEFORE INSERT ON harness_notification_work BEGIN SELECT RAISE(ABORT,'projection fault'); END`); err != nil {
		t.Fatal(err)
	}
	if err = recordDelivery(t, s, d); err == nil {
		t.Fatal("projection failure was ignored")
	}
	if countRows(t, s.store, "harness_task_deliveries") != 0 || countRows(t, s.store, "harness_notification_work") != 0 {
		t.Fatal("partial delivery/projection survived failed transaction")
	}
	if _, err = s.store.DB().Exec(`DROP TRIGGER inbox_delivery_fault`); err != nil {
		t.Fatal(err)
	}
	if err = recordDelivery(t, s, d); err != nil {
		t.Fatal(err)
	}
}
