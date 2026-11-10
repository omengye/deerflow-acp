package skillmaintenance

import (
	"context"
	"testing"
)

func TestAutomaticEpochIsDurableAndFencesOldClaims(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, policy())
	old := enqueue(t, f.store, "old")
	claimed := claim(t, f.store)
	if _, err := f.store.db.Exec(`CREATE TABLE harness_events(sequence INTEGER); INSERT INTO harness_events VALUES(7);
 CREATE TABLE harness_runs(id TEXT,session_id TEXT,input_id TEXT); INSERT INTO harness_runs VALUES('old-root','chat','old-input');`); err != nil {
		t.Fatal(err)
	}
	tx, err := f.store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = BeginAutomaticEpoch(ctx, tx); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.Broker(claimed).Reserve(ctx, call("generation")); err != ErrConflict {
		t.Fatal("old lease retained authority", err)
	}
	saved, err := f.store.Get(ctx, old.ID)
	if err != nil || saved.State != "cancelled" || saved.ErrorCode != "historical_epoch" {
		t.Fatal(saved, err)
	}
	if _, err = f.store.db.Exec(`INSERT INTO harness_runs VALUES('new-root','chat','new-input'); INSERT INTO harness_events VALUES(8)`); err != nil {
		t.Fatal(err)
	}
	fresh := enqueue(t, f.store, "new")
	tx, err = f.store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = BeginAutomaticEpoch(ctx, tx); err != nil {
		t.Fatal(err)
	}
	var watermark int
	if err = tx.QueryRow(`SELECT input_watermark FROM df_evolution_epoch`).Scan(&watermark); err != nil || watermark != 7 {
		t.Fatal(watermark, err)
	}
	historical, err := HistoricalRootTx(ctx, tx, "new-root")
	if err != nil || historical {
		t.Fatal("restart moved epoch", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	next := claim(t, f.store)
	if next.Job.ID != fresh.ID {
		t.Fatal("new work was retired", next.Job)
	}
	tx, err = f.store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = BeginAutomaticEpoch(WithMigrationID(ctx, "first-config"), tx); err != nil {
		t.Fatal("first config could not bind existing epoch", err)
	}
	if err = tx.QueryRow(`SELECT input_watermark FROM df_evolution_epoch`).Scan(&watermark); err != nil || watermark != 7 {
		t.Fatal("config binding reset watermark", watermark, err)
	}
	if err = BeginAutomaticEpoch(WithMigrationID(ctx, "unrelated-config"), tx); err == nil {
		t.Fatal("a bound configuration identity was replaced")
	}
}
