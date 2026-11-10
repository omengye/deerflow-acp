package evolution

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestHistoricalObservationKeepsDeadlineAndWithdrawsWithoutReplay(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, nil)
	base := f.head(t)
	p := f.submit(t, f.request(t, "old-observation", "Historical adopted candidate"))
	adopted, err := f.service.Approve(ctx, p.ID, review(p))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Add(-73 * time.Hour)
	o := harness.SkillAutoPatchObservation{ProposalID: p.ID, Target: p.Target, PublicationRevision: adopted.PublishedRevision, BaselineRevision: adopted.PublishedRevision - 1, Head: f.head(t), Status: "observing", StartedAt: start, Deadline: start.Add(72 * time.Hour), UpdatedAt: start, Options: harness.SkillAutoPatchOptions{MaxChangedLines: 40, ObservationHours: 72, MinObservations: 3, RollbackFailureThreshold: 2}}
	raw, _ := json.Marshal(o)
	var legacy map[string]any
	json.Unmarshal(raw, &legacy)
	legacy["options"].(map[string]any)["max_daily_publications"] = 1
	legacy["options"].(map[string]any)["experience_patterns_enabled"] = false
	raw, _ = json.Marshal(legacy)
	if _, err = f.db.DB().Exec(`INSERT INTO df_skill_auto_patch_observations VALUES(?,?,?,?,?,?,?)`, o.ProposalID, o.Target.SourceIdentity, o.Target.Name, o.Status, o.StartedAt.UnixNano(), raw, hash(raw)); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(ctx, f.registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	before, err := readAutoPatchObservation(ctx, f.db.DB(), o.ProposalID)
	if err != nil || before.Deadline != o.Deadline {
		t.Fatal("historical deadline changed", err)
	}
	if err = reopened.RunAutoPatch(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := readAutoPatchObservation(ctx, f.db.DB(), o.ProposalID)
	if err != nil || after.Status != "rolled_back" || after.Deadline != o.Deadline || f.head(t).Version != base.Version {
		t.Fatal("historical observation was extended or treated as passed", after.Status, err)
	}
	var calls int
	if err = f.db.DB().QueryRow(`SELECT count(*) FROM df_skill_maintenance_calls`).Scan(&calls); err != nil || calls != 0 {
		t.Fatal("historical completion replayed model work", calls)
	}
}

func TestHistoricalDiscoveryRetainsDestinationWithoutRewritingAuditBytes(t *testing.T) {
	f, _, _ := discoveryFixture(t)
	discoveryCorrection(t, f, "", "legacy-a")
	discoveryCorrection(t, f, "", "legacy-b")
	discoverySteps(t, f.service, 3)
	d := discoveries(t, f.service)[0]
	var raw []byte
	if err := f.db.DB().QueryRow(`SELECT payload FROM df_skill_discoveries WHERE id=?`, d.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var old map[string]any
	json.Unmarshal(raw, &old)
	options := old["options"].(map[string]any)
	delete(options, "destination_source_id")
	options["new_skill_source_id"] = "project"
	options["max_daily_proposals"] = 5
	raw, _ = json.Marshal(old)
	sha := hash(raw)
	if _, err := f.db.DB().Exec(`UPDATE df_skill_discoveries SET payload=?,payload_sha=? WHERE id=?`, raw, sha, d.ID); err != nil {
		t.Fatal(err)
	}
	got, _, digest, err := readDiscovery(context.Background(), f.db.DB(), d.ID)
	if err != nil || got.Options.DestinationSourceID != "project" || digest != sha {
		t.Fatal("historical destination or audit hash lost", digest, err)
	}
}
