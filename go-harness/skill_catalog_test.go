package deerflow

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
)

func TestSkillCatalogAuthenticatedManagement(t *testing.T) {
	c, cfg := skillEvolutionClient(t, true)
	ctx := context.Background()
	host, err := localhost.New(localhost.Config{RuntimeDir: t.TempDir(), Manage: c.ManageLocal, ServeACP: func(context.Context, io.ReadCloser, io.WriteCloser) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	ep, err := host.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	call := func(fields map[string]any, expectedCode string, result any) {
		t.Helper()
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(ep.Host, fmt.Sprint(ep.Port)), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err = fmt.Fprintf(conn, "DFACP/1 %s MANAGE\n", ep.Token); err != nil {
			t.Fatal(err)
		}
		reader := bufio.NewReader(conn)
		if line, err := reader.ReadString('\n'); err != nil || strings.TrimSpace(line) != "OK" {
			t.Fatalf("handshake: %q %v", line, err)
		}
		if err = json.NewEncoder(conn).Encode(fields); err != nil {
			t.Fatal(err)
		}
		var response struct {
			OK   bool            `json:"ok"`
			Code string          `json:"code"`
			Data json.RawMessage `json:"data"`
		}
		if err = json.NewDecoder(reader).Decode(&response); err != nil {
			t.Fatal(err)
		}
		if response.OK != (expectedCode == "") || response.Code != expectedCode {
			t.Fatalf("response: %+v", response)
		}
		if result != nil {
			if err = json.Unmarshal(response.Data, result); err != nil {
				t.Fatal(err)
			}
		}
	}
	var page harness.SkillCatalogPage
	call(map[string]any{"operation": "skill.list"}, "", &page)
	if len(page.Skills) != 1 || len(page.Sources) != 1 || !page.Sources[0].Builtin || page.Skills[0].Record == nil {
		t.Fatalf("catalog: %+v", page)
	}
	entry := page.Skills[0]
	var detail harness.SkillCatalogDetail
	call(map[string]any{"operation": "skill.get", "target": entry.Target}, "", &detail)
	if detail.Skill.Head != entry.Head || detail.CatalogVersion != page.CatalogVersion {
		t.Fatalf("detail: %+v", detail)
	}
	disable := map[string]any{"operation": "skill.set_enabled", "target": entry.Target, "expected_head": entry.Head, "enabled": false, "request_key": "desktop-disable"}
	var applied harness.SkillManagementResult
	call(disable, "", &applied)
	if applied.Replayed || applied.Skill.Head.Enabled || !applied.Skill.Head.Managed || applied.Skill.OverlayBase != nil {
		t.Fatalf("enablement: %+v", applied)
	}
	call(disable, "", &applied)
	if !applied.Replayed {
		t.Fatal("MANAGE retry was not idempotent")
	}
	disable["request_key"] = "stale-page"
	call(disable, "conflict", nil)
	call(map[string]any{"operation": "skill.imports.refresh", "target": entry.Target, "expected_head": applied.Skill.Head}, "", &detail)
	if detail.Skill.SourceStatus != "ready" || detail.Skill.Head != applied.Skill.Head {
		t.Fatalf("refresh: %+v", detail)
	}
	call(map[string]any{"operation": "skill.list", "status": "disabled"}, "", &page)
	if len(page.Skills) != 1 || page.Skills[0].SourceStatus != "ready" {
		t.Fatalf("disabled filter/observation: %+v", page)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c2, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	current, err := c2.GetCatalogSkill(ctx, entry.Target)
	if err != nil || current.Skill.Head != applied.AppliedHead {
		t.Fatalf("restart lost desktop decision: %+v %v", current, err)
	}
}

func TestSkillCatalogManagementRejectsIncompleteAndUnknownFields(t *testing.T) {
	c, _ := skillEvolutionClient(t)
	page, err := c.QuerySkillCatalog(context.Background(), harness.SkillQuery{Limit: 10})
	if err != nil || len(page.Skills) != 1 {
		t.Fatalf("catalog: %+v %v", page, err)
	}
	entry := page.Skills[0]
	base := map[string]any{"operation": "skill.set_enabled", "target": entry.Target, "expected_head": entry.Head, "enabled": false, "request_key": "must-not-apply"}
	for _, field := range []string{"target", "expected_head", "enabled", "request_key"} {
		for _, mode := range []string{"missing", "null"} {
			t.Run(field+"-"+mode, func(t *testing.T) {
				fields := map[string]any{}
				for k, v := range base {
					fields[k] = v
				}
				if mode == "missing" {
					delete(fields, field)
				} else {
					fields[field] = nil
				}
				_, err := skillManagementCall(t, c, fields)
				var known *localhost.ManagementError
				if !errors.As(err, &known) || known.Code != "invalid_request" {
					t.Fatalf("incomplete command accepted: %v", err)
				}
			})
		}
	}
	for _, bad := range []map[string]any{
		{"operation": "skill.list", "status": "published"},
		{"operation": "skill.list", "limit": 101},
		{"operation": "skill.get", "target": entry.Target, "actor": "forged"},
		{"operation": "skill.set_enabled", "target": entry.Target, "expected_head": map[string]any{"version": entry.Head.Version, "state_version": entry.Head.StateVersion}, "enabled": false, "request_key": "incomplete-head"},
		{"operation": "skill.set_enabled", "target": entry.Target, "expected_head": entry.Head, "enabled": "false", "request_key": "string-boolean"},
	} {
		_, err := skillManagementCall(t, c, bad)
		var known *localhost.ManagementError
		if !errors.As(err, &known) || known.Code != "invalid_request" {
			t.Fatalf("invalid command accepted: %+v %v", bad, err)
		}
	}
	current, err := c.GetCatalogSkill(context.Background(), entry.Target)
	if err != nil || current.Skill.Head != entry.Head || current.CatalogVersion != page.CatalogVersion {
		t.Fatalf("invalid commands changed state: %+v %v", current, err)
	}
}

func TestSkillCatalogExportRetryManagement(t *testing.T) {
	_, cfg := skillFixture(t)
	cfg.Engine = engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		return harness.RunResult{}, nil
	})
	cfg.Skills.Install = []harness.SkillInstall{{SourceID: "project", Directory: "research", Enabled: true}}
	cfg.Skills.ExportRoot = t.TempDir()
	c, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	page, err := c.QuerySkillCatalog(context.Background(), harness.SkillQuery{Limit: 10})
	if err != nil || len(page.Skills) != 1 {
		t.Fatalf("catalog: %+v %v", page, err)
	}
	entry := page.Skills[0]
	managed, err := c.SetSkillEnabledChecked(context.Background(), harness.SkillEnablement{Target: entry.Target, Expected: entry.Head, Enabled: true, RequestKey: "manage", Actor: "test"})
	if err != nil || managed.Skill.Export == nil {
		t.Fatalf("manage: %+v %v", managed, err)
	}
	// Wait for the real worker to settle, so a stale token test does not depend
	// on a race between querying and its first scheduled attempt.
	deadline := time.Now().Add(5 * time.Second)
	for {
		detail, err := c.GetCatalogSkill(context.Background(), entry.Target)
		if err != nil {
			t.Fatal(err)
		}
		entry = detail.Skill
		if entry.Export.Status == "synced" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker did not settle: %+v", entry.Export)
		}
		time.Sleep(10 * time.Millisecond)
	}
	fields := map[string]any{"operation": "skill.export.retry", "target": entry.Target, "expected_head": entry.Head, "expected_export_token": entry.Export.StateToken, "request_key": "desktop-retry"}
	for i := range 2 {
		value, err := skillManagementCall(t, c, fields)
		if err != nil {
			t.Fatal(err)
		}
		result, ok := value.(harness.SkillManagementResult)
		if !ok || result.Replayed != (i == 1) || result.Skill.Export == nil || result.AppliedHead != entry.Head {
			t.Fatalf("retry response: %+v", value)
		}
	}
	fields["request_key"] = "stale-retry"
	_, err = skillManagementCall(t, c, fields)
	var known *localhost.ManagementError
	if !errors.As(err, &known) || known.Code != "conflict" {
		t.Fatalf("accepted stale export state: %v", err)
	}
}

func TestSkillCatalogRollbackBindsObservedTarget(t *testing.T) {
	c, _ := skillEvolutionClient(t)
	ctx := context.Background()
	p := submitSkillEdit(t, c, "target-rollback", "Reviewed publication")
	if _, err := c.ApproveSkillProposal(ctx, p.ID, harness.SkillReview{ExpectedStateVersion: p.StateVersion, Actor: "operator"}); err != nil {
		t.Fatal(err)
	}
	page, err := c.QuerySkillCatalog(ctx, harness.SkillQuery{Limit: 10})
	if err != nil || len(page.Skills) != 1 {
		t.Fatalf("catalog: %+v %v", page, err)
	}
	entry := page.Skills[0]
	request := map[string]any{"operation": "proposal.rollback", "source_id": entry.Target.SourceID, "name": entry.Target.Name, "version": 1, "target": entry.Target, "expected_head": entry.Head, "request_key": "desktop-target-rollback"}
	badTarget := entry.Target
	badTarget.SourceIdentity = strings.Repeat("f", 64)
	request["target"] = badTarget
	_, err = skillManagementCall(t, c, request)
	var known *localhost.ManagementError
	if !errors.As(err, &known) || known.Code != "permission_denied" {
		t.Fatalf("old target accepted: %v", err)
	}
	request["target"] = entry.Target
	delete(request, "expected_head")
	_, err = skillManagementCall(t, c, request)
	if !errors.As(err, &known) || known.Code != "invalid_request" {
		t.Fatalf("full-target rollback accepted missing head: %v", err)
	}
	request["expected_head"] = entry.Head
	for range 2 {
		if _, err := skillManagementCall(t, c, request); err != nil {
			t.Fatal(err)
		}
	}
	current, err := c.GetCatalogSkill(ctx, entry.Target)
	if err != nil || current.Skill.Head.StateVersion != entry.Head.StateVersion+1 || current.Skill.Head.Version != p.Base.Version {
		t.Fatalf("rollback result: %+v %v", current, err)
	}
}
