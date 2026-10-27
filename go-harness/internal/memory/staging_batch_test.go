package memory

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestMemoryLegacyBatchDoesNotConflictWithItsOwnInserts(t *testing.T) {
	s, _ := personalStore(t)
	ctx := context.Background()
	scope, err := NewScope(WorkspaceScope, t.TempDir(), "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		_, err = s.db.ExecContext(ctx, `INSERT INTO memory_staged_facts(run_id,input_id,attempt_id,ordinal,scope_key,scope_kind,workspace,subject,agent,scope_revision,content,category,confidence,source_id,source_event_sequence,policy_version) VALUES('run','input','attempt',?,?,?,?,?,?,0,?,'project',1,?,1,'test')`, i, scope.key, scope.kind, scope.workspace, "", "", fmt.Sprintf("Independent project fact %d", i), fmt.Sprintf("source-%d", i))
		if err != nil {
			t.Fatal(err)
		}
	}
	var count int
	err = transact(ctx, s.db, func(tx *sql.Tx) error {
		var e error
		count, e = s.SettleStagingTx(ctx, tx, "run", "attempt", true)
		return e
	})
	if err != nil || count != 2 {
		t.Fatalf("batch count=%d error=%v", count, err)
	}
	page, err := s.List(ctx, scope, "", 100)
	if err != nil || len(page.Facts) != 2 {
		t.Fatal("lost independent batch fact", err)
	}
}
