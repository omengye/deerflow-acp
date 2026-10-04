package deerflow

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSkillCollectorManagementRetryKeepsSessionUsable(t *testing.T) {
	ctx := context.Background()
	var runs atomic.Int32
	c, err := Open(ctx, Config{DataDir: t.TempDir(), Engine: engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		runs.Add(1)
		return harness.RunResult{StopReason: "end_turn"}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Drive the collector deterministically while testing the real SDK/session
	// and management boundary. Production lifecycle starts it automatically.
	if err = c.evolution.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = c.store.DB().Exec(`CREATE TRIGGER collector_fixture_fault BEFORE UPDATE OF processed ON harness_skill_outcomes WHEN NEW.processed=1 BEGIN SELECT RAISE(ABORT,'PRIVATE_ERROR_WITH_CREDENTIAL'); END`); err != nil {
		t.Fatal(err)
	}
	session, err := c.NewSession(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "first"}}, nil, nil); err != nil {
		t.Fatal("collector fault broke prompt", err)
	}
	for i := 0; i < 3; i++ {
		if _, err = c.evolution.CollectEvidence(ctx, 128); err == nil {
			t.Fatal("expected collector fault")
		}
		if _, err = c.store.DB().Exec(`UPDATE df_skill_collection_jobs SET retry_ns=0 WHERE state='retry'`); err != nil {
			t.Fatal(err)
		}
	}
	value, err := c.ManageLocal(ctx, localManagementRequest("skill.collector.status"))
	if err != nil {
		t.Fatal(err)
	}
	status := value.(harness.SkillCollectorStatus)
	if status.Failed != 1 || len(status.Jobs) != 1 || status.Jobs[0].ErrorCode != "storage_error" {
		t.Fatalf("status=%+v", status)
	}
	encoded, _ := json.Marshal(status)
	if strings.Contains(string(encoded), "PRIVATE") {
		t.Fatal("collector status exposed underlying database error")
	}
	if _, err = c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "next prompt"}}, nil, nil); err != nil {
		t.Fatal("failed collector blocked next prompt", err)
	}
	pageRequest := localManagementRequest("skill.collector.jobs")
	pageRequest.Fields["limit"] = json.RawMessage(`1`)
	jobs, err := c.ManageLocal(ctx, pageRequest)
	if err != nil || len(jobs.(harness.SkillCollectionJobPage).Jobs) != 1 || jobs.(harness.SkillCollectionJobPage).Next == 0 {
		t.Fatalf("jobs=%+v %v", jobs, err)
	}
	pageRequest.Fields["after_outcome_id"], _ = json.Marshal(jobs.(harness.SkillCollectionJobPage).Next)
	jobs, err = c.ManageLocal(ctx, pageRequest)
	if err != nil || len(jobs.(harness.SkillCollectionJobPage).Jobs) != 1 || jobs.(harness.SkillCollectionJobPage).Jobs[0].State != "unclaimed" {
		t.Fatalf("next jobs=%+v %v", jobs, err)
	}
	if _, err = c.ManageLocal(ctx, localManagementRequest("skill.collector.retry")); err == nil {
		t.Fatal("retry accepted missing preconditions")
	}
	request := localManagementRequest("skill.collector.retry")
	request.Fields["outcome_id"], _ = json.Marshal(status.Jobs[0].OutcomeID)
	request.Fields["expected_state_version"], _ = json.Marshal(status.Jobs[0].StateVersion)
	if _, err = c.ManageLocal(ctx, request); err != nil {
		t.Fatal(err)
	}
	if _, err = c.ManageLocal(ctx, request); err == nil {
		t.Fatal("duplicate retry reset durable attempts")
	}
	if _, err = c.store.DB().Exec(`DROP TRIGGER collector_fixture_fault`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		worked, err := c.evolution.CollectEvidence(ctx, 128)
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			break
		}
	}
	status, err = c.SkillCollectorStatus(ctx)
	if err != nil || status.Pending != 0 || status.Completed != 2 || runs.Load() != 2 {
		t.Fatalf("retry replayed user work: %+v runs=%d %v", status, runs.Load(), err)
	}
}
