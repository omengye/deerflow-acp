package evolution

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

func TestSkillLearningMaintenanceDispatcherPriorityAndAccounting(t *testing.T) {
	f, g, v := generationFixture(t)
	ctx := context.Background()
	var admissions, sublimits, terminals, handled int
	hooks := skillmaintenance.Hooks{
		Admission: func(_ context.Context, _ *sql.Tx, job skillmaintenance.Job) error {
			admissions++
			if job.Request.Kind != skillmaintenance.ExperienceExtract {
				t.Fatal("producer confusion")
			}
			return nil
		},
		CallAdmission: func(context.Context, *sql.Tx, skillmaintenance.Job, skillmaintenance.ModelCall, time.Time) error {
			sublimits++
			return nil
		},
		Terminal: func(context.Context, *sql.Tx, skillmaintenance.Job) error { terminals++; return nil },
	}
	if err := f.service.RegisterMaintenanceProducer(skillmaintenance.ExperienceExtract, hooks, func(ctx context.Context, job skillmaintenance.Job) (string, bool) {
		handled++
		if err := generationCharge(ctx, "experience_extract"); err != nil {
			t.Fatal(err)
		}
		tx, err := f.db.DB().BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = skillmaintenance.GuardTx(ctx, tx, job.ID); err != nil {
			t.Fatal(err)
		}
		_ = tx.Rollback()
		return "", false
	}); err != nil {
		t.Fatal(err)
	}
	job, err := f.service.MaintenanceQueue().Enqueue(ctx, skillmaintenance.Request{Kind: skillmaintenance.ExperienceExtract, OwnerKind: skillmaintenance.WorkspaceOwner, OwnerKey: strings.Repeat("a", 64), Key: "extract-first", Reference: "snapshot", InputDigest: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	queueGeneration(t, f, "explicit-priority")
	if worked, err := f.service.RunGeneration(ctx); err != nil || !worked {
		t.Fatal(worked, err)
	}
	if handled != 0 || g.calls.Load() != 1 || v.calls.Load() != 1 {
		t.Fatal("learning starved explicit skill maintenance")
	}
	var before int64
	if err := f.db.DB().QueryRow(`SELECT revision FROM df_skill_generation_catalog WHERE id=1`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if worked, err := f.service.RunGeneration(ctx); err != nil || !worked {
		t.Fatal(worked, err)
	}
	if handled != 1 || admissions != 1 || sublimits != 1 || terminals != 1 {
		t.Fatalf("typed hooks %d/%d/%d/%d", handled, admissions, sublimits, terminals)
	}
	var after int64
	if err := f.db.DB().QueryRow(`SELECT revision FROM df_skill_generation_catalog WHERE id=1`).Scan(&after); err != nil || after != before {
		t.Fatal("learning invalidated unrelated generation catalog", before, after, err)
	}
	receipts, err := f.service.MaintenanceQueue().Receipts(ctx, job.ID, 0, 10)
	if err != nil || len(receipts) != 1 || receipts[0].Stage != "experience_extract" {
		t.Fatal(receipts, err)
	}
	usage, err := f.service.MaintenanceQueue().DayUsage(ctx, time.Now().UTC().Format("2006-01-02"))
	if err != nil || usage.ModelCalls != 4 {
		t.Fatal(usage, err)
	}
}
