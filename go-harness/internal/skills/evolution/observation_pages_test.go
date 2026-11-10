package evolution

import (
	"context"
	"fmt"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"testing"
	"time"
)

func TestObservationPagesIncludeLateRootsAndInvalidateWithdrawalAndDeletion(t *testing.T) {
	ctx := context.Background()
	f := newCollectorFixture(t)
	p := f.submit(t, f.request(t, "page-observation", "Bounded observation fixture"))
	o := harness.SkillAutoPatchObservation{ProposalID: p.ID, Target: p.Target, Head: p.Base, Status: "observing", StartedAt: time.Now().Add(-time.Hour), Deadline: time.Now().Add(time.Hour)}
	tx, err := f.db.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = writeAutoPatchObservation(ctx, tx, o, true); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	add := func(i int) harness.SkillSignalEvidence {
		run := f.run(t, "", 1, "completed", false)
		f.drain(t, 128)
		e := feedbackEvidence(t, f, run)
		req := feedbackRequest(e, fmt.Sprintf("page-%d", i))
		req.Kind = "helpful"
		mustSubmitFeedback(t, f.service, req)
		return e
	}
	var first harness.SkillSignalEvidence
	for i := range 129 {
		e := add(i)
		if i == 0 {
			first = e
		}
	}
	collect := func() bool {
		tx, err := f.db.DB().BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		complete, critical, err := f.service.collectObservationPageTx(ctx, tx, &o)
		if err != nil || critical {
			t.Fatal("observation scan", err, critical)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return complete
	}
	if collect() || o.Successes != 128 {
		t.Fatal("incomplete page claimed a full census", o.Successes)
	}
	add(129)
	complete := false
	for range 4 {
		complete = collect()
		if complete {
			break
		}
	}
	if !complete || o.Successes != 130 {
		t.Fatal("late root lost behind cursor", complete, o.Successes)
	}
	req := feedbackRequest(first, "withdraw-page-vote")
	req.ExpectedFeedbackVersion = 1
	req.Kind = "withdrawn"
	req.Summary = ""
	req.ReadIDs = nil
	mustSubmitFeedback(t, f.service, req)
	for range 4 {
		if collect() {
			break
		}
	}
	if o.Successes != 129 || o.Ignored != 1 {
		t.Fatal("withdrawn feedback retained a vote", o.Successes, o.Ignored)
	}
	if _, err = f.db.DB().Exec(`DELETE FROM df_skill_signals WHERE id=?`, first.Signal.ID); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if collect() {
			break
		}
	}
	if o.Successes != 129 || o.Ignored != 0 {
		t.Fatal("deleted source retained an observation", o.Successes, o.Ignored)
	}
}
