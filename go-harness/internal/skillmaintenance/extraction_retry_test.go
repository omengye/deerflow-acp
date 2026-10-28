package skillmaintenance

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestExtractionRetryKeepsLifetimeAccountingAndQueueCap(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, policy())
	r := Request{Kind: ExperienceExtract, OwnerKind: WorkspaceOwner, OwnerKey: strings.Repeat("a", 64), Key: "extract", Reference: "extract", InputDigest: strings.Repeat("b", 64)}
	j, err := f.store.Enqueue(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	c, ok, err := f.store.ClaimKinds(ctx, []JobKind{ExperienceExtract})
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	receipt, err := f.store.Broker(c).Reserve(ctx, call("experience_extract"))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.store.Finish(ctx, c, "model_failed", false); err != nil {
		t.Fatal(err)
	}
	j, err = f.store.Get(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := f.store.DayUsage(ctx, receipt.Day)
	if err != nil {
		t.Fatal(err)
	}
	// Unsettled reservations remain fully charged across explicit retry.
	tx, err := f.db.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	retried, err := f.store.RetryFailedExtractionTx(ctx, tx, j.ID, j.StateVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	after, err := f.store.DayUsage(ctx, receipt.Day)
	if err != nil || before != after || retried.Attempts != j.Attempts || !retried.Deadline.Equal(j.Deadline) || retried.Policy != j.Policy {
		t.Fatal(retried, after, err)
	}
	c, ok, err = f.store.ClaimKinds(ctx, []JobKind{ExperienceExtract})
	if err != nil || !ok || c.Job.Attempts != j.Attempts+1 {
		t.Fatal(c, ok, err)
	}
	if err = f.store.Finish(ctx, c, "model_failed", false); err != nil {
		t.Fatal(err)
	}
	j, err = f.store.Get(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		r.Key, r.Reference = fmt.Sprintf("queue-%d", i), fmt.Sprintf("queue-%d", i)
		if _, err = f.store.Enqueue(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	tx, err = f.db.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.RetryFailedExtractionTx(ctx, tx, j.ID, j.StateVersion); err != ErrBudget {
		t.Fatal("retry bypassed learning queue cap", err)
	}
	_ = tx.Rollback()
	unchanged, err := f.store.Get(ctx, j.ID)
	if err != nil || unchanged != j {
		t.Fatal("failed retry changed job", unchanged, err)
	}
}

func TestExtractionRetryRejectsBudgetConsumedByAnotherProducer(t *testing.T) {
	ctx := context.Background()
	p := policy()
	p.MaxDailyModelCalls = 2
	f := newFixture(t, p)
	r := Request{Kind: ExperienceExtract, OwnerKind: WorkspaceOwner, OwnerKey: strings.Repeat("a", 64), Key: "extract", Reference: "extract", InputDigest: strings.Repeat("b", 64)}
	j, err := f.store.Enqueue(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	c, ok, err := f.store.ClaimKinds(ctx, []JobKind{ExperienceExtract})
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, err = f.store.Broker(c).Reserve(ctx, call("experience_extract")); err != nil {
		t.Fatal(err)
	}
	if err = f.store.Finish(ctx, c, "model_failed", false); err != nil {
		t.Fatal(err)
	}
	enqueue(t, f.store, "generation")
	generation := claim(t, f.store)
	if _, err = f.store.Broker(generation).Reserve(ctx, call("generation")); err != nil {
		t.Fatal(err)
	}
	if err = f.store.Finish(ctx, generation, "model_failed", false); err != nil {
		t.Fatal(err)
	}
	j, err = f.store.Get(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.db.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	available, err := f.store.BudgetAvailableTx(ctx, tx, j)
	if err != nil || available {
		t.Fatal("shared budget reported available", available, err)
	}
	if _, err = f.store.RetryFailedExtractionTx(ctx, tx, j.ID, j.StateVersion); err != ErrBudget {
		t.Fatal("retry bypassed shared exhaustion", err)
	}
	_ = tx.Rollback()
	unchanged, err := f.store.Get(ctx, j.ID)
	if err != nil || unchanged != j {
		t.Fatal("budget rejection changed retry identity", unchanged, err)
	}
}
