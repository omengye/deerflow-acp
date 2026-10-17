package deerflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/channelproto"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/channels"
)

func TestChannelFullAccessAndCheckpointResumeWithoutReplay(t *testing.T) {
	for _, initial := range []string{harness.ApprovalGlobal, harness.ApprovalAllowAlways} {
		t.Run(initial, func(t *testing.T) {
			workspace, cfg := skillFixture(t)
			cfg.PermissionMode = harness.PermissionModeAll
			var toolCalls atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { toolCalls.Add(1); w.Write([]byte("fixture ok")) }))
			defer target.Close()
			var modelCalls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				modelCalls.Add(1)
				var req struct {
					Stream   bool
					Messages []struct{ Role string }
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				for _, m := range req.Messages {
					if m.Role == "tool" {
						skillToolResponse(w, req.Stream, "", "", "done")
						return
					}
				}
				args, _ := json.Marshal(map[string]string{"url": target.URL})
				skillToolResponse(w, req.Stream, "web_fetch", string(args), "")
			}))
			defer provider.Close()
			cfg.BuiltinTools = []harness.BuiltinToolConfig{{Name: "web_fetch"}}
			cfg.Provider, cfg.Model, cfg.BaseURL, cfg.APIKey = "openai", "fixture", provider.URL+"/v1", "fixture"
			ctx := context.Background()
			c, err := Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			var id string
			out, err := c.channelRun(ctx, "", workspace, "fetch", initial, func(s string) error { id = s; return nil })
			if err != nil {
				t.Fatal(err)
			}
			before, err := c.service.Store.Execution(ctx, id, "")
			if err != nil {
				t.Fatal(err)
			}
			if initial == harness.ApprovalGlobal {
				if toolCalls.Load() != 0 || out.Status != "needs_attention" {
					t.Fatal(out, toolCalls.Load())
				}
				dispatch := setupChannelCommandFixture(t, c, workspace)
				status, err := c.channels.Status(ctx, "fixture")
				if err != nil {
					t.Fatal(err)
				}
				rev := status.(map[string]any)["revision"].(int64)
				if _, err = c.channels.SetPermissions(ctx, "fixture", rev, harness.ApprovalAllowAlways); err != nil {
					t.Fatal(err)
				}
				dispatch.Revision = rev + 1
				out, err = c.channelRun(channels.WithDispatch(ctx, dispatch), id, workspace, "/resume", harness.ApprovalAllowAlways, func(string) error { return nil })
				if err != nil || out.Status != "completed" {
					t.Fatal(out, err)
				}
				after, err := c.service.Store.Execution(ctx, id, "")
				if err != nil || after.RunID != before.RunID || after.Status != harness.ExecutionCompleted {
					t.Fatal(after, err)
				}

			} else if out.Status != "completed" || !strings.Contains(out.Text, "done") {
				t.Fatal(out)
			}
			var runs int
			if err = c.store.DB().QueryRow(`SELECT count(*) FROM harness_runs WHERE session_id=?`, id).Scan(&runs); err != nil || runs != 1 || toolCalls.Load() != 1 || modelCalls.Load() != 2 {
				t.Fatal(runs, toolCalls.Load(), modelCalls.Load(), err)
			}
		})
	}
}

func TestChannelExplicitSkillReviewIsBounded(t *testing.T) {
	p := harness.PermissionRequest{ID: "intent", ToolName: "skill_edit"}
	p.Arguments = json.RawMessage(`{"action":"apply","edit_id":"edit","host_review":{"target":{"name":"fixture"},"changes":[{"path":"SKILL.md","preview":"-old\n+new"}]}}`)
	text, ok := channelConfirmationReview(p)
	if !ok || !strings.Contains(text, "SKILL.md") || !strings.Contains(text, "+new") {
		t.Fatal(text, ok)
	}
	p.Arguments = json.RawMessage(`{"action":"apply"}`)
	if _, ok = channelConfirmationReview(p); ok {
		t.Fatal("missing review allowed")
	}
	p.ToolName = "bash"
	if _, ok = channelConfirmationReview(p); ok {
		t.Fatal("unreviewable shell confirmation")
	}
}

type commandFixturePlugin struct{}

func (commandFixturePlugin) Handle(_ context.Context, method string, _ json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		return channelproto.Descriptor{ID: "fixture", Name: "Fixture", Protocol: 1}, nil
	case "login.start":
		return channelproto.Login{State: "confirmed", Auth: &channelproto.Auth{Account: "bot", Owner: "alice", Secret: json.RawMessage(`{"token":"fixture"}`)}}, nil
	case "poll":
		return channelproto.Batch{}, nil
	default:
		return map[string]bool{"sent": true}, nil
	}
}
func TestChannelCommandFixtureChild(t *testing.T) {
	if len(os.Args) < 2 || os.Args[len(os.Args)-1] != "channel-command-fixture" {
		return
	}
	_ = channelproto.Serve(context.Background(), os.Stdin, os.Stdout, commandFixturePlugin{})
	os.Exit(0)
}

func TestChannelSkillEditExplicitConfirmationAllowDenyAndRestart(t *testing.T) {
	for _, action := range []string{"approve", "deny"} {
		t.Run(action, func(t *testing.T) {
			workspace, cfg := skillFixture(t)
			cfg.Skills.Install = []harness.SkillInstall{{SourceID: "project", Directory: "research", Enabled: true}}
			cfg.PermissionMode = harness.PermissionModeAll
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Stream   bool
					Messages []struct{ Role, Content string }
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				switch calls.Add(1) {
				case 1:
					skillToolResponse(w, req.Stream, "skill_edit", `{"action":"prepare","name":"research","edits":[{"path":"SKILL.md","operation":"replace","find":"BODY_ONLY_AFTER_LOAD","replace":"CHANNEL_APPROVED_UPDATE"}]}`, "", "prepare")
				case 2:
					var id string
					for _, m := range req.Messages {
						if m.Role == "tool" {
							var v struct{ Review harness.SkillEditReview }
							if json.Unmarshal([]byte(m.Content), &v) == nil {
								id = v.Review.ID
							}
						}
					}
					args, _ := json.Marshal(map[string]string{"action": "apply", "edit_id": id})
					skillToolResponse(w, req.Stream, "skill_edit", string(args), "", "apply")
				default:
					skillToolResponse(w, req.Stream, "", "", "checked")
				}
			}))
			defer provider.Close()
			cfg.Provider, cfg.Model, cfg.BaseURL, cfg.APIKey = "openai", "fixture", provider.URL+"/v1", "fixture"
			ctx := context.Background()
			c, err := Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if c != nil {
					c.Close()
				}
			}()
			d := setupChannelCommandFixture(t, c, workspace)
			if _, err = c.channels.SetPermissions(ctx, d.Instance, d.Revision, harness.ApprovalAllowAlways); err != nil {
				t.Fatal(err)
			}
			d.Revision++
			var id string
			out, err := c.channelRun(channels.WithDispatch(ctx, d), "", workspace, "edit research", harness.ApprovalAllowAlways, func(s string) error { id = s; return nil })
			if err != nil || out.Status != "needs_attention" || !strings.Contains(out.Attention, "CHANNEL_APPROVED_UPDATE") {
				t.Fatal(out, err)
			}
			before, err := c.service.Store.Execution(ctx, id, "")
			if err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(cfg.Skills.Sources[0].Root, "research", "SKILL.md")
			body, err := os.ReadFile(p)
			if err != nil || strings.Contains(string(body), "CHANNEL_APPROVED_UPDATE") {
				t.Fatal("unapproved edit", err)
			}
			code := regexp.MustCompile(`/approve ([A-Z2-7]{12})`).FindStringSubmatch(out.Attention)
			if len(code) != 2 {
				t.Fatal("no code", out.Attention)
			}
			if err = c.Close(); err != nil {
				t.Fatal(err)
			}
			c = nil
			c, err = Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			out, err = c.channelRun(channels.WithDispatch(ctx, d), id, workspace, "/"+action+" "+code[1], harness.ApprovalAllowAlways, func(string) error { return nil })
			if err != nil || out.Status != "completed" {
				t.Fatal(out, err)
			}
			after, err := c.service.Store.Execution(ctx, id, "")
			if err != nil || after.RunID != before.RunID || after.Status != harness.ExecutionCompleted {
				t.Fatal(after, err)
			}
			body, err = os.ReadFile(p)
			if err != nil || strings.Contains(string(body), "CHANNEL_APPROVED_UPDATE") != (action == "approve") {
				t.Fatal("wrong final edit", action, err)
			}
			if calls.Load() != 3 {
				t.Fatal("replayed model work", calls.Load())
			}
			if _, err = c.channelRun(channels.WithDispatch(ctx, d), id, workspace, "/"+action+" "+code[1], harness.ApprovalAllowAlways, func(string) error { return nil }); err != nil || calls.Load() != 3 {
				t.Fatal("duplicate command ran model", err)
			}
		})
	}
}
func setupChannelCommandFixture(t *testing.T, c *Client, workspace string) channels.Dispatch {
	t.Helper()
	ctx := context.Background()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	manifest := channels.Manifest{ID: "fixture", Command: exe, Args: []string{"-test.run=^TestChannelCommandFixtureChild$", "channel-command-fixture"}, SHA256: hex.EncodeToString(sum[:])}
	if _, err = c.channels.Install(ctx, "fixture", manifest); err != nil {
		t.Fatal(err)
	}
	if _, err = c.channels.Configure(ctx, "fixture", 1, workspace, nil, true); err != nil {
		t.Fatal(err)
	}
	if _, err = c.channels.Login(ctx, "fixture", "login.start", "", ""); err != nil {
		t.Fatal(err)
	}
	return channels.Dispatch{Instance: "fixture", Account: "bot", Conversation: "alice", Sender: "alice", Workspace: workspace, Revision: 3}
}
