package learning

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestLearningSoftSharesBorrowAndDeferWithoutClaim(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(t.TempDir() + "/shares.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &Store{options: harness.SkillLearningOptions{MaxDailyModelCalls: 10, MaxDailyTokens: 1000}, now: time.Now}
	queue, err := skillmaintenance.NewWithHooks(ctx, db.DB(), skillmaintenance.Policy{MaxDailyModelCalls: 100, MaxDailyTokens: 100000, MaxJobSeconds: 60, Workers: 1}, skillmaintenance.Hooks{Ready: s.budgetReadyTx})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.DB().Exec(`CREATE TABLE df_learning_extractions(id TEXT,job_id TEXT,payload BLOB,payload_sha TEXT,result_sha TEXT)`); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]int{"max_daily_model_calls": 10, "max_daily_tokens": 1000})
	request := func(kind skillmaintenance.JobKind, key string) skillmaintenance.Request {
		return skillmaintenance.Request{Kind: kind, OwnerKind: skillmaintenance.WorkspaceOwner, OwnerKey: strings.Repeat("a", 64), Key: key, Reference: key, InputDigest: digest(payload)}
	}
	// Spend 400 tokens but only one call, proving token shares participate.
	prior, err := queue.Enqueue(ctx, request(skillmaintenance.ExperienceExtract, "prior"))
	if err != nil {
		t.Fatal(err)
	}
	c, ok, err := queue.ClaimKinds(ctx, []skillmaintenance.JobKind{skillmaintenance.ExperienceExtract})
	if err != nil || !ok || c.Job.ID != prior.ID {
		t.Fatal(c, ok, err)
	}
	b := queue.Broker(c)
	receipt, err := b.Reserve(ctx, skillmaintenance.ModelCall{Stage: "experience_extract", ModelIdentity: strings.Repeat("c", 64), InputDigest: strings.Repeat("d", 64), InputTokens: 100, MaxOutputTokens: 300})
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Settle(ctx, receipt, skillmaintenance.Usage{}); err != nil {
		t.Fatal(err)
	}
	if err = queue.Finish(ctx, c, "", false); err != nil {
		t.Fatal(err)
	}
	j, err := queue.Enqueue(ctx, request(skillmaintenance.ExperienceExtract, "next"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.DB().Exec(`INSERT INTO df_learning_extractions VALUES(?,?,?,?, '')`, j.Request.Reference, j.ID, payload, digest(payload)); err != nil {
		t.Fatal(err)
	}
	ready := func() bool {
		tx, err := db.DB().BeginTx(ctx, &sql.TxOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		ok, err := s.budgetReadyTx(ctx, tx, j)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !ready() {
		t.Fatal("idle shares not borrowed")
	}
	for _, kind := range []skillmaintenance.JobKind{skillmaintenance.ExperienceConsolidate, skillmaintenance.SkillMatch} {
		if _, err = queue.Enqueue(ctx, request(kind, string(kind))); err != nil {
			t.Fatal(err)
		}
	}
	if ready() {
		t.Fatal("token reservations ignored")
	}
	if _, ok, err = queue.ClaimKinds(ctx, []skillmaintenance.JobKind{skillmaintenance.ExperienceExtract}); err != nil || ok {
		t.Fatal("deferred work claimed", ok, err)
	}
	saved, err := queue.Get(ctx, j.ID)
	if err != nil || saved.Attempts != 0 || !saved.Deadline.IsZero() || saved.State != "queued" {
		t.Fatal(saved, err)
	}
	if _, err = db.DB().Exec(`UPDATE df_learning_extractions SET result_sha='committed'`); err != nil {
		t.Fatal(err)
	}
	if !ready() {
		t.Fatal("durable result ACK blocked")
	}
}
