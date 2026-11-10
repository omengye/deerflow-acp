package runtime

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func queueCapacityTasks(t *testing.T, s *Service, x harness.Session, count int) {
	t.Helper()
	for i := range count {
		req := learningTestRun(t, s, x, fmt.Sprintf("Verify independent JSON document %d", i))
		for _, kind := range []string{"tool_start", "tool_execute", "tool_end"} {
			e := receiptEvent(req, kind, "complete-file", "read_file")
			if kind == "tool_start" {
				e.Arguments = []byte(`{"path":"configuration.json"}`)
			}
			if kind == "tool_end" {
				e.Content = []harness.Content{{Type: "text", Text: `{"verified":true}`}}
			}
			if _, err := s.Store.Append(context.Background(), e); err != nil {
				t.Fatal(err)
			}
		}
		learningTestFinish(t, s, req, "end_turn")
	}
	time.Sleep(1050 * time.Millisecond)
}

func drainCapacityTasks(t *testing.T, s *Service, count int) {
	t.Helper()
	ctx := context.Background()
	for range count {
		if worked, err := s.Store.Learning.QueueOne(ctx); err != nil || !worked {
			t.Fatal("pending input lost", worked, err)
		}
		if worked, err := s.Store.Learning.RunOne(ctx); err != nil || !worked {
			t.Fatal("extraction did not advance", worked, err)
		}
		// Directory/pattern scan pages may yield without enqueueing a model job.
		for page := 0; page < 4; page++ {
			if _, err := s.Store.Learning.QueueConsolidationOne(ctx); err != nil {
				t.Fatal(err)
			}
			if worked, err := s.Store.Learning.RunOne(ctx); err != nil {
				t.Fatal(err)
			} else if worked {
				break
			} else if page == 3 {
				t.Fatal("consolidation pagination stalled")
			}
		}
	}
}

func TestSkillLearningCapacity129PatternsAndDurableInputs(t *testing.T) {
	s, x, _, model := newConsolidationTest(t)
	model.relation = "distinct"
	queueCapacityTasks(t, s, x, 129)
	if got := learningCount(t, s.Store, "harness_learning_inputs"); got != 129 {
		t.Fatal("accepted input silently dropped", got)
	}
	drainCapacityTasks(t, s, 129)
	var count int
	if err := s.Store.db.QueryRow(`SELECT count(*) FROM df_learning_patterns`).Scan(&count); err != nil || count != 129 || len(model.seen) != 129 {
		t.Fatal("129th pattern blocked", count, len(model.seen), err)
	}
}

func TestSkillLearningLedgerBeyond32StillRejectsChangedSource(t *testing.T) {
	s, x, _, _, _ := newMatchingTest(t)
	queueCapacityTasks(t, s, x, 35)
	drainCapacityTasks(t, s, 35)
	id := experienceMatch(t, s)
	ctx := context.Background()
	if _, err := s.Store.Learning.Promotion(ctx, id, 2, time.Now().Add(-time.Hour), time.Now()); err != nil {
		t.Fatal("35 member pattern blocked", err)
	}
	var root string
	err := s.Store.db.QueryRow(`SELECT json_extract(CAST(x.payload AS TEXT),'$.sources[0].root')
 FROM df_learning_patterns p JOIN df_learning_pattern_revisions r ON r.pattern_id=p.id AND r.revision=p.current_revision
 JOIN df_learning_pattern_members m ON m.pattern_id=p.id JOIN df_learning_experiences e ON e.id=m.experience_id
 JOIN df_learning_extractions x ON x.id=e.extraction_id
 WHERE NOT EXISTS(SELECT 1 FROM json_each(CAST(r.payload AS TEXT),'$.members') sample WHERE json_extract(sample.value,'$.id')=m.experience_id) LIMIT 1`).Scan(&root)
	if err != nil || root == "" {
		t.Fatal("missing ledger member outside context sample", err)
	}
	if _, err = s.Store.db.Exec(`UPDATE harness_tool_receipts SET version=version+1 WHERE run_id IN (SELECT id FROM budget_members WHERE root_id=?)`, root); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Store.Learning.Promotion(ctx, id, 2, time.Now().Add(-time.Hour), time.Now()); err == nil {
		t.Fatal("changed source outside representative sample was ignored")
	}
}
