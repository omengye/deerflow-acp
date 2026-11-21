package channels

import (
	"context"
	"fmt"
	"testing"

	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
)

func TestChannelSessionStatusUsesLatestRunAndCheckpoint(t *testing.T) {
	ctx := context.Background()
	m, i := fixture(t, nil)
	if _, err := hr.NewStore(ctx, m.db); err != nil {
		t.Fatal(err)
	}
	_, err := m.db.Exec(`INSERT INTO harness_sessions(id,cwd,title,mode,model,created_at,updated_at) VALUES('s','workspace','任务','default','fixture','2026-01-01','2026-01-01');
INSERT INTO harness_channel_messages(instance,account,id,conversation,sender,context,text,status,session_id,client_id,workspace,created_ms) VALUES('test','bot','in','chat','alice','','prompt','sent','s','final','workspace',1);
INSERT INTO harness_runs(id,session_id,input_id,status,created_at,updated_at) VALUES('old','s','old-in','completed','2026-01-01','2026-01-01'),('r','s','in','waiting_input','2026-01-02','2026-01-02');
INSERT INTO harness_inputs(id,run_id,content) VALUES('in','r','[]');
INSERT INTO harness_executions(run_id,session_id,input_id,status,version,attempt_id,attempt,root_budget_id,config,input_digest,manifest,created_at,updated_at) VALUES('r','s','in','waiting_input',1,'attempt',1,'r','{}','','{"CheckpointSHA":"current","TaskWaits":[{}]}','2026-01-02','2026-01-02');
INSERT INTO harness_execution_wakeups VALUES('r','older-checkpoint',3,0)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ sql, want string }{
		{"", "waiting_tasks"},
		{`UPDATE harness_execution_wakeups SET checkpoint_sha='current'`, "blocked"},
		{`UPDATE harness_executions SET manifest='{"CheckpointSHA":"next","Interrupts":[{}],"TaskWaits":[{}]}'`, "waiting_approval"},
		{`UPDATE harness_executions SET status='completed'`, "completed"},
	} {
		if tc.sql != "" {
			if _, err := m.db.Exec(tc.sql); err != nil {
				t.Fatal(err)
			}
		}
		items, err := m.Sessions(ctx, i.id)
		if err != nil || len(items) != 1 || items[0].Status != tc.want {
			t.Fatal(items, tc.want, err)
		}
	}
	page, err := m.SessionMessages(ctx, i.id, "bot", "s", 0)
	if err != nil || len(page.Items) != 1 || page.Items[0].Status != "sent" {
		t.Fatal("lost delivery status", page, err)
	}
}

func TestSessionHistoryGroupsBeforeLimitAndKeepsMessageScope(t *testing.T) {
	ctx := context.Background()
	m, i := fixture(t, nil)
	if _, err := hr.NewStore(ctx, m.db); err != nil {
		t.Fatal(err)
	}
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	insert := func(account, id, session, text, status string, ms int) {
		t.Helper()
		_, err := tx.Exec(`INSERT INTO harness_channel_messages(instance,account,id,conversation,sender,context,text,status,session_id,client_id,workspace,created_ms) VALUES(?,?,?,'chat','alice','',?,?,?,?,?,?)`, i.id, account, id, text, status, session, id, i.cfg.Workspace, ms)
		if err != nil {
			t.Fatal(err)
		}
	}
	for n := range 101 {
		insert("bot", fmt.Sprint(n), fmt.Sprintf("older-%03d", n), "older prompt", "sent", n)
	}
	insert("bot", "start", "busy", "/new", "sent", 200)
	insert("bot", "approval", "busy", "/approve\tPRIVATE", "sent", 201)
	insert("bot", "first", "busy", "https://example.org/?q=private  播客概要", "sent", 202)
	for n := range 120 {
		insert("bot", fmt.Sprintf("q%d", n), "busy", "消息", "sent", 203+n)
	}
	insert("second", "same", "busy", "/help", "unknown", 400)
	insert("bot", "same", "", "", "unknown", 500)
	if _, err = tx.Exec(`INSERT INTO harness_sessions(id,cwd,title,mode,model,created_at,updated_at) VALUES('older-100',?,'已保存的标题','default','model','2020-01-01','2020-01-01')`, i.cfg.Workspace); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	items, err := m.Sessions(ctx, i.id)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 101 || items[0].Account != "second" || items[0].Title != "渠道会话 busy" || items[1].Title != "播客概要" || items[1].Messages != 123 || items[2].Title != "已保存的标题" || items[99].SessionID != "older-003" || items[100].SessionID != "" {
		t.Fatalf("wrong session groups: %+v", items)
	}
	var before int64
	seen := map[string]bool{}
	for _, want := range []int{50, 50, 23} {
		page, err := m.SessionMessages(ctx, i.id, "bot", "busy", before)
		if err != nil || len(page.Items) != want {
			t.Fatal(page, err)
		}
		for _, msg := range page.Items {
			if seen[msg.ID] || msg.ID == "same" || msg.Status != "sent" || msg.ID == "approval" && msg.Preview != "/approve" {
				t.Fatalf("wrong message: %+v", msg)
			}
			seen[msg.ID] = true
		}
		before = page.NextBefore
	}
	if before != 0 || len(seen) != 123 {
		t.Fatal("last page", before, len(seen))
	}
	if err = m.ReviewMessage(ctx, i.id, "second", "same"); err != nil {
		t.Fatal(err)
	}
	p, err := m.SessionMessages(ctx, i.id, "bot", "", 0)
	if err != nil || len(p.Items) != 1 || p.Items[0].Status != "unknown" {
		t.Fatal("unbound delivery lost or wrong account reviewed", p, err)
	}
}
