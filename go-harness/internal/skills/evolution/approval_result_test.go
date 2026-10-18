package evolution

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestApprovalAdmissionConflictReturnsDurableProposalWithoutApproving(t *testing.T) {
	f := newFixture(t, nil)
	p := f.submit(t, f.request(t, "admission-conflict", "Reviewed next steps."))
	before := f.head(t)
	decision := review(p)
	decision.ExpectedStateVersion++
	got, err := f.service.Approve(context.Background(), p.ID, decision)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("stale decision accepted: %+v %v", got, err)
	}
	saved, err := f.service.Get(context.Background(), p.ID)
	if err != nil || !reflect.DeepEqual(got, saved) {
		t.Fatalf("admission lost durable proposal: returned=%+v stored=%+v err=%v", got, saved, err)
	}
	if got.ID != p.ID || got.Status != "pending_review" || got.StateVersion != p.StateVersion || got.Approval != nil || f.head(t) != before {
		t.Fatalf("rejected admission changed proposal or installed head: %+v", got)
	}
}
