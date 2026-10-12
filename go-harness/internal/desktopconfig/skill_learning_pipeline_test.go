package desktopconfig

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/launch"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
	"github.com/omengye/deerflow-acp/go-harness/internal/skilllearningtest"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestSkillLearningSettingsNativeAutomaticPipelineAndReview(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		name := "review-draft"
		if automatic {
			name = "automatic-existing"
		}
		t.Run(name, func(t *testing.T) {
			skillLearningSettingsNativePipeline(t, automatic, false)
		})
	}
}

func TestSkillLearningSettingsNativeNewCapabilityAutomaticallyDraftsAndRequiresReview(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		name := "review-draft"
		if automatic {
			name = "independent-auto-mode"
		}
		t.Run(name, func(t *testing.T) {
			skillLearningSettingsNativePipeline(t, automatic, true)
		})
	}
}

func skillLearningSettingsNativePipeline(t *testing.T, automatic, newCapability bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancel()
	options, doc := reviewDocument(t)
	provider := skilllearningtest.NamedRoutes(t)
	targetName := "research"
	if newCapability {
		provider, targetName = skilllearningtest.NamedNewCapabilityRoutes(t), skilllearningtest.CapabilityName
	}
	server := httptest.NewServer(provider)
	defer server.Close()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "config.json"), []byte(`{"verified":true,"token":"NATIVE_PRIVATE_TOKEN","note":"Inspect whole file; to\u200bken=NATIVE_HIDDEN_TOKEN"}`), 0600); err != nil {
		t.Fatal(err)
	}
	imported := filepath.Join(str(readObject(doc, "paths"), "skills", ""), "custom", "research")
	if err := os.MkdirAll(imported, 0700); err != nil {
		t.Fatal(err)
	}
	original := []byte("---\nname: research\ndescription: Research a source\n---\nBODY_ONLY_AFTER_LOAD. Read guide.txt.\n")
	if newCapability {
		original = []byte("---\nname: research\ndescription: Research bibliographic sources, not local JSON verification\n---\nBODY_ONLY_AFTER_LOAD. Read guide.txt.\n")
	}
	for path, data := range map[string][]byte{"SKILL.md": original, "guide.txt": []byte("PINNED_REFERENCE")} {
		if err := os.WriteFile(filepath.Join(imported, path), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	template := cloneMap(modelByName(t, doc, "first"))
	models := []any{}
	for _, stage := range []string{"foreground", "extract", "consolidate", "match", "generate", "security", "quality"} {
		model := cloneMap(template)
		model["name"], model["original_name"], model["provider"] = stage, "", "openai"
		model["model"], model["base_url"], model["api_key"] = skilllearningtest.SharedModelID, server.URL+"/"+stage+"/v1", "fixture-"+stage
		models = append(models, model)
	}
	doc["models"], doc["default_model"] = models, "foreground"
	runtime := readObject(doc, "runtime")
	runtime["model_name"], runtime["tool_allowlist"] = "foreground", []any{"skill", "read_file"}
	memory := readObject(doc, "memory")
	memory["enabled"], memory["injection_enabled"], memory["retrieval_enabled"] = false, false, false
	e := readObject(doc, "skill_evolution")
	e["moderation_model_name"], e["evaluation_model_name"], e["generation_model_name"] = "security", "quality", "generate"
	l, d := readObject(e, "learning"), readObject(e, "discovery")
	l["enabled"], l["consolidation_enabled"], l["matching_enabled"], l["cross_session_enabled"] = true, true, true, automatic
	l["extraction_model_name"], l["consolidation_model_name"], l["matching_model_name"] = "extract", "consolidate", "match"
	l["idle_seconds"], l["max_daily_model_calls"], l["max_daily_tokens"] = 1, 6, 100000
	d["experience_patterns_enabled"], d["experience_review_drafts_enabled"] = true, !automatic
	if newCapability {
		d["experience_new_skills_enabled"], d["new_skill_source_id"] = true, "python-custom"
	}
	readObject(e, "generation")["enabled"] = true
	e["budget"] = map[string]any{"max_daily_model_calls": 20, "max_daily_tokens": 200000, "max_job_seconds": 60, "workers": 1}
	suite := harness.SkillBehaviorSuite{Version: "skill-behavior-v1", ID: "native-settings-json-check", SourceID: "python-custom", Name: "research", Cases: []harness.SkillBehaviorCase{
		{ID: "targeted", Kind: "targeted", Prompt: "Inspect the complete JSON document and report completion.", Repetitions: 1, Files: map[string]string{}, Fixtures: []harness.SkillBehaviorFixture{}, Assertions: []harness.SkillBehaviorAssertion{{ID: "oracle-native-complete", Kind: "output_equals", Expected: "verified"}}},
		{ID: "regression", Kind: "regression", Prompt: "Retain the existing source research behavior.", Repetitions: 1, Files: map[string]string{}, Fixtures: []harness.SkillBehaviorFixture{}, Assertions: []harness.SkillBehaviorAssertion{{ID: "oracle-native-retained", Kind: "output_equals", Expected: "retained"}}},
	}}
	encoded, err := json.Marshal(suite)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(filepath.Dir(options.Config), "native-suite.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	b := readObject(e, "behavior")
	b["enabled"], b["suite_path"], b["max_steps"] = true, "native-suite.json", 1
	if automatic {
		e["mode"] = "auto_patch"
		readObject(e, "auto_patch")["experience_patterns_enabled"] = true
	}
	saved := run(t, options, "save", doc)
	if len(provider.Counts()) != 0 {
		t.Fatal("saving settings performed paid model work", provider.Counts())
	}
	var cfg deerflow.Config
	connections := 0
	source, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil)
	if err != nil {
		t.Fatal(err)
	}
	if source.Path != options.Config || source.Revision != saved["config_revision"] || cfg.SkillExperienceExtractor != nil || cfg.SkillExperienceConsolidator != nil || cfg.SkillExperienceMatcher != nil || cfg.Engine != nil || cfg.SkillLearning == nil || cfg.SkillGeneration == nil || cfg.SkillDiscovery == nil || cfg.SkillBehavior == nil || cfg.SkillBehaviorEvaluator == nil {
		t.Fatal("saved native configuration did not construct the full automatic chain", source, err)
	}
	for path, want := range map[string]bool{
		"/skill_evolution/learning/enabled": true, "/skill_evolution/learning/consolidation_enabled": true,
		"/skill_evolution/learning/matching_enabled": true, "/skill_evolution/learning/cross_session_enabled": automatic,
		"/skill_evolution/discovery/experience_patterns_enabled": true, "/skill_evolution/discovery/experience_review_drafts_enabled": !automatic,
		"/skill_evolution/discovery/experience_new_skills_enabled": newCapability,
	} {
		if field, found := source.Settings.Fields[path]; !found || field.Value != want || field.Source != "yaml" {
			t.Fatal("effective settings disagree with saved grants", path, field)
		}
	}
	if automatic {
		if field := source.Settings.Fields["/skill_evolution/auto_patch/experience_patterns_enabled"]; field.Value != true || field.Source != "yaml" {
			t.Fatal("effective settings omitted the independent publication grant", field)
		}
	} else if cfg.SkillAutoPatch != nil {
		t.Fatal("review configuration constructed automatic-publication authority")
	}
	// Existing-skill cases start from an adopted temporary baseline. Creation
	// cases have no managed head; no learning result or job is ever seeded.
	var base harness.SkillHead
	if !newCapability {
		base = seedLearningSettingsBase(t, ctx, cfg)
	} else if !cfg.SkillDiscovery.ExperienceNewSkillsEnabled || cfg.SkillDiscovery.NewSkillSourceID != "python-custom" || !slices.Contains(cfg.SkillGeneration.Sources, "python-custom") {
		t.Fatal("saved native configuration lost host-only creation authority", cfg.SkillDiscovery, cfg.SkillGeneration)
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
	researchHead, err := c.GetSkillHead(ctx, "python-custom", "research")
	if err != nil || !researchHead.Exists || !researchHead.Enabled || newCapability && researchHead.Managed {
		t.Fatal("native import setup lost the untouched source skill", researchHead, err)
	}
	host, err := localhost.New(localhost.Config{RuntimeDir: t.TempDir(), ConfigPath: source.Path, Manage: c.ManageLocal, ServeACP: c.ServeACP})
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	endpoint, err := host.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	manage := func(fields map[string]any, out any) { t.Helper(); learningSettingsManagement(t, endpoint, fields, out) }
	var status harness.SkillLearningStatus
	manage(map[string]any{"operation": "skill.learning.status"}, &status)
	if !status.Enabled || !status.ConsolidationEnabled || !status.MatchingEnabled || !status.ProposalDiscoveryEnabled || status.AutoPatchEnabled != automatic || status.CrossSessionEnabled != automatic {
		t.Fatal("running host lost saved learning grants", status)
	}
	wait := func(description string, ready func() bool) {
		t.Helper()
		until := time.Now().Add(20 * time.Second)
		for ctx.Err() == nil && time.Now().Before(until) {
			if ready() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("%s did not settle: calls=%v context=%v", description, provider.Counts(), ctx.Err())
	}
	var first harness.Session
	tasks := 1
	if automatic {
		tasks = 2
	}
	allow := func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		return harness.AllowOnce, nil
	}
	for task := 0; task < tasks; task++ {
		session, err := c.NewSession(ctx, workspace)
		if err != nil {
			t.Fatal(err)
		}
		if task == 0 {
			first = session
		}
		if automatic {
			var consent harness.SkillLearningControl
			manage(map[string]any{"operation": "skill.learning.control.set", "session_id": session.ID, "control": map[string]any{"contribute": true, "workspace_shared": true, "expected_epoch": 0}}, &consent)
			if !consent.Contribute || !consent.WorkspaceShared || consent.Epoch != 1 {
				t.Fatal("authenticated sharing consent was not applied", consent)
			}
		}
		prompt := fmt.Sprintf("Inspect JSON document %d using research", task+1)
		if newCapability {
			prompt = fmt.Sprintf("Inspect local JSON document %d and verify its required fields", task+1)
		}
		prompt += "; to\u200bken=NATIVE_HIDDEN_USER_TOKEN"
		for _, prompt := range []string{prompt, "Correct the previous inspection: read the complete document and verify required fields"} {
			result, err := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: prompt}}, nil, allow)
			if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted {
				t.Fatal("saved native foreground run failed", result, err)
			}
		}
		if automatic && task == 0 {
			wait("first native task matching", func() bool {
				page, err := c.WorkspaceSkillExperienceMatches(ctx, session.ID, "", 10)
				if err != nil {
					t.Fatal(err)
				}
				return len(page.Matches) == 1
			})
			if page, err := c.ListSkillDiscoveries(ctx, harness.SkillQuery{Limit: 10}); err != nil || len(page.Discoveries) != 0 || provider.Counts()["generate"] != 0 {
				t.Fatal("one task bypassed the automatic-publication evidence threshold", page, err)
			}
		}
	}
	var discovery harness.SkillDiscovery
	var proposal harness.SkillProposal
	wait("saved native automatic proposal", func() bool {
		page, err := c.ListSkillDiscoveries(ctx, harness.SkillQuery{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Discoveries) == 0 {
			return false
		}
		if len(page.Discoveries) != 1 || page.Discoveries[0].State == "failed" || page.Discoveries[0].State == "cancelled" {
			t.Fatal("saved native discovery failed or duplicated", page)
		}
		discovery = page.Discoveries[0]
		if discovery.State != "completed" || discovery.ProposalID == "" {
			return false
		}
		proposal, err = c.GetSkillProposal(ctx, discovery.ProposalID)
		if err != nil {
			t.Fatal(err)
		}
		return automatic && !newCapability && proposal.Status == "published" || (!automatic || newCapability) && proposal.Status == "pending_review"
	})
	modelReviewPassed := slices.EqualFunc(proposal.Evaluation.ModelChecks, []string{"security", "quality"}, func(check harness.SkillModelCheck, stage string) bool {
		return check.Stage == stage && check.Passed
	})
	if discovery.ReviewDraft != !automatic || discovery.HostAdjudicated || discovery.CreationRetry != nil || len(discovery.Evidence) != tasks || discovery.Target.SourceID != "python-custom" || discovery.Target.Name != targetName || proposal.SkillName != targetName || proposal.Origin != "automatic" || proposal.Author != "discovery" || proposal.Trigger.Type != "experience_pattern" || proposal.Base != base || !slices.Equal(proposal.ChangedFiles, []string{"SKILL.md"}) || proposal.Evaluation.Security != "passed" || !modelReviewPassed {
		t.Fatal("saved native pipeline lost generated attribution or actual baseline comparison", discovery, proposal)
	}
	if newCapability {
		if proposal.Action != "create" || proposal.Evaluation.Behavior != "not_run" || proposal.Evaluation.BehaviorReport != nil || !strings.Contains(proposal.Diff, "+"+skilllearningtest.Instruction) {
			t.Fatal("automatically generated capability fabricated a baseline or behavioral validation", proposal)
		}
		var matches harness.SkillExperienceMatchPage
		operation := "skill.learning.matches"
		if automatic {
			operation = "skill.learning.workspace.matches"
		}
		manage(map[string]any{"operation": operation, "session_id": first.ID, "limit": 10}, &matches)
		matchID := discovery.Evidence[0].Experience.MatchID
		if !slices.ContainsFunc(matches.Matches, func(m harness.SkillExperienceMatch) bool {
			return m.ID == matchID && m.Relation == "new_capability" && m.SuggestedName == targetName && m.Origin == "model_matched" && m.Quality == "unassessed" && m.Target == nil && m.Head == nil && m.OverlayBase == nil && len(m.ActualReads) == 0
		}) {
			t.Fatal("native matcher invented a target or reads for a new capability", matches)
		}
	} else if proposal.Evaluation.Behavior != "passed" || proposal.Evaluation.BehaviorReport == nil || proposal.Evaluation.BehaviorReport.Improvements != 1 || proposal.Evaluation.BehaviorReport.Regressions != 0 {
		t.Fatal("saved native existing-skill pipeline lost its actual baseline comparison", proposal)
	}
	counts := provider.Counts()
	for _, stage := range []string{"extract", "consolidate", "match"} {
		if counts[stage] != tasks {
			t.Fatal("saved named model missed or retried a learning task", stage, counts)
		}
	}
	foreground, behaviorCalls, maintenanceCalls := 6*tasks, 4, 3*tasks+7
	if newCapability {
		foreground, behaviorCalls, maintenanceCalls = 4*tasks, 0, 3*tasks+3
	}
	if counts["foreground"] != foreground || counts["generate"] != 1 || counts["security"] != 1 || counts["quality"] != 1 || counts["behavior"] != behaviorCalls {
		t.Fatal("saved named routes changed native pipeline calls", counts)
	}
	if newCapability && automatic {
		// Allow the real two-second auto-patch worker to scan the fully
		// supported creation; enabled publication must still leave it pending.
		until := time.Now().Add(2500 * time.Millisecond)
		for time.Now().Before(until) && ctx.Err() == nil {
			current, err := c.GetSkillProposal(ctx, proposal.ID)
			if err != nil || current.Status != "pending_review" {
				t.Fatal("automatic publication adopted a newly discovered capability", current, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	if !automatic || newCapability {
		if head, err := c.GetSkillHead(ctx, "python-custom", targetName); err != nil || head != base {
			t.Fatal("generated review draft changed the live skill", head, err)
		}
		var reviewed struct {
			Proposal harness.SkillProposal `json:"proposal"`
		}
		manage(map[string]any{"operation": "proposal.approve", "proposal_id": proposal.ID, "expected_state_version": proposal.StateVersion, "expected_base_sha256": base.Version}, &reviewed)
		proposal = reviewed.Proposal
		if proposal.Status != "published" {
			t.Fatal("authenticated review did not adopt the generated candidate", proposal)
		}
	} else {
		var page harness.SkillAutoPatchPage
		manage(map[string]any{"operation": "skill.auto_patch.list", "limit": 10}, &page)
		if len(page.Observations) != 1 || page.Observations[0].Status != "observing" || page.Observations[0].LearningSource == nil || page.Observations[0].LearningSource.DiscoveryID != discovery.ID || page.Observations[0].Successes != 0 || page.Observations[0].Failures != 0 {
			t.Fatal("saved automatic publication lost source or fabricated quality votes", page)
		}
	}
	if newCapability {
		var page harness.SkillAutoPatchPage
		manage(map[string]any{"operation": "skill.auto_patch.list", "limit": 10}, &page)
		if len(page.Observations) != 0 {
			t.Fatal("human capability approval fabricated automatic-publication observation", page)
		}
		if head, err := c.GetSkillHead(ctx, "python-custom", "research"); err != nil || head != researchHead {
			t.Fatal("new-capability evolution modified the unrelated source skill", head, err)
		}
	}
	published, err := c.GetSkillHead(ctx, "python-custom", targetName)
	if err != nil || published.Version != proposal.CandidateVersion || !published.Managed || published == base {
		t.Fatal("saved native publication missing", published, err)
	}
	if err = host.Close(); err != nil {
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c = nil
	accounting, err := sqlite.Open(filepath.Join(cfg.DataDir, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer accounting.Close()
	var receipts, tokens, unsettled, feedback int
	if err = accounting.DB().QueryRowContext(ctx, `SELECT count(*),COALESCE(sum(charged_tokens),0),COALESCE(sum(state<>'settled'),0) FROM df_skill_maintenance_calls`).Scan(&receipts, &tokens, &unsettled); err != nil || receipts != maintenanceCalls || tokens != receipts*125 || unsettled != 0 {
		t.Fatal("saved stages did not share settled maintenance accounting", receipts, tokens, unsettled, err)
	}
	if err = accounting.DB().QueryRowContext(ctx, `SELECT count(*) FROM df_skill_feedback`).Scan(&feedback); err != nil || feedback != 0 {
		t.Fatal("native models fabricated operator feedback", feedback, err)
	}
	if newCapability {
		var reserved, consumed, discoveries int
		if err = accounting.DB().QueryRowContext(ctx, `SELECT (SELECT count(*) FROM df_learning_new_skill_consumed), (SELECT count(*) FROM df_skill_discovery_episode_consumed), (SELECT count(*) FROM df_skill_discoveries)`).Scan(&reserved, &consumed, &discoveries); err != nil || reserved != 1 || consumed != tasks || discoveries != 1 {
			t.Fatal("native new-capability drafting duplicated or refunded its evidence reservation", reserved, consumed, discoveries, err)
		}
		var reads int
		if err = accounting.DB().QueryRowContext(ctx, `SELECT count(*) FROM harness_skill_reads`).Scan(&reads); err != nil || reads != 0 {
			t.Fatal("native capability discovery manufactured skill reads before adoption", reads, err)
		}
	}
	if err = accounting.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopen from a newly saved, disabled configuration, not edited SDK
	// options. The import startup profile must retain the published head.
	doc = run(t, options, "snapshot", nil)
	e = readObject(doc, "skill_evolution")
	e["enabled"], e["mode"] = false, "review"
	readObject(e, "generation")["enabled"] = false
	l = readObject(e, "learning")
	l["enabled"], l["consolidation_enabled"], l["matching_enabled"], l["cross_session_enabled"] = false, false, false, false
	d = readObject(e, "discovery")
	d["experience_patterns_enabled"], d["experience_review_drafts_enabled"] = false, false
	d["experience_new_skills_enabled"] = false
	readObject(e, "auto_patch")["experience_patterns_enabled"] = false
	readObject(e, "behavior")["enabled"] = false
	run(t, options, "save", doc)
	cfg = deerflow.Config{}
	if _, err = launch.ApplyConfig(options.Config, &cfg, &connections, nil); err != nil || cfg.SkillLearning != nil || cfg.SkillGeneration != nil || cfg.SkillDiscovery != nil || cfg.SkillAutoPatch != nil {
		t.Fatal("saved opt-out retained learning authority", err)
	}
	c, err = deerflow.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.LoadSession(ctx, first.ID, workspace, false, nil); err != nil {
		t.Fatal(err)
	}
	result, err := c.Run(ctx, first.ID, []harness.Content{{Type: "text", Text: "Use the adopted " + targetName + " skill to inspect config.json"}}, nil, allow)
	if err != nil || result.Execution == nil || result.Execution.Status != harness.ExecutionCompleted {
		t.Fatal("saved published skill failed after native restart", result, err)
	}
	if head, err := c.GetSkillHead(ctx, "python-custom", targetName); err != nil || head != published {
		t.Fatal("startup import replaced the adopted skill", head, err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c = nil
	check, err := sqlite.Open(filepath.Join(cfg.DataDir, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var readVersion string
	if err = check.DB().QueryRowContext(ctx, `SELECT version FROM harness_skill_reads WHERE run_id=? AND name=? AND path='SKILL.md'`, result.Execution.RunID, targetName).Scan(&readVersion); err != nil || readVersion != published.Version {
		t.Fatal("reopened native run did not actually read the published version", readVersion, err)
	}
	for _, stage := range []string{"extract", "consolidate", "match", "generate", "security", "quality", "behavior"} {
		if provider.Counts()[stage] != counts[stage] {
			t.Fatal("approval or saved opt-out regenerated or evaluated a skill", stage, provider.Counts())
		}
	}
	if data, err := os.ReadFile(filepath.Join(imported, "SKILL.md")); err != nil || string(data) != string(original) {
		t.Fatal("saved evolution overwrote imported files", err)
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
