package deerflow

import (
	"context"
	"errors"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
)

func TestSkillCatalogDirectDeleteWithoutEvolution(t *testing.T) {
	_, cfg := skillFixture(t)
	cfg.Engine = engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		return harness.RunResult{}, nil
	})
	cfg.Skills.Install = []harness.SkillInstall{{SourceID: "project", Directory: "research", Enabled: true}}
	cfg.Skills.BuiltinSources = []string{"project"}
	c, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	page, err := c.QuerySkillCatalog(context.Background(), harness.SkillQuery{Status: "current", Limit: 10})
	if err != nil || len(page.Skills) != 1 {
		t.Fatalf("catalog: %+v %v", page, err)
	}
	entry := page.Skills[0]
	request := map[string]any{"operation": "skill.delete", "target": entry.Target, "expected_head": entry.Head, "request_key": "desktop-delete"}
	for _, field := range []string{"target", "expected_head", "request_key"} {
		bad := map[string]any{}
		for key, value := range request {
			if key != field {
				bad[key] = value
			}
		}
		if _, err := skillManagementCall(t, c, bad); err == nil {
			t.Fatalf("deletion without %s accepted", field)
		}
	}
	request["actor"] = "forged"
	if _, err := skillManagementCall(t, c, request); err == nil {
		t.Fatal("caller-controlled actor accepted")
	}
	delete(request, "actor")
	value, err := skillManagementCall(t, c, request)
	if err != nil {
		t.Fatal(err)
	}
	result := value.(harness.SkillManagementResult)
	if result.Skill.Head.Exists || result.AppliedHead.Exists || result.Replayed {
		t.Fatalf("direct deletion not applied: %+v", result)
	}
	value, err = skillManagementCall(t, c, request)
	if err != nil || !value.(harness.SkillManagementResult).Replayed {
		t.Fatalf("retry: %+v %v", value, err)
	}
	request["request_key"] = "stale-list"
	_, err = skillManagementCall(t, c, request)
	var conflict *localhost.ManagementError
	if !errors.As(err, &conflict) || conflict.Code != "conflict" {
		t.Fatalf("stale list accepted: %v", err)
	}
}
