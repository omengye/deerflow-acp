package skillmaintenance

import (
	"context"
	"testing"
)

func TestProgressSlicesRequireNewCommittedEvidence(t *testing.T) {
	f := newFixture(t, policy())
	j := enqueue(t, f.store, "slice")
	c := claim(t, f.store)
	ctx := WithBroker(context.Background(), f.store.Broker(c))
	if err := f.store.Finish(ctx, c, "yield_progress", false); err != ErrConflict {
		t.Fatal("no-progress slice renewed", err)
	}
	tx, err := f.store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = MarkProgressTx(ctx, tx, j.ID, "case/1"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = f.store.Finish(ctx, c, "yield_progress", false); err != nil {
		t.Fatal(err)
	}
	next := claim(t, f.store)
	if next.Job.Attempts != 1 || next.Job.ID != j.ID {
		t.Fatal(next.Job)
	}
	ctx = WithBroker(context.Background(), f.store.Broker(next))
	tx, err = f.store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = MarkProgressTx(ctx, tx, j.ID, "case/1"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = f.store.Finish(ctx, next, "yield_progress", false); err != ErrConflict {
		t.Fatal("replayed evidence renewed the slice", err)
	}
}

func TestWaitingRuntimeWakesOnlyOnDependencyChange(t *testing.T) {
	f := newFixture(t, policy())
	enqueue(t, f.store, "runtime")
	c := claim(t, f.store)
	ctx := context.Background()
	if err := f.store.Finish(ctx, c, "blocked_runtime", false); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := f.store.Claim(ctx); err != nil || ok {
		t.Fatal("waiting job polled a provider", ok, err)
	}
	if err := f.store.WakeChanged(ctx, "blocked_runtime", "image-v1"); err != nil {
		t.Fatal(err)
	}
	c = claim(t, f.store)
	if err := f.store.Finish(ctx, c, "blocked_runtime", false); err != nil {
		t.Fatal(err)
	}
	if err := f.store.WakeChanged(ctx, "blocked_runtime", "image-v1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := f.store.Claim(ctx); err != nil || ok {
		t.Fatal("unchanged missing dependency retried", ok, err)
	}
	if err := f.store.WakeChanged(ctx, "blocked_runtime", "image-v2"); err != nil {
		t.Fatal(err)
	}
	claim(t, f.store)
}

func TestEpochRejectsAnotherConfigJournal(t *testing.T) {
	f := newFixture(t, policy())
	ctx := context.Background()
	if _, err := f.store.db.Exec(`CREATE TABLE harness_events(sequence INTEGER);CREATE TABLE harness_runs(id TEXT,session_id TEXT,input_id TEXT)`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"journal-a", "journal-a", "journal-b"} {
		tx, err := f.store.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		err = BeginAutomaticEpoch(WithMigrationID(ctx, id), tx)
		if id == "journal-b" {
			tx.Rollback()
			if err == nil {
				t.Fatal("mismatched config journal admitted")
			}
			continue
		}
		if err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
}
