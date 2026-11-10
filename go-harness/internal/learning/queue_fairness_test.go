package learning

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestMaintenanceContinuesAllStagesAfterLargeUsage(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(t.TempDir() + "/usage.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	queue, err := skillmaintenance.New(ctx, db.DB(), skillmaintenance.Policy{MaxJobSeconds: 60, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	day := time.Now().UTC().Format("2006-01-02")
	if _, err = db.DB().Exec(`INSERT INTO df_skill_maintenance_days VALUES(?,1000000,1000000000000)`, day); err != nil {
		t.Fatal(err)
	}
	kinds := []skillmaintenance.JobKind{skillmaintenance.ExperienceExtract, skillmaintenance.ExperienceConsolidate, skillmaintenance.SkillMatch}
	for _, kind := range kinds {
		_, err := queue.Enqueue(ctx, skillmaintenance.Request{Kind: kind, OwnerKind: skillmaintenance.WorkspaceOwner, OwnerKey: strings.Repeat("a", 64), Key: string(kind), Reference: string(kind), InputDigest: strings.Repeat("b", 64)})
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := map[skillmaintenance.JobKind]bool{}
	for range kinds {
		claim, ok, err := queue.ClaimKinds(ctx, kinds)
		if err != nil || !ok {
			t.Fatalf("claim: %v %v", ok, err)
		}
		broker := queue.Broker(claim)
		receipt, err := broker.Reserve(ctx, skillmaintenance.ModelCall{Stage: string(claim.Job.Request.Kind), ModelIdentity: strings.Repeat("c", 64), InputDigest: strings.Repeat("d", 64), InputTokens: 100, MaxOutputTokens: 100})
		if err != nil {
			t.Fatal(err)
		}
		// Missing provider usage is not a quota failure or an invalid result.
		if err = broker.Settle(ctx, receipt, skillmaintenance.Usage{}); err != nil {
			t.Fatal(err)
		}
		if err = queue.Finish(ctx, claim, "", false); err != nil {
			t.Fatal(err)
		}
		seen[claim.Job.Request.Kind] = true
	}
	if len(seen) != len(kinds) {
		t.Fatalf("stages starved: %v", seen)
	}
}
