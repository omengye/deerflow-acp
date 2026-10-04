package runtime

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
)

func TestBackgroundContinuationResourceWaitDoesNotSpendQuotaOrRetry(t *testing.T) {
	f := newBackgroundTargetFixture(t, false)
	f.completeParent(t)
	ctx := context.Background()
	f.service.BackgroundContinuation = func(context.Context, string, string, string) (harness.BackgroundTask, error) {
		panic("scan dispatched unavailable resource")
	}
	f.service.CheckContinuationResources = func(context.Context, *sql.Tx, background.Binding, BackgroundExecutionSpec) error {
		return harness.ErrResourcesWaiting
	}
	root := f.owner.binding.RootSession()
	for range 2 {
		id, err := f.service.nextAutomaticNotification(ctx, "owner", root)
		if err != nil || id != "" {
			t.Fatalf("resource wait selected %q: %v", id, err)
		}
		if _, err = f.owner.store.db.Exec(`UPDATE harness_notification_work SET next_at=0`); err != nil {
			t.Fatal(err)
		}
	}
	var state string
	var attempts, quota int
	if err := f.owner.store.db.QueryRow(`SELECT state,attempts FROM harness_notification_work WHERE notification_id=?`, f.note.ID).Scan(&state, &attempts); err != nil || state != "waiting_resources" || attempts != 0 {
		t.Fatalf("resource wait state=%q attempts=%d err=%v", state, attempts, err)
	}
	if _, _, err := f.service.PrepareBackgroundContinuation(ctx, "owner", root, f.note.ID); !errors.Is(err, harness.ErrResourcesWaiting) {
		t.Fatalf("explicit continuation bypassed resource gate: %v", err)
	}
	if err := f.owner.store.db.QueryRow(`SELECT COALESCE(sum(used),0) FROM harness_automatic_continuations`).Scan(&quota); err != nil || quota != 0 {
		t.Fatalf("resource wait spent quota=%d: %v", quota, err)
	}
	f.service.CheckContinuationResources = func(context.Context, *sql.Tx, background.Binding, BackgroundExecutionSpec) error { return nil }
	id, err := f.service.nextAutomaticNotification(ctx, "owner", root)
	if err != nil || id != f.note.ID {
		t.Fatalf("restored resources did not unblock notification %q: %v", id, err)
	}
	continuation, existing, err := f.service.PrepareBackgroundContinuation(ctx, "owner", root, f.note.ID)
	if err != nil || continuation == nil || existing != "" {
		t.Fatalf("restored resources admission=%+v existing=%q err=%v", continuation, existing, err)
	}
}

func TestForegroundContinuationResourceGenerationFailureBlocksRebuild(t *testing.T) {
	f, attempt := newRoleResultFixture(t)
	ctx := context.Background()
	called := false
	attempt.service.ContinuationResourceGeneration = func(context.Context, *sql.Tx, background.Binding, BackgroundExecutionSpec) (string, error) {
		called = true
		return "", harness.ErrResourcesWaiting
	}
	err := executionTx(ctx, f.s, func(tx *sql.Tx) error {
		row, err := readExecution(ctx, tx, attempt.lease.Scope.SessionID, attempt.lease.Scope.MemberID)
		if err != nil {
			return err
		}
		_, err = attempt.service.executionInputSourceTx(ctx, tx, row)
		return err
	})
	if !called || !errors.Is(err, harness.ErrResourcesWaiting) {
		t.Fatalf("foreground continuation ignored resource authorization: called=%t err=%v", called, err)
	}
	attempt.service.ContinuationResourceGeneration = func(context.Context, *sql.Tx, background.Binding, BackgroundExecutionSpec) (string, error) {
		return "new-authorized-client-generation", nil
	}
	mustExecutionTx(t, f, func(tx *sql.Tx) error {
		row, err := readExecution(ctx, tx, attempt.lease.Scope.SessionID, attempt.lease.Scope.MemberID)
		if err != nil {
			return err
		}
		source, err := attempt.service.executionInputSourceTx(ctx, tx, row)
		if err == nil && (source == nil || source.ResourceGeneration != "new-authorized-client-generation") {
			t.Fatal("foreground continuation discarded trusted resource grant")
		}
		return err
	})
}
