package evolution

import (
	"context"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

func TestCandidateSnapshotCancellationDoesNotReviveWaitingJob(t *testing.T) {
	ctx := context.Background()
	f, _, _, _ := submittedFixture(t, false)
	p, err := f.service.SubmitAutomatic(ctx, f.request(t, "mobile-cancel", "Verify sources before summarizing"), f.cfg.Sources[0].Workspace)
	if err != nil {
		t.Fatal(err)
	}
	generationRun(t, f.service)
	p, progress, err := f.service.CandidateSnapshot(ctx, p.ID)
	if err != nil || !progress.CanCancel || progress.Status != "blocked_evidence" || progress.StateVersion != p.StateVersion {
		t.Fatal("waiting candidate snapshot invalid", progress, err)
	}
	if _, err = f.service.Reject(ctx, p.ID, review(p)); err != nil {
		t.Fatal(err)
	}
	p, progress, err = f.service.CandidateSnapshot(ctx, p.ID)
	if err != nil || progress.Status != "rejected" || progress.CanCancel || progress.StateVersion != p.StateVersion {
		t.Fatal("cancelled candidate resurrected by its old waiting job", progress, err)
	}
}

func TestCandidateSnapshotRetiredAutomaticCandidateIsHistorical(t *testing.T) {
	ctx := context.Background()
	f, _, _, _ := submittedFixture(t, true)
	p, err := f.service.SubmitAutomatic(ctx, f.request(t, "mobile-retired", "Verify sources before summarizing"), f.cfg.Sources[0].Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.db.DB().Exec(`INSERT INTO df_evolution_retired_proposals VALUES(?)`, p.ID); err != nil {
		t.Fatal(err)
	}
	_, progress, err := f.service.CandidateSnapshot(ctx, p.ID)
	if err != nil || progress.Status != "historical" || progress.CanCancel {
		t.Fatal("automatic origin hid retired pipeline", progress, err)
	}
}

func TestCandidateSnapshotObservationChangesWithoutCandidateVersionChange(t *testing.T) {
	ctx := context.Background()
	f, _, _, _ := submittedFixture(t, true)
	p, err := f.service.SubmitAutomatic(ctx, f.request(t, "mobile-observation", "Verify sources before summarizing"), f.cfg.Sources[0].Workspace)
	if err != nil {
		t.Fatal(err)
	}
	generationRun(t, f.service)
	if err = f.service.RunAutoPatch(ctx); err != nil {
		t.Fatal(err)
	}
	p, progress, err := f.service.CandidateSnapshot(ctx, p.ID)
	if err != nil || progress.Status != "observing" || progress.Observation == nil || progress.CanCancel {
		t.Fatal("published candidate lost observation", progress, err)
	}
	version := p.StateVersion
	err = f.service.withProposal(ctx, p.ID, func(tx *skills.PublicationTx, _ *Proposal) error {
		o := *progress.Observation
		o.Status, o.Reason = "rolled_back", "regression"
		return writeAutoPatchObservation(ctx, tx.SQL(), o, false)
	})
	if err != nil {
		t.Fatal(err)
	}
	p, progress, err = f.service.CandidateSnapshot(ctx, p.ID)
	if err != nil || p.StateVersion != version || progress.Status != "withdrawn" ||
		progress.Observation == nil || progress.Observation.Reason != "regression" {
		t.Fatal("same-version observation was hidden", progress, err)
	}
}
