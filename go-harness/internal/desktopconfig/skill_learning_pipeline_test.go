package desktopconfig

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/launch"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
	"github.com/omengye/deerflow-acp/go-harness/internal/runtimepaths"
	"github.com/omengye/deerflow-acp/go-harness/internal/skilllearningtest"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Real native adapters, task capture, scheduler, authenticated management,
// security review and publication are exercised against a deterministic provider.
func TestSkillLearningSettingsNativeAutomaticPipeline(t *testing.T) {
	for _, create := range []bool{false, true} {
		t.Run(fmt.Sprintf("create=%t", create), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			options, doc := reviewDocument(t)
			provider := skilllearningtest.PrimaryRoutes(t, create)
			server := httptest.NewServer(provider)
			defer server.Close()
			workspace := t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, "config.json"), []byte(`{"verified":true,"token":"NATIVE_PRIVATE_TOKEN"}`), 0600); err != nil {
				t.Fatal(err)
			}
			imported := filepath.Join(str(readObject(doc, "paths"), "skills", ""), "custom", "research")
			if err := os.MkdirAll(imported, 0700); err != nil {
				t.Fatal(err)
			}
			original := []byte("---\nname: research\ndescription: Research bibliographic sources\n---\nBODY_ONLY_AFTER_LOAD. Read guide.txt.\n")
			for path, data := range map[string][]byte{"SKILL.md": original, "guide.txt": []byte("PINNED_REFERENCE")} {
				if err := os.WriteFile(filepath.Join(imported, path), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			model := cloneMap(modelByName(t, doc, "first"))
			model["name"], model["original_name"], model["provider"] = "primary", "", "openai"
			model["model"], model["base_url"], model["api_key"] = skilllearningtest.SharedModelID, server.URL+"/v1", "fixture-primary"
			doc["models"], doc["default_model"] = []any{model}, "primary"
			runtime := readObject(doc, "runtime")
			runtime["model_name"], runtime["tool_allowlist"] = "primary", []any{"skill", "read_file"}
			memory := readObject(doc, "memory")
			memory["enabled"], memory["injection_enabled"], memory["retrieval_enabled"] = false, false, false
			target := "research"
			if create {
				target = skilllearningtest.CapabilityName
			}
			saved := run(t, options, "save", doc)
			if _, ok := saved["skill_evolution"]; ok || len(provider.Counts()) != 0 {
				t.Fatal("settings introduced legacy fields or model calls")
			}
			var cfg deerflow.Config
			connections := 0
			source, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.SkillLearning == nil || cfg.SkillGeneration == nil || !cfg.SkillGeneration.PublishAfterSecurityReview || cfg.SkillBehaviorEvaluator != nil {
				t.Fatal("native automatic chain incomplete")
			}
			// Reduce only the internal idle delay for this test; no client field exists.
			cfg.SkillLearning.Idle = time.Second
			var base harness.SkillHead
			if !create {
				base = seedLearningSettingsBase(t, ctx, cfg)
				// This fixture creates SQLite directly, so record the same binding
				// that the ordinary first Open writes after database creation.
				if err = runtimepaths.BindCreated(cfg.RuntimeProfile, cfg.DataDir); err != nil {
					t.Fatal(err)
				}
			}
			c, err := deerflow.Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if c != nil {
					_ = c.Close()
				}
			}()
			host, err := localhost.New(localhost.Config{RuntimeDir: t.TempDir(), ConfigPath: source.Path, Manage: c.ManageLocal, ServeACP: c.ServeACP})
			if err != nil {
				t.Fatal(err)
			}
			defer host.Close()
			endpoint, err := host.Start(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var status harness.SkillLearningStatus
			learningSettingsManagement(t, endpoint, map[string]any{"operation": "skill.learning.status"}, &status)
			if !status.Enabled || !status.AutoPatchEnabled || !status.ConsolidationEnabled || !status.MatchingEnabled {
				t.Fatal("running automatic stages missing")
			}
			session, err := c.NewSession(ctx, workspace)
			if err != nil {
				t.Fatal(err)
			}
			var consent harness.SkillLearningControl
			learningSettingsManagement(t, endpoint, map[string]any{"operation": "skill.learning.control.set", "session_id": session.ID, "control": map[string]any{"contribute": true, "workspace_shared": true, "expected_epoch": 0}}, &consent)
			wait := func(label string, ready func() bool) {
				t.Helper()
				for ctx.Err() == nil {
					if ready() {
						return
					}
					time.Sleep(25 * time.Millisecond)
				}
				t.Fatalf("%s timed out: %v", label, provider.Counts())
			}
			allow := func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
				return harness.AllowOnce, nil
			}
			for task := 0; task < 2; task++ {
				prompt := fmt.Sprintf("Inspect JSON document %d completely and verify all required fields", task+1)
				result, e := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: prompt}}, nil, allow)
				if e != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted {
					t.Fatal("native task failed", e)
				}
				wait("task matching", func() bool {
					page, e := c.WorkspaceSkillExperienceMatches(ctx, session.ID, "", 10)
					if e != nil {
						t.Fatal(e)
					}
					return len(page.Matches) == task+1
				})
				if task == 0 && provider.Counts()["generate"] != 0 {
					t.Fatal("one independent root admitted generation")
				}
			}
			var proposal harness.SkillProposal
			wait("automatic adoption", func() bool {
				page, e := c.ListSkillDiscoveries(ctx, harness.SkillQuery{Limit: 10})
				if e != nil {
					t.Fatal(e)
				}
				if len(page.Discoveries) == 0 {
					return false
				}
				if len(page.Discoveries) != 1 {
					t.Fatal("duplicate discovery")
				}
				d := page.Discoveries[0]
				if d.State == "failed" || d.State == "cancelled" {
					t.Fatal("discovery failed", d)
				}
				if d.ProposalID == "" {
					return false
				}
				proposal, e = c.GetSkillProposal(ctx, d.ProposalID)
				if e != nil {
					t.Fatal(e)
				}
				if proposal.Status == "failed" || proposal.Status == "stale" {
					t.Fatal("candidate failed", proposal.Status, proposal.Error, proposal.Evaluation.BehaviorReport)
				}
				return proposal.Status == "published"
			})
			if proposal.Base != base || proposal.ReviewedBy != "security_review" || proposal.Evaluation.Security != "passed" || proposal.Evaluation.Behavior != "not_run" || proposal.Evaluation.BehaviorReport != nil || proposal.Approval == nil || proposal.Approval.SecurityReview == nil || !proposal.Approval.SecurityReview.WorkspaceOnly {
				t.Fatal("missing scoped safety publication", proposal)
			}
			if _, err = c.ApproveSkillProposal(ctx, proposal.ID, harness.SkillReview{ExpectedStateVersion: proposal.StateVersion, Actor: "desktop"}); err == nil {
				t.Fatal("manual approval accepted for automatic candidate")
			}
			var observation harness.SkillAutoPatchPage
			learningSettingsManagement(t, endpoint, map[string]any{"operation": "skill.auto_patch.list", "limit": 10}, &observation)
			if len(observation.Observations) != 0 {
				t.Fatal("offline tests fabricated online success")
			}
			published, err := c.GetSkillHead(ctx, "python-custom", target)
			if err != nil || published.Version != proposal.CandidateVersion {
				t.Fatal("candidate head missing", err)
			}
			if content, err := os.ReadFile(filepath.Join(imported, "SKILL.md")); err != nil || string(content) != string(original) {
				t.Fatal("scoped adoption overwrote shared physical source", err)
			}
			other, err := c.NewSession(ctx, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			for _, probe := range []struct {
				id      string
				visible bool
			}{{session.ID, true}, {other.ID, false}} {
				var receipt struct {
					Available        bool
					Version          string
					ExecutionChecked bool `json:"execution_checked"`
				}
				learningSettingsManagement(t, endpoint, map[string]any{"operation": "skill.verify", "target": proposal.Target, "session_id": probe.id, "expected_version": proposal.CandidateVersion}, &receipt)
				if receipt.Available != probe.visible || receipt.ExecutionChecked || probe.visible && receipt.Version != proposal.CandidateVersion {
					t.Fatal("incorrect next-turn visibility", receipt, probe)
				}
			}
			var statusAfter harness.SkillAutoPatchStatus
			learningSettingsManagement(t, endpoint, map[string]any{"operation": "skill.auto_patch.status"}, &statusAfter)
			if statusAfter.PublicationPolicy != harness.SkillPublicationSecurityOnly || !statusAfter.Enabled || !statusAfter.Running || statusAfter.PublishedToday != 1 || statusAfter.Observing != 0 {
				t.Fatal("incorrect safety publication status", statusAfter)
			}
			counts := provider.Counts()
			for _, stage := range []string{"extract", "consolidate", "match"} {
				if counts[stage] != 2 {
					t.Fatal("root not independently learned", stage, counts)
				}
			}
			for _, stage := range []string{"generate", "security"} {
				if counts[stage] != 1 {
					t.Fatal("stage duplicated", stage, counts)
				}
			}
			if counts["behavior"] != 0 || counts["trigger"] != 0 || counts["quality"] != 0 {
				t.Fatal("unexpected evaluation calls", counts)
			}
			_ = host.Close()
			if err = c.Close(); err != nil {
				t.Fatal(err)
			}
			c = nil
			db, err := sqlite.Open(filepath.Join(cfg.DataDir, "harness.db"))
			if err != nil {
				t.Fatal(err)
			}
			var calls, tokens, unsettled, feedback int
			err = db.DB().QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(actual_tokens),0),COALESCE(sum(state<>'settled'),0) FROM df_skill_maintenance_calls`).Scan(&calls, &tokens, &unsettled)
			if err != nil || calls != 8 || tokens != calls*125 || unsettled != 0 {
				t.Fatal("usage not recorded independently of quota", calls, tokens, unsettled, err)
			}
			if err = db.DB().QueryRowContext(ctx, `SELECT count(*) FROM df_skill_feedback`).Scan(&feedback); err != nil || feedback != 0 {
				t.Fatal("model fabricated feedback", err)
			}
			_ = db.Close()
			cfg = deerflow.Config{}
			if _, err = launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil {
				t.Fatal(err)
			}
			c, err = deerflow.Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if head, e := c.GetSkillHead(ctx, "python-custom", target); e != nil || head != published {
				t.Fatal("restart lost published head", e)
			}
			if provider.Counts()["generate"] != 1 {
				t.Fatal("restart replayed generation")
			}
		})
	}
}

func seedLearningSettingsBase(t *testing.T, ctx context.Context, cfg deerflow.Config) harness.SkillHead {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(cfg.DataDir, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	registry, err := skills.NewRegistry(ctx, cfg.Skills, store.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	for _, install := range cfg.Skills.Install {
		if err = registry.InstallConfigured(ctx, install.SourceID, install.Directory, install.Enabled); err != nil {
			t.Fatal(err)
		}
	}
	var adopted harness.SkillHead
	if err = registry.WithPublication(ctx, "python-custom", "research", func(tx *skills.PublicationTx) error {
		head, err := tx.Current()
		if err != nil {
			return err
		}
		adopted, err = tx.Publish(head.Version, head, true)
		return err
	}); err != nil || !adopted.Managed {
		t.Fatal("isolated adopted baseline missing", adopted, err)
	}
	return adopted
}

func learningSettingsManagement(t *testing.T, endpoint localhost.Endpoint, fields map[string]any, out any) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(endpoint.Host, fmt.Sprint(endpoint.Port)), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err = conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = fmt.Fprintf(conn, "DFACP/1 %s MANAGE\n", endpoint.Token); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "OK" {
		t.Fatal("native authenticated handshake failed", line, err)
	}
	if err = json.NewEncoder(conn).Encode(fields); err != nil {
		t.Fatal(err)
	}
	var response struct {
		OK   bool            `json:"ok"`
		Code string          `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err = json.NewDecoder(reader).Decode(&response); err != nil || !response.OK || response.Code != "" {
		t.Fatal("native management request failed", fields, response.Code, err)
	}
	if err = json.Unmarshal(response.Data, out); err != nil {
		t.Fatal(err)
	}
}
