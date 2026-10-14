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

type skillValidatorFunc func(context.Context, harness.SkillValidationInput) (harness.SkillValidationReport, error)

func (f skillValidatorFunc) Validate(ctx context.Context, input harness.SkillValidationInput) (harness.SkillValidationReport, error) {
	return f(ctx, input)
}

func skillEvolutionClient(t *testing.T, builtin ...bool) (*Client, Config) {
	t.Helper()
	_, cfg := skillFixture(t)
	cfg.Engine = engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		return harness.RunResult{}, nil
	})
	cfg.Skills.Install = []harness.SkillInstall{{SourceID: "project", Directory: "research", Enabled: true}}
	if len(builtin) > 0 && builtin[0] {
		cfg.Skills.BuiltinSources = []string{"project"}
	}
	cfg.SkillValidator = skillValidatorFunc(func(_ context.Context, in harness.SkillValidationInput) (harness.SkillValidationReport, error) {
		return harness.SkillValidationReport{ValidatorVersion: "fixture-review-v1", BaseVersion: in.Proposal.Base.Version, CandidateVersion: in.Proposal.CandidateVersion, Security: "passed", Behavior: "not_run"}, nil
	})
	c, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, cfg
}

func submitSkillEdit(t *testing.T, c *Client, key, body string) harness.SkillProposal {
	t.Helper()
	head, err := c.GetSkillHead(context.Background(), "project", "research")
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.SubmitSkillProposal(context.Background(), harness.SkillProposalRequest{SourceID: "project", Name: "research", RequestKey: key, Action: "edit", Base: head, Reason: "Explicit user improvement", Files: map[string][]byte{"SKILL.md": []byte("---\nname: research\ndescription: Research a source\n---\n" + body + "\n")}})
	if err != nil || p.Status != "pending_review" {
		t.Fatalf("submit: %+v %v", p, err)
	}
	return p
}

func skillManagementCall(t *testing.T, c *Client, fields map[string]any) (any, error) {
	t.Helper()
	request := localhost.ManagementRequest{Operation: fields["operation"].(string), Fields: map[string]json.RawMessage{}}
	for k, v := range fields {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		request.Fields[k] = data
	}
	return c.ManageLocal(context.Background(), request)
}

func TestSkillEvolutionSDKAndAuthenticatedManagement(t *testing.T) {
	c, cfg := skillEvolutionClient(t, true)
	ctx := context.Background()
	p := submitSkillEdit(t, c, "edit-one", "Reviewed next steps")
	host, err := localhost.New(localhost.Config{RuntimeDir: t.TempDir(), Manage: c.ManageLocal, ServeACP: func(context.Context, io.ReadCloser, io.WriteCloser) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	ep, err := host.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	call := func(fields map[string]any) map[string]json.RawMessage {
		t.Helper()
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(ep.Host, fmt.Sprint(ep.Port)), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		_, err = fmt.Fprintf(conn, "DFACP/1 %s MANAGE\n", ep.Token)
		if err != nil {
			t.Fatal(err)
		}
		reader := bufio.NewReader(conn)
		if line, err := reader.ReadString('\n'); err != nil || strings.TrimSpace(line) != "OK" {
			t.Fatalf("handshake: %q %v", line, err)
		}
		if err = json.NewEncoder(conn).Encode(fields); err != nil {
			t.Fatal(err)
		}
		var response map[string]json.RawMessage
		if err = json.NewDecoder(reader).Decode(&response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	response := call(map[string]any{"operation": "proposal.get", "proposal_id": p.ID})
	var detail map[string]any
	if string(response["ok"]) != "true" || json.Unmarshal(response["data"], &detail) != nil || detail["base_sha256"] != p.Base.Version || !strings.Contains(detail["diff"].(string), "+Reviewed next steps") {
		t.Fatalf("detail: %s", response)
	}
	var projected harness.SkillProposal
	if json.Unmarshal(response["data"], &projected) != nil || projected.OverlayBase == nil || p.OverlayBase == nil || *projected.OverlayBase != *p.OverlayBase {
		t.Fatalf("MANAGE lost overlay identity: %s", response)
	}
	response = call(map[string]any{"operation": "proposal.approve", "proposal_id": p.ID, "expected_state_version": p.StateVersion + 1})
	if string(response["ok"]) != "false" || string(response["code"]) != `"conflict"` {
		t.Fatalf("CAS: %s", response)
	}
	for range 2 {
		response = call(map[string]any{"operation": "proposal.approve", "proposal_id": p.ID, "expected_base_sha256": p.Base.Version})
		if string(response["ok"]) != "true" {
			t.Fatalf("legacy review: %s", response)
		}
	}
	head, err := c.GetSkillHead(ctx, "project", "research")
	if err != nil || head.Version != p.CandidateVersion {
		t.Fatalf("published: %+v %v", head, err)
	}
	response = call(map[string]any{"operation": "proposal.history"})
	var history struct {
		Proposals []harness.SkillProposal    `json:"proposals"`
		Revisions []harness.SkillPublication `json:"revisions"`
	}
	if string(response["ok"]) != "true" || json.Unmarshal(response["data"], &history) != nil || len(history.Revisions) != 2 || len(history.Proposals) != 1 || history.Proposals[0].Diff != "" {
		t.Fatalf("history: %s", response)
	}
	if history.Revisions[0].OverlayBase != nil || history.Revisions[1].OverlayBase == nil || *history.Revisions[1].OverlayBase != *p.OverlayBase || history.Proposals[0].OverlayBase == nil || *history.Proposals[0].OverlayBase != *p.OverlayBase {
		t.Fatalf("MANAGE history lost overlay identity: %s", response)
	}
	rollback := map[string]any{"operation": "proposal.rollback", "source_id": "project", "name": "research", "version": 1, "expected_head": head, "request_key": "restore-original"}
	for range 2 {
		if result := call(rollback); string(result["ok"]) != "true" {
			t.Fatalf("rollback: %s", result)
		}
	}
	rolled, err := c.GetSkillHead(ctx, "project", "research")
	if err != nil || rolled.Version != p.Base.Version || rolled.StateVersion != head.StateVersion+1 {
		t.Fatalf("rolled: %+v %v", rolled, err)
	}
	// A user action based on the pre-rollback head must not silently overwrite it.
	rollback["request_key"] = "new-stale-request"
	if result := call(rollback); string(result["code"]) != `"conflict"` {
		t.Fatalf("stale rollback: %s", result)
	}
	if err = host.Close(); err != nil {
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got, err := reopened.GetSkillHead(ctx, "project", "research"); err != nil || got != rolled {
		t.Fatalf("restart: %+v %v", got, err)
	}
	if got, err := reopened.GetSkillProposal(ctx, p.ID); err != nil || got.Status != "published" || got.PublishedRevision != 2 {
		t.Fatalf("saved review: %+v %v", got, err)
	}
}

func TestSkillEvolutionManagementInvalidFieldsAndRejection(t *testing.T) {
	c, _ := skillEvolutionClient(t)
	p := submitSkillEdit(t, c, "reject", "Candidate to reject")
	for _, fields := range []map[string]any{
		{"operation": "proposal.approve", "proposal_id": p.ID, "actor": "model"},
		{"operation": "proposal.approve", "proposal_id": p.ID, "expected_state_version": nil},
		{"operation": "proposal.approve", "proposal_id": p.ID, "expected_base_sha256": "bad"},
		{"operation": "proposal.list", "limit": 1.5},
		{"operation": "proposal.list", "status": "anything"},
		{"operation": "proposal.list", "status": "pending_review", "needs_attention": "true"},
		{"operation": "proposal.list", "status": "pending_review", "needs_attention": nil},
		{"operation": "proposal.list", "needs_attention": true},
		{"operation": "proposal.rollback", "name": "research", "version": true},
		{"operation": "proposal.rollback", "source_id": "project", "name": "research", "version": 1, "expected_head": nil},
	} {
		_, err := skillManagementCall(t, c, fields)
		var known *localhost.ManagementError
		if !errors.As(err, &known) || known.Code != "invalid_request" {
			t.Fatalf("request %+v: %v", fields, err)
		}
	}
	for _, attention := range []bool{true, false} {
		result, err := skillManagementCall(t, c, map[string]any{"operation": "proposal.list", "status": "pending_review", "needs_attention": attention, "limit": 1})
		if err != nil {
			t.Fatal("attention projection route", result, err)
		}
	}
	for range 2 {
		_, err := skillManagementCall(t, c, map[string]any{"operation": "proposal.reject", "proposal_id": p.ID, "note": "Needs clearer evidence"})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.ApproveSkillProposal(context.Background(), p.ID, harness.SkillReview{ExpectedStateVersion: p.StateVersion, Actor: "desktop"}); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatalf("approved rejected proposal: %v", err)
	}
	if head, err := c.GetSkillHead(context.Background(), "project", "research"); err != nil || head != p.Base {
		t.Fatalf("rejection changed head: %+v %v", head, err)
	}
}

func TestSkillEvolutionSDKWithoutValidatorFailsClosed(t *testing.T) {
	c, cfg := skillEvolutionClient(t)
	_ = c.Close()
	cfg.SkillValidator = nil
	c, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	head, err := c.GetSkillHead(context.Background(), "project", "new-skill")
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.SubmitSkillProposal(context.Background(), harness.SkillProposalRequest{SourceID: "project", Name: "new-skill", RequestKey: "new", Action: "create", Base: head, Files: map[string][]byte{"SKILL.md": []byte("---\nname: new-skill\ndescription: New task\n---\nFollow the approved task.\n")}})
	if err != nil || p.Status != "failed" {
		t.Fatalf("validator unavailable: %+v %v", p, err)
	}
	if _, err = c.ApproveSkillProposal(context.Background(), p.ID, harness.SkillReview{ExpectedStateVersion: p.StateVersion, Actor: "desktop"}); !errors.Is(err, harness.ErrSkillConflict) {
		t.Fatalf("invalid candidate approved: %v", err)
	}
}
