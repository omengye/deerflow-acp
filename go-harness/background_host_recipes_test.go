package deerflow

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// One bounded SDK integration covers the durable registry boundary using real
// MCP and provider fixtures. Reconnection is never treated as tool replay.
func TestSDKHostMCPRecipesRestartReconstruction(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		name := "completed_effect"
		if uncertain {
			name = "uncertain_effect"
		}
		t.Run(name, func(t *testing.T) {
			f := newResourceFixture(t)
			f.blockEffects.Store(uncertain)
			workspace := t.TempDir()
			f.config.MCP = harness.MCPPolicy{} // Host HTTP must not authorize client HTTP.
			f.config.HostMCPRecipes = []harness.HostMCPRecipe{{ID: "fixture-host", Workspace: workspace, RestoreOnRestart: true, Servers: []harness.MCPServer{f.server}}}
			ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
			defer cancel()
			var c *Client
			open := func(cfg Config) {
				t.Helper()
				var err error
				c, err = Open(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
			}
			closeClient := func() {
				t.Helper()
				if c != nil {
					if err := c.Close(); err != nil {
						t.Fatal(err)
					}
					c = nil
				}
			}
			defer closeClient()
			open(f.config)
			if _, err := c.NewSession(ctx, t.TempDir(), f.server); !errors.Is(err, harness.ErrPermissionDenied) {
				t.Fatalf("host recipe widened client transport authority: %v", err)
			}
			if _, err := c.NewSession(ctx, workspace, f.server); !errors.Is(err, harness.ErrInvalidInput) {
				t.Fatalf("mixed ownership accepted: %v", err)
			}
			session, err := c.NewSession(ctx, workspace)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "Delegate this work"}}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
				return harness.AllowOnce, nil
			}); err != nil {
				t.Fatal(err)
			}
			tasks, err := c.BackgroundTasks(ctx, session.ID, "", 10)
			if err != nil || len(tasks) != 1 {
				t.Fatalf("tasks=%+v err=%v", tasks, err)
			}
			task := resourceTaskSettled(t, ctx, c, session.ID, tasks[0])
			if task.Interaction == nil || task.Resources == nil || task.Resources.Status != "ready" {
				t.Fatalf("host task=%+v", task)
			}
			if _, err = c.ApproveBackgroundTask(ctx, session.ID, task.ID, harness.TaskApproval{ID: task.Interaction.ID, TaskVersion: task.Version, Decision: harness.AllowOnce}); err != nil {
				t.Fatal(err)
			}
			started := f.blocked
			if uncertain {
				started = f.effectStarted
			}
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if uncertain {
				task, err = c.CancelBackgroundTask(ctx, session.ID, task.ID)
				if err != nil {
					t.Fatal(err)
				}
				task = resourceTaskSettled(t, ctx, c, session.ID, task)
			} else {
				before, err := c.mcp.Generation(ctx, session.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err = c.CloseSession(ctx, session.ID); err != nil {
					t.Fatal(err)
				}
				if _, err = c.LoadSession(ctx, session.ID, workspace, false, nil); err != nil {
					t.Fatal(err)
				}
				after, err := c.mcp.Generation(ctx, session.ID)
				if err != nil || after != before {
					t.Fatalf("client detach retired host transport: %v", err)
				}
			}
			record, err := readBackgroundResource(ctx, c.store.DB(), task.ID)
			if err != nil || record.ref.HostRecipeID != "fixture-host" {
				t.Fatalf("missing host identity: %+v %v", record, err)
			}
			var saved []byte
			if err = c.store.DB().QueryRowContext(ctx, `SELECT ref FROM harness_background_resources WHERE task_id=?`, task.ID).Scan(&saved); err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{f.server.URL, "resource-secret-never-persist"} {
				if bytes.Contains(saved, []byte(secret)) {
					t.Fatal("host recipe credential persisted")
				}
			}
			closeClient()
			if !uncertain {
				// No recipe, changed configuration, and changed live schema must
				// each retain the saved grant and refuse resume.
				for _, variant := range []string{"missing", "changed_config", "changed_schema", "manual"} {
					cfg := f.config
					cfg.HostMCPRecipes = append([]harness.HostMCPRecipe(nil), f.config.HostMCPRecipes...)
					want := "incompatible"
					switch variant {
					case "missing":
						cfg.HostMCPRecipes = nil
						want = "waiting_connection"
					case "changed_config":
						cfg.HostMCPRecipes[0].Servers = []harness.MCPServer{{Name: f.server.Name, Transport: "http", URL: f.server.URL, Headers: map[string]string{"X-Test-Secret": "different-secret"}}}
					case "changed_schema":
						f.setTool("Changed schema after restart")
					case "manual":
						cfg.HostMCPRecipes[0].RestoreOnRestart = false
						want = "waiting_authorization"
					}
					open(cfg)
					current, e := readBackgroundResource(ctx, c.store.DB(), task.ID)
					if e != nil || current.generation != record.generation || current.version != record.version {
						t.Fatalf("%s renewed unapproved resources: %+v %v", variant, current, e)
					}
					if _, e = c.LoadSession(ctx, session.ID, workspace, false, nil); e != nil {
						t.Fatal(e)
					}
					task, e = c.BackgroundTask(ctx, session.ID, task.ID)
					if e != nil || task.Resources == nil || task.Resources.Status != want {
						t.Fatalf("%s task=%+v err=%v", variant, task, e)
					}
					if _, e = c.ResumeBackgroundTask(ctx, session.ID, task.ID, task.Version); e == nil {
						t.Fatalf("%s resumed saved task", variant)
					}
					if f.effects.Load() != 1 {
						t.Fatalf("%s replayed effect", variant)
					}
					closeClient()
					f.setTool("Record one external effect")
				}
			}
			open(f.config)
			current, err := readBackgroundResource(ctx, c.store.DB(), task.ID)
			if err != nil || current.generation == record.generation || current.version <= record.version || !sameMCPRef(&current.ref, &record.ref) {
				t.Fatalf("host restart did not renew grant independently: %+v %v", current, err)
			}
			if _, err = c.LoadSession(ctx, session.ID, workspace, false, nil); err != nil {
				t.Fatal(err)
			}
			task, err = c.BackgroundTask(ctx, session.ID, task.ID)
			if err != nil || task.Resources == nil || task.Resources.Status != "ready" {
				t.Fatalf("restored=%+v err=%v", task, err)
			}
			if uncertain {
				receipts, e := c.service.Store.ListToolReceipts(ctx, task.ChildSessionID)
				if e != nil || len(receipts) != 1 || receipts[0].State != harness.ReceiptUncertain {
					t.Fatalf("uncertainty lost: %+v %v", receipts, e)
				}
				if _, e = c.ResumeBackgroundTask(ctx, session.ID, task.ID, task.Version); e == nil {
					t.Fatal("host grant bypassed uncertainty")
				}
			} else {
				task, err = c.ResumeBackgroundTask(ctx, session.ID, task.ID, task.Version)
				if err != nil {
					t.Fatal(err)
				}
				task = resourceTaskSettled(t, ctx, c, session.ID, task)
				if task.Status != "completed" || f.calls.Load() != 3 {
					t.Fatalf("resume=%+v model calls=%d", task, f.calls.Load())
				}
			}
			if f.effects.Load() != 1 {
				t.Fatalf("host restoration replayed effect %d times", f.effects.Load())
			}
		})
	}
}
