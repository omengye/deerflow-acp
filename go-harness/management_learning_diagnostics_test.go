package deerflow

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestManagementLearningDiagnosticsAndRetryStrictScope(t *testing.T) {
	ctx := context.Background()
	c, err := Open(ctx, Config{DataDir: t.TempDir(), Engine: engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		t.Fatal("diagnostics invoked foreground work")
		return harness.RunResult{}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	session, err := c.NewSession(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	request := localManagementFields("skill.learning.diagnostics", map[string]string{"session_id": session.ID})
	value, err := c.ManageLocal(ctx, request)
	d, ok := value.(harness.SkillLearningDiagnostics)
	if err != nil || !ok || d.SessionID != session.ID || d.Running || d.Contribute || len(d.Inputs) != 0 || len(d.Jobs) != 0 || d.Queue.Pending != 0 {
		t.Fatal(value, err)
	}
	for _, tc := range []struct{ key, raw string }{
		{"limit", "101"}, {"limit", "0"}, {"limit", "1.5"}, {"limit", "null"}, {"Limit", "1"}, {"include_prompts", "true"}, {"session_id", `"missing"`},
	} {
		bad := localManagementFields("skill.learning.diagnostics", map[string]string{"session_id": session.ID})
		bad.Fields[tc.key] = json.RawMessage(tc.raw)
		if _, err := c.ManageLocal(ctx, bad); err == nil {
			t.Fatal("invalid diagnostics accepted", tc.key, tc.raw)
		}
	}
	for _, fields := range []map[string]json.RawMessage{
		{"job_id": json.RawMessage(`"smj_missing"`)},
		{"job_id": json.RawMessage(`"smj_missing"`), "expected_epoch": json.RawMessage(`1`), "expected_state_version": json.RawMessage(`null`)},
		{"job_id": json.RawMessage(`"smj_missing"`), "expected_epoch": json.RawMessage(`0`), "expected_state_version": json.RawMessage(`1`)},
		{"job_id": json.RawMessage(`"smj_missing"`), "expected_epoch": json.RawMessage(`1`), "expected_state_version": json.RawMessage(`1`), "reset_budget": json.RawMessage(`true`)},
	} {
		bad := localManagementFields("skill.learning.extraction.retry", map[string]string{"session_id": session.ID})
		for k, v := range fields {
			bad.Fields[k] = v
		}
		if _, err := c.ManageLocal(ctx, bad); err == nil {
			t.Fatal("unguarded retry accepted", fields)
		}
	}
}
