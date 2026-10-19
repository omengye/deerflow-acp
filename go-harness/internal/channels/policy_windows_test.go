package channels

import (
	"context"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/testutil"
)

func TestScheduleDispatchShortWorkspace(t *testing.T) {
	ctx := context.Background()
	m, i := fixture(t, nil)
	if _, err := m.SetPermissions(ctx, i.id, i.revision, harness.ApprovalAllowAlways); err != nil {
		t.Fatal(err)
	}
	d := Dispatch{i.id, i.auth.Account, "alice", "alice", testutil.ShortPath(t, i.cfg.Workspace), "message", i.revision}
	source := m.ScheduleSource(d, "session")
	if source.Workspace != i.cfg.Workspace || source.Grant == "" || !m.AuthorizeSchedule(source, true) {
		t.Fatal("short dispatch did not match pinned channel grant", source)
	}
	d.Workspace = testutil.ShortPath(t, testutil.CanonicalTempDir(t))
	if m.ScheduleSource(d, "foreign").Grant != "" {
		t.Fatal("foreign short path inherited channel grant")
	}
	if _, err := m.SetPermissions(ctx, i.id, i.revision, harness.ApprovalGlobal); err != nil {
		t.Fatal(err)
	}
	if m.AuthorizeSchedule(source, true) {
		t.Fatal("short-path dispatch survived grant revocation")
	}
}
