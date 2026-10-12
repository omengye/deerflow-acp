package runtime

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
	"github.com/omengye/deerflow-acp/go-harness/internal/learning"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills/evolution"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func learningDelegationNative(t *testing.T, s *Service) *sqlite.Store {
	t.Helper()
	native := learningDelegationNativeUnconfigured(t, s)
	if err := s.Store.Learning.ConfigureRootEvidence(s.Store.LearningRootObservationsTx); err != nil {
		t.Fatal(err)
	}
	return native
}

func learningDelegationCreate(t *testing.T, s *Service, native *sqlite.Store, parent harness.RunRequest, suffix string, configure ...func(*BackgroundExecutionSpec, *background.Binding)) *backgroundHostFixture {
	t.Helper()
	ctx := context.Background()
	call := "delegate-" + suffix
	host, err := NewBackgroundExecutionStore(ctx, s.Store)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := background.NewLedgerAdapter(native, s.Store.BudgetLedger)
	if err != nil {
		t.Fatal(err)
	}
	spec := BackgroundExecutionSpec{Version: 1, Parent: parent.Session, AgentVersion: "learning-child-v1", HostPolicy: "learning-host-v1", Input: []harness.Content{{Type: "text", Text: "PRIVATE_DELEGATED_INSTRUCTION token=PRIVATE_CHILD_SECRET"}}, Extension: []byte(`{}`), OriginArguments: `{"instruction":"PRIVATE_DELEGATED_INSTRUCTION token=PRIVATE_CHILD_SECRET"}`}
	binding := background.Binding{TaskID: "learning-child-" + suffix, ChildSessionID: "learning-child-" + suffix + "/session", ParentSessionID: parent.Session.ID, OriginRunID: parent.RunID, OriginToolCallID: call, Workspace: parent.Session.CWD, RootBudgetID: parent.RootBudgetID, AgentVersion: spec.AgentVersion, ConfigVersion: parent.Session.ConfigVersion}
	for _, apply := range configure {
		apply(&spec, &binding)
	}
	binding.ExecutionContract, err = spec.Contract()
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"tool_start", "tool_execute"} {
		event := receiptEvent(parent, kind, call, "background_agent")
		event.Arguments = []byte(spec.OriginArguments)
		if _, err := s.Store.Append(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	if spec.Role != nil {
		controller, err := agents.NewController(s.Store.db)
		if err != nil {
			t.Fatal(err)
		}
		parentRole := spec.ParentInvocationID
		if parentRole == "" && spec.ParentTaskID != "" {
			parentRole = "background/" + spec.ParentTaskID
		}
		if err = controller.Register(ctx, agents.Invocation{RootID: binding.RootBudgetID, ID: binding.RoleID(), ParentID: parentRole, Depth: binding.TaskDepth(), Spec: *spec.Role}, 1, 8); err != nil {
			t.Fatal(err)
		}
	}
	f := &backgroundHostFixture{store: s.Store, native: native, host: host, adapter: adapter, spec: spec, binding: binding, scope: background.TaskScope{Binding: binding, Attempt: 1}}
	f.installTaskHooks()
	if err = f.create(); err != nil {
		t.Fatal(err)
	}
	f.start(t)
	if err = f.begin(f.scope); err != nil {
		t.Fatal(err)
	}
	event := receiptEvent(parent, "tool_end", call, "background_agent")
	event.Content = []harness.Content{{Type: "text", Text: "PRIVATE_DELEGATION_REPLY"}}
	if _, err = s.Store.Append(ctx, event); err != nil {
		t.Fatal(err)
	}
	return f
}

func learningDelegationComplete(t *testing.T, f *backgroundHostFixture, tool string) {
	t.Helper()
	ctx := context.Background()
	child, _, err := f.host.Load(ctx, f.binding)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []harness.RunEvent{
		{SessionID: child.Session.ID, RunID: child.RunID, Kind: "user_message", Text: "PRIVATE_SYNTHETIC_CORRECTION"},
		{SessionID: child.Session.ID, RunID: child.RunID, Kind: "text_delta", AgentName: "deerflow", Text: "PRIVATE_CHILD_REPLY"},
		{SessionID: child.Session.ID, RunID: child.RunID, Kind: "reasoning_delta", Text: "PRIVATE_CHILD_REASONING"},
	} {
		if _, err = f.store.Append(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	for _, kind := range []string{"tool_start", "tool_execute", "tool_end"} {
		event := receiptEvent(child, kind, "complete-file", tool)
		event.Arguments = []byte(`{"path":"configuration.json","token":"PRIVATE_RECEIPT_SECRET"}`)
		if kind == "tool_end" {
			event.Content = []harness.Content{{Type: "text", Text: `{"verified":true,"token":"PRIVATE_RESULT_SECRET"}`}}
		}
		if _, err = f.store.Append(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	task, err := f.tasks.Get(ctx, f.binding.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.tasks.Complete(ctx, &bt.CompleteTaskRequest{TaskID: task.Spec.ID, ExpectedVersion: task.Version, Data: []byte(`"PRIVATE_NATIVE_CHILD_RESULT"`)}); err != nil {
		t.Fatal(err)
	}
}

func learningDelegationSession(t *testing.T, s *Service, x harness.Session) harness.Session {
	t.Helper()
	x.Subagents, x.ConfigVersion = true, x.ConfigVersion+1
	if err := s.Store.SaveConfig(context.Background(), x); err != nil {
		t.Fatal(err)
	}
	return x
}

type learningDelegationExtractor struct {
	seen          []harness.SkillLearningInput
	after         func()
	observed      bool
	syntheticOnly bool
}

func (*learningDelegationExtractor) Identity() string { return "delegated-extract-fixture-v1" }
func (e *learningDelegationExtractor) Extract(ctx context.Context, input harness.SkillLearningInput) ([]harness.SkillExperienceCandidate, error) {
	e.seen = append(e.seen, input)
	if err := consolidationTestCall(ctx, "experience_extract"); err != nil {
		return nil, err
	}
	if e.after != nil {
		e.after()
	}
	refs := []string{}
	for _, ref := range input.References {
		if !e.syntheticOnly && ref.Kind == "user" || ref.Kind == "skill_read" || ref.Kind == "tool_observation" {
			refs = append(refs, ref.ID)
		}
	}
	if e.syntheticOnly {
		refs = append(refs, "input/learning-child-bound/input")
	}
	outcome := "unresolved"
	if e.observed {
		outcome = "observed_execution"
	}
	return []harness.SkillExperienceCandidate{{Kind: "correction_chain", Goal: "Inspect structured configuration", Applicability: "PowerShell JSON checks", Problem: "Incomplete input", Resolution: "Read complete input", Steps: []string{"Read complete input"}, Verification: "Local observation remains unassessed", Outcome: outcome, References: refs}}, nil
}

func learningDelegationRestart(t *testing.T, s *Service, options *harness.SkillLearningOptions, extractor harness.SkillExperienceExtractor, registry *skills.Registry, matcher harness.SkillExperienceMatcher) {
	t.Helper()
	ctx := context.Background()
	if err := s.Store.Learning.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := learning.New(ctx, s.Store.db, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = next.Close() })
	if err = next.ConfigureRootEvidence(s.Store.LearningRootObservationsTx); err != nil {
		t.Fatal(err)
	}
	if options != nil {
		if err = next.Configure(ctx, extractor, nil); err != nil {
			t.Fatal(err)
		}
		if options.Consolidation != nil {
			if err = next.ConfigureConsolidation(&consolidationTestModel{}); err != nil {
				t.Fatal(err)
			}
		}
		if options.Matching != nil {
			access, err := registry.NewMatchAccess(harness.SkillSelection{})
			if err != nil {
				t.Fatal(err)
			}
			if err = next.ConfigureMatching(matcher, access); err != nil {
				t.Fatal(err)
			}
		}
	}
	s.Store.Learning = next
}

func TestSkillLearningDelegatedExecutionAutomaticallyReachesReview(t *testing.T) {
	s, x, _, evolution, generator, _, _ := experienceDiscoveryTest(t)
	native := learningDelegationNative(t, s)
	ctx := context.Background()
	x = learningDelegationSession(t, s, x)
	parent := learningTestRun(t, s, x, "Inspect complete JSON input using a delegated local check")
	f := learningDelegationCreate(t, s, native, parent, "review")
	learningTestFinish(t, s, parent, "end_turn")
	learningDelegationComplete(t, f, "read_file")
	learningTestQueue(t, s)
	if worked, err := s.Store.Learning.RunOne(ctx); err != nil || !worked {
		t.Fatal(worked, err)
	}
	page, err := s.Store.Learning.Experiences(ctx, x.ID, "", 10)
	if err != nil || len(page.Experiences) != 1 || page.Experiences[0].Candidate.Outcome != "observed_execution" {
		t.Fatal("completed delegated local receipt did not automatically produce an experience", page, err)
	}
	source, err := s.Store.Learning.Source(ctx, x.ID, sourceExperienceRequest(page.Experiences[0]))
	if err != nil || len(source.References) != 2 || source.SourceSessionID != x.ID {
		t.Fatal(source, err)
	}
	data, _ := json.Marshal(source)
	for _, forbidden := range []string{"PRIVATE_", "DELEGATED_INSTRUCTION", "SYNTHETIC_CORRECTION"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatal("delegated source leaked private text", forbidden)
		}
	}
	var users, observations int
	for _, ref := range source.References {
		if ref.Kind == "user" && ref.RunID == parent.RunID {
			users++
		}
		if ref.Kind == "tool_observation" && ref.RunID == f.binding.TaskID && ref.State == "completed" && !ref.Truncated {
			observations++
		}
	}
	if users != 1 || observations != 1 || learningCount(t, s.Store, "harness_learning_inputs") != 1 || learningCount(t, s.Store, "harness_learning_outbox") != 1 {
		t.Fatal("child became an accepted input or its observation lost root attribution", source)
	}
	consolidateRun(t, s)
	experienceMatch(t, s)
	enableExperienceReviewDrafts(t, evolution, false)
	drafts := experienceDiscoveries(t, evolution)
	if len(drafts) != 1 || !drafts[0].ReviewDraft || len(drafts[0].Evidence) != 1 || drafts[0].Evidence[0].RootID != parent.RootBudgetID {
		t.Fatal("delegated evidence did not reach an automatic review-only discovery", drafts)
	}
	proposal := experienceGenerate(t, evolution)
	if proposal.Status != "pending_review" || len(generator.seen) != 1 || !generator.seen[0].InstructionOnly {
		t.Fatal(proposal, generator.seen)
	}
	approved, err := evolution.Approve(ctx, proposal.ID, harness.SkillReview{ExpectedStateVersion: proposal.StateVersion, Actor: "owner"})
	if err != nil || approved.Status != "published" {
		t.Fatal("system-generated delegated proposal could not be reviewed", approved, err)
	}
}

func TestSkillLearningDelegatedExecutionSnapshotAndLiveAuthorityGuards(t *testing.T) {
	for _, mode := range []string{"child-frontier", "child-receipt", "child-input", "binding", "child-blocked", "native-result", "parent-policy", "member-parent", "tree-parent", "new-member", "provider-output-change"} {
		t.Run(mode, func(t *testing.T) {
			s, x, _ := newLearningTest(t, true)
			x = learningDelegationSession(t, s, x)
			extractor := &learningDelegationExtractor{observed: true}
			learningDelegationRestart(t, s, learningTestOptions(), extractor, nil, nil)
			native := learningDelegationNativeUnconfigured(t, s)
			parent := learningTestRun(t, s, x, "Inspect delegated configuration")
			f := learningDelegationCreate(t, s, native, parent, "guard")
			learningTestFinish(t, s, parent, "end_turn")
			learningDelegationComplete(t, f, "read_file")
			learningTestQueue(t, s)
			var job string
			if err := s.Store.db.QueryRow(`SELECT job_id FROM df_learning_extractions`).Scan(&job); err != nil {
				t.Fatal(err)
			}
			change := func() {
				switch mode {
				case "child-frontier", "provider-output-change":
					_, err := s.Store.Append(context.Background(), harness.RunEvent{SessionID: f.binding.ChildSessionID, RunID: f.binding.TaskID, Kind: "text_delta", AgentName: "deerflow", Text: "PRIVATE_LATER_CHILD"})
					if err != nil {
						t.Fatal(err)
					}
				case "child-receipt":
					_, err := s.Store.db.Exec(`UPDATE harness_tool_receipts SET version=version+1 WHERE run_id=?`, f.binding.TaskID)
					if err != nil {
						t.Fatal(err)
					}
				case "child-input":
					_, err := s.Store.db.Exec(`UPDATE harness_inputs SET content=? WHERE run_id=?`, []byte(`[{"type":"text","text":"changed delegated goal"}]`), f.binding.TaskID)
					if err != nil {
						t.Fatal(err)
					}
				case "binding":
					binding := f.binding
					binding.RootBudgetID = "different-root"
					data, _ := json.Marshal(binding)
					_, err := s.Store.db.Exec(`UPDATE harness_background_bindings SET payload=? WHERE task_id=?`, data, f.binding.TaskID)
					if err != nil {
						t.Fatal(err)
					}
				case "child-blocked":
					if _, err := s.Store.db.Exec(`UPDATE harness_background_bindings SET blocked_reason='reconciliation_required' WHERE task_id=?`, f.binding.TaskID); err != nil {
						t.Fatal(err)
					}
				case "native-result":
					task, err := f.tasks.Get(context.Background(), f.binding.TaskID)
					if err != nil {
						t.Fatal(err)
					}
					task.ResultData = []byte(`"changed result bytes"`)
					data, _ := json.Marshal(task)
					if _, err = s.Store.db.Exec(`UPDATE eino_background_tasks SET payload=? WHERE id=?`, data, f.binding.TaskID); err != nil {
						t.Fatal(err)
					}
				case "parent-policy":
					x.Subagents, x.ConfigVersion = false, x.ConfigVersion+1
					if err := s.Store.SaveConfig(context.Background(), x); err != nil {
						t.Fatal(err)
					}
				case "member-parent":
					_, err := s.Store.db.Exec(`UPDATE budget_members SET parent_id=id WHERE id=?`, f.binding.TaskID)
					if err != nil {
						t.Fatal(err)
					}
				case "tree-parent":
					if _, err := s.Store.db.Exec(`UPDATE harness_background_tree SET parent_task_id=task_id WHERE task_id=?`, f.binding.TaskID); err != nil {
						t.Fatal(err)
					}
				case "new-member":
					// A separately authenticated tree cannot enter this root by
					// changing only its settled budget projection.
					other := learningTestRun(t, s, x, "Independent later accepted task")
					late := learningDelegationCreate(t, s, native, other, "late")
					learningDelegationComplete(t, late, "read_file")
					learningTestFinish(t, s, other, "end_turn")
					if _, err := s.Store.db.Exec(`UPDATE budget_members SET root_id=? WHERE id=?`, parent.RootBudgetID, late.binding.TaskID); err != nil {
						t.Fatal(err)
					}
				}
			}
			paid := mode == "provider-output-change"
			if paid {
				extractor.after = change
			} else {
				change()
			}
			if worked, err := s.Store.Learning.RunOne(context.Background()); err != nil || !worked {
				t.Fatal(worked, err)
			}
			saved, err := skillmaintenance.ReadJob(context.Background(), s.Store.db, job)
			if err != nil || saved.State != "failed" || saved.ErrorCode != "evidence_changed" || saved.Attempts != 1 || len(extractor.seen) != boolInt(paid) || learningCount(t, s.Store, "df_learning_experiences") != 0 {
				t.Fatal("changed root source authorized extraction or retry", saved, len(extractor.seen), err)
			}
			var calls, charged int
			if err = s.Store.db.QueryRow(`SELECT count(*),COALESCE(sum(charged_tokens),0) FROM df_skill_maintenance_calls WHERE job_id=?`, job).Scan(&calls, &charged); err != nil || calls != boolInt(paid) || charged != 15*calls {
				t.Fatal("root invalidation changed shared paid accounting", calls, charged, err)
			}
		})
	}
}

func learningDelegationNativeUnconfigured(t *testing.T, s *Service) *sqlite.Store {
	t.Helper()
	var sequence int
	var name, path string
	if err := s.Store.db.QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &path); err != nil {
		t.Fatal(err)
	}
	native, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = native.Close() })
	if _, err = background.New(context.Background(), background.Config{Store: native}); err != nil {
		t.Fatal(err)
	}
	return native
}

func TestSkillLearningDelegatedExecutionBoundsExternalAndNoSyntheticVotes(t *testing.T) {
	for _, mode := range []string{"synthetic-only", "external", "unknown", "failed-child", "too-many-children", "tree-unsettled", "forged-capture"} {
		t.Run(mode, func(t *testing.T) {
			s, x, _ := newLearningTest(t, true)
			x = learningDelegationSession(t, s, x)
			extractor := &learningDelegationExtractor{observed: true, syntheticOnly: mode == "synthetic-only"}
			learningDelegationRestart(t, s, learningTestOptions(), extractor, nil, nil)
			native := learningDelegationNativeUnconfigured(t, s)
			parent := learningTestRun(t, s, x, "Inspect actual delegated input")
			children := 1
			if mode == "too-many-children" {
				children = 9
			}
			fixtures := []*backgroundHostFixture{}
			for i := 0; i < children; i++ {
				fixtures = append(fixtures, learningDelegationCreate(t, s, native, parent, fmt.Sprintf("bound-%d", i)))
			}
			learningTestFinish(t, s, parent, "end_turn")
			if mode == "tree-unsettled" {
				time.Sleep(1050 * time.Millisecond)
				if worked, err := s.Store.Learning.QueueOne(context.Background()); err != nil || worked || len(extractor.seen) != 0 {
					t.Fatal("active child authorized paid capture", worked, err)
				}
			}
			tool := "read_file"
			if mode == "external" {
				tool = "web_fetch"
			} else if mode == "unknown" {
				tool = "custom_remote_reader"
			}
			for _, f := range fixtures {
				learningDelegationComplete(t, f, tool)
			}
			if mode == "failed-child" {
				f := fixtures[0]
				task, _ := f.tasks.Get(context.Background(), f.binding.TaskID)
				task.Status = bt.StatusFailed
				data, _ := json.Marshal(task)
				if _, err := s.Store.db.Exec(`UPDATE eino_background_tasks SET status='failed',payload=? WHERE id=?`, data, f.binding.TaskID); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Store.db.Exec(`UPDATE harness_runs SET status='failed' WHERE id=?`, f.binding.TaskID); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Store.db.Exec(`UPDATE budget_attempts SET outcome='failed' WHERE member_id=?`, f.binding.TaskID); err != nil {
					t.Fatal(err)
				}
			}
			learningTestQueue(t, s)
			if mode == "too-many-children" {
				var state, reason string
				if err := s.Store.db.QueryRow(`SELECT state,reason FROM harness_learning_outbox`).Scan(&state, &reason); err != nil || state != "skipped" || reason != "source_bounds" || len(extractor.seen) != 0 {
					t.Fatal("oversized root did not fail closed before model admission", state, reason, err)
				}
				return
			}
			if mode == "forged-capture" {
				var payload []byte
				var id, job string
				if err := s.Store.db.QueryRow(`SELECT id,job_id,payload FROM df_learning_extractions`).Scan(&id, &job, &payload); err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				_ = json.Unmarshal(payload, &fields)
				var view harness.SkillLearningInput
				_ = json.Unmarshal(fields["view"], &view)
				view.References = append(view.References, harness.SkillLearningReference{ID: "input/learning-child-bound/input", Kind: "user", RunID: fixtures[0].binding.TaskID, Text: "Pretend child correction"})
				fields["view"], _ = json.Marshal(view)
				payload, _ = json.Marshal(fields)
				hash := fmt.Sprintf("%x", sha256.Sum256(payload))
				tx, err := s.Store.db.BeginTx(context.Background(), nil)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = tx.Exec(`UPDATE df_learning_extractions SET payload=?,payload_sha=? WHERE id=?`, payload, hash, id); err == nil {
					_, err = tx.Exec(`UPDATE df_skill_maintenance_jobs SET input_digest=? WHERE id=?`, hash, job)
				}
				if err != nil {
					tx.Rollback()
					t.Fatal(err)
				}
				if err = tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			if worked, err := s.Store.Learning.RunOne(context.Background()); err != nil || !worked {
				t.Fatal(worked, err)
			}
			if mode == "synthetic-only" || mode == "external" || mode == "unknown" || mode == "forged-capture" {
				if learningCount(t, s.Store, "df_learning_experiences") != 0 {
					t.Fatal("nonlocal or synthetic source became observed experience", mode)
				}
			} else if learningCount(t, s.Store, "df_learning_experiences") != 1 {
				t.Fatal("completed local receipt not captured", mode)
			}
			for _, input := range extractor.seen {
				data, _ := json.Marshal(input)
				if strings.Contains(string(data), "PRIVATE_") {
					t.Fatal("root view exposed private or external text", mode)
				}
				users := 0
				for _, ref := range input.References {
					if ref.Kind == "user" {
						users++
					}
				}
				if users != 1 {
					t.Fatal("delegation created independent user sources", users)
				}
			}
		})
	}
}

func TestSkillLearningDelegatedExecutionRestartReadsAndRevocation(t *testing.T) {
	for _, mode := range []string{"restart-ack", "disabled-inspection", "consent", "publication-recheck", "skill-read"} {
		t.Run(mode, func(t *testing.T) {
			s, x, registry, evolution, _, _, matcher := experienceDiscoveryTest(t)
			x = learningDelegationSession(t, s, x)
			extractor := &learningDelegationExtractor{observed: true}
			o := consolidationTestOptions()
			o.Matching = &harness.SkillGenerationModelConfig{}
			learningDelegationRestart(t, s, o, extractor, registry, matcher)
			// This evolution service has not started; rebuild it with the new
			// startup-pinned learning store before enabling discovery.
			if err := evolution.Close(); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			native := learningDelegationNativeUnconfigured(t, s)
			parent := learningTestRun(t, s, x, "Inspect full configuration and validate required fields")
			f := learningDelegationCreate(t, s, native, parent, "restart")
			if mode == "skill-read" {
				view, err := registry.Snapshot(ctx, harness.SkillSelection{Workspace: x.CWD})
				if err != nil {
					t.Fatal(err)
				}
				var observation skills.ReadObservation
				if _, err = view.WithReadObserver(func(_ context.Context, e skills.ReadObservation) error { observation = e; return nil }).Get(ctx, "json-check"); err != nil {
					t.Fatal(err)
				}
				tx, err := s.Store.db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err = recordSkillReadTx(ctx, tx, background.BudgetScope(f.scope), interaction.SkillRead{RunID: f.binding.TaskID, SessionID: f.binding.ChildSessionID, Read: observation}); err != nil {
					tx.Rollback()
					t.Fatal(err)
				}
				if err = tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			learningTestFinish(t, s, parent, "end_turn")
			learningDelegationComplete(t, f, "read_file")
			learningTestQueue(t, s)
			matchingRun(t, s)
			experience := sourceExperience(t, s, x)
			request := sourceExperienceRequest(experience)
			source, err := s.Store.Learning.Source(ctx, x.ID, request)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "restart-ack" {
				if _, err := s.Store.db.Exec(`UPDATE df_skill_maintenance_jobs SET state='retry',token='',lease_ns=0,retry_ns=0 WHERE job_kind='experience_extract'`); err != nil {
					t.Fatal(err)
				}
				learningDelegationRestart(t, s, o, extractor, registry, matcher)
				matchingRun(t, s)
				if len(extractor.seen) != 1 {
					t.Fatal("root checkpoint recovery made a new model call", len(extractor.seen))
				}
			} else if mode == "disabled-inspection" {
				learningDelegationRestart(t, s, nil, nil, nil, nil)
			} else if mode == "consent" {
				if _, err := s.SetSkillLearningControl(ctx, "owner", x.ID, harness.SkillLearningControlRequest{ExpectedEpoch: 1, Contribute: false}); err != nil {
					t.Fatal(err)
				}
				if got, err := s.Store.Learning.Source(ctx, x.ID, request); err == nil || len(got.References) != 0 {
					t.Fatal("parent revocation retained delegated learning evidence", got, err)
				}
				return
			} else if mode == "publication-recheck" {
				consolidateRun(t, s)
				experienceMatch(t, s)
				next, err := newLearningDelegationEvolution(ctx, registry, s.Store.Learning)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = next.Close() })
				enableExperienceReviewDrafts(t, next, false)
				if discoveries := experienceDiscoveries(t, next); len(discoveries) != 1 {
					t.Fatal(discoveries)
				}
				p := experienceGenerate(t, next)
				if _, err := s.Store.db.Exec(`UPDATE harness_tool_receipts SET version=version+1 WHERE run_id=?`, f.binding.TaskID); err != nil {
					t.Fatal(err)
				}
				if _, err = next.Approve(ctx, p.ID, harness.SkillReview{ExpectedStateVersion: p.StateVersion, Actor: "owner"}); err == nil {
					t.Fatal("changed delegated receipt authorized publication")
				}
				return
			}
			got, err := s.Store.Learning.Source(ctx, x.ID, request)
			if err != nil || learningRootDigest(got) != learningRootDigest(source) {
				t.Fatal("root source did not survive bounded restart/disabled inspection", got, err)
			}
			if mode == "skill-read" {
				consolidateRun(t, s)
				experienceMatch(t, s)
				if len(matcher.seen) != 1 || !matcher.seen[0].Skills[0].ActuallyRead || !matcher.seen[0].Skills[0].CurrentVersionRead {
					t.Fatal("delegated actual skill read lost matching attribution", matcher.seen)
				}
				if _, err := s.Store.db.Exec(`UPDATE harness_skill_reads SET file_hash=? WHERE run_id=?`, strings.Repeat("a", 64), f.binding.TaskID); err != nil {
					t.Fatal(err)
				}
				if _, err = s.Store.Learning.Source(ctx, x.ID, request); err == nil {
					t.Fatal("changed delegated skill read remained authoritative")
				}
			}
		})
	}
}

func newLearningDelegationEvolution(ctx context.Context, registry *skills.Registry, source *learning.Store) (*evolution.Service, error) {
	e, err := evolution.New(ctx, registry, &experienceDiscoveryValidator{})
	if err == nil {
		err = e.ConfigureGeneration(ctx, &experienceDiscoveryGenerator{}, consolidationTestOptions().Budget, []string{"project"}, false)
	}
	if err == nil {
		err = e.ConfigureExperienceDiscoverySource(source)
	}
	return e, err
}

func TestSkillLearningDelegatedExecutionIndependentSharedAutoPublication(t *testing.T) {
	for _, mode := range []string{"publish", "sharing-revoked", "child-source-changed"} {
		t.Run(mode, func(t *testing.T) {
			s, first, registry, extractor, matcher := newMatchingTest(t)
			ctx := context.Background()
			adoptLearningBase(t, registry)
			o := consolidationTestOptions()
			o.CrossSession, o.Matching = true, &harness.SkillGenerationModelConfig{}
			learningDelegationRestart(t, s, o, extractor, registry, matcher)
			native := learningDelegationNativeUnconfigured(t, s)
			second := workspaceLearningSession(t, s, first.CWD)
			first, second = learningDelegationSession(t, s, first), learningDelegationSession(t, s, second)
			var last *backgroundHostFixture
			for i, x := range []harness.Session{first, second} {
				workspaceLearningConsent(t, s, x, true)
				parent := learningTestRun(t, s, x, fmt.Sprintf("Independent delegated JSON check number %d", i+1))
				last = learningDelegationCreate(t, s, native, parent, fmt.Sprintf("independent-%d", i))
				learningTestFinish(t, s, parent, "end_turn")
				learningDelegationComplete(t, last, "read_file")
				learningTestQueue(t, s)
				matchingRun(t, s)
				consolidateRun(t, s)
			}
			experienceMatch(t, s)
			patterns, err := s.Store.Learning.WorkspacePatterns(ctx, first.ID, "", 10)
			if err != nil || len(patterns.Patterns) != 1 || patterns.Patterns[0].SupportEpisodes != 2 || patterns.Patterns[0].ObservedEpisodes != 2 {
				t.Fatal("children inflated independent support or lost authorized roots", patterns, err)
			}
			generator := &experienceDiscoveryGenerator{preserveFrontmatter: true}
			e, err := evolution.New(ctx, registry, &experienceDiscoveryValidator{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = e.Close() })
			if err = e.ConfigureGeneration(ctx, generator, o.Budget, []string{"project"}, false); err != nil {
				t.Fatal(err)
			}
			if err = e.ConfigureExperienceDiscoverySource(s.Store.Learning); err != nil {
				t.Fatal(err)
			}
			enableExperienceDiscovery(t, e)
			evaluator := &autoPatchLearningEvaluator{}
			if err = e.ConfigureBehavior(evaluator, harness.SkillBehaviorOptions{Suites: []harness.SkillBehaviorSuite{autoPatchLearningSuite()}}); err != nil {
				t.Fatal(err)
			}
			if err = e.ConfigureAutoPatch(learningAutoPatchOptions(true)); err != nil {
				t.Fatal(err)
			}
			discoveries := experienceDiscoveries(t, e)
			if len(discoveries) != 1 || discoveries[0].ReviewDraft || len(discoveries[0].Evidence) != 2 {
				t.Fatal("authorized independent delegated observations lost automatic path", discoveries)
			}
			proposal := autoPatchLearningProposal(t, e, discoveries[0])
			if mode == "sharing-revoked" {
				workspaceLearningConsent(t, s, second, false)
			} else if mode == "child-source-changed" {
				if _, err = s.Store.db.Exec(`UPDATE harness_tool_receipts SET version=version+1 WHERE run_id=?`, last.binding.TaskID); err != nil {
					t.Fatal(err)
				}
			}
			if err = e.RunAutoPatch(ctx); err != nil {
				t.Fatal(err)
			}
			got, err := e.Get(ctx, proposal.ID)
			revokedGone := mode == "sharing-revoked" && errors.Is(err, harness.ErrNotFound)
			if err != nil && !revokedGone || mode == "publish" && got.Status != "published" || mode != "publish" && got.Status == "published" || len(generator.seen) != 1 || evaluator.calls != 4 {
				t.Fatal("delegated source bypassed guarded auto-publication", got, evaluator.calls, err)
			}
			var consumed, feedback int
			if err = s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_discovery_episode_consumed`).Scan(&consumed); err != nil || consumed != 2 {
				t.Fatal("delegated children created or refunded episode consumption", consumed, err)
			}
			if err = s.Store.db.QueryRow(`SELECT count(*) FROM df_skill_feedback`).Scan(&feedback); err != nil || feedback != 0 {
				t.Fatal("delegated execution fabricated quality feedback", feedback, err)
			}
		})
	}
}

func TestSkillLearningDelegatedExecutionNestedAndNotificationContinuations(t *testing.T) {
	for _, mode := range []string{"nested", "root-notification", "background-notification", "changed-notification"} {
		t.Run(mode, func(t *testing.T) {
			s, x, _, e, _, _, _ := experienceDiscoveryTest(t)
			x = learningDelegationSession(t, s, x)
			native := learningDelegationNative(t, s)
			ctx := context.Background()
			depth, enabled := 3, true
			config := harness.SubagentsConfig{MaxDepth: &depth, AutoContinue: &enabled,
				Agents:       map[string]harness.AgentConfig{"general-purpose": {DelegateTo: []string{"leaf"}}},
				CustomAgents: map[string]harness.AgentConfig{"leaf": {Description: "Local input verifier", SystemPrompt: "Check complete local input"}}}
			roles, err := agents.New(config, nil)
			if err != nil {
				t.Fatal(err)
			}
			catalog, err := agents.Snapshot(&config)
			if err != nil {
				t.Fatal(err)
			}
			parentSpec, err := roles.Resolve("general-purpose", agents.Parent{Model: x.Model})
			if err != nil {
				t.Fatal(err)
			}
			leafSpec, err := roles.Resolve("leaf", agents.Parent{Model: parentSpec.Model, Tools: parentSpec.Tools, Skills: parentSpec.Skills, TimeoutSeconds: parentSpec.TimeoutSeconds})
			if err != nil {
				t.Fatal(err)
			}
			parent := learningTestRun(t, s, x, "Inspect full local JSON using a delegated verifier")
			owner := learningDelegationCreate(t, s, native, parent, "nested-owner", func(spec *BackgroundExecutionSpec, b *background.Binding) {
				spec.Version, spec.Depth, spec.Role, spec.Roles, spec.Automatic = 5, 1, &parentSpec, catalog, ContinuationPolicy(catalog)
				b.Depth = 1
			})
			var child *backgroundHostFixture
			if mode != "root-notification" {
				request, _, err := owner.host.Load(ctx, owner.binding)
				if err != nil {
					t.Fatal(err)
				}
				child = learningDelegationCreate(t, s, native, request, "nested-leaf", func(spec *BackgroundExecutionSpec, b *background.Binding) {
					spec.Version, spec.Depth, spec.Role, spec.Roles, spec.Automatic = 5, 2, &leafSpec, catalog, ContinuationPolicy(catalog)
					spec.ParentTaskID, spec.ParentInvocationID = owner.binding.TaskID, owner.binding.RoleID()
					spec.OriginArguments = `{"instruction":"PRIVATE_DELEGATED_INSTRUCTION token=PRIVATE_CHILD_SECRET","role":"leaf"}`
					b.Depth, b.RootSessionID, b.ParentTaskID, b.ParentInvocationID = 2, x.ID, owner.binding.TaskID, owner.binding.RoleID()
				})
				learningDelegationComplete(t, child, "read_file")
			}
			learningTestFinish(t, s, parent, "end_turn")
			if mode == "background-notification" || mode == "changed-notification" {
				role := agents.Invocation{RootID: parent.RootBudgetID, ID: owner.binding.RoleID(), Depth: 1, Spec: parentSpec}
				if err = executionTx(ctx, s.Store, func(tx *sql.Tx) error {
					return stageRoleContextTx(ctx, tx, background.BudgetScope(owner.scope), interaction.RoleContext{SessionID: owner.binding.ChildSessionID, RunID: owner.binding.TaskID, Invocation: role, Messages: []byte(`[{"role":"user","content":"PRIVATE_ROLE_INPUT"},{"role":"assistant","content":"PRIVATE_ROLE_RESULT"}]`)})
				}); err != nil {
					t.Fatal(err)
				}
			}
			learningDelegationComplete(t, owner, "read_file")
			var note bt.Notification
			if mode != "nested" {
				target := owner
				if child != nil {
					target = child
				}
				task, err := target.tasks.Get(ctx, target.binding.TaskID)
				if err != nil {
					t.Fatal(err)
				}
				note = bt.Notification{ID: "learning-delegated-notification", TaskID: task.Spec.ID, SessionID: target.binding.ParentSessionID, Kind: bt.NotificationCompleted, Version: task.Version, Data: []byte(`"PRIVATE_NOTIFICATION_BODY"`), CreatedAt: time.Now().UTC()}
				data, _ := json.Marshal(note)
				if _, err = s.Store.db.Exec(`INSERT INTO harness_background_inbox(notification_id,parent_session_id,task_id,payload) VALUES(?,?,?,?)`, note.ID, note.SessionID, note.TaskID, data); err != nil {
					t.Fatal(err)
				}
				s.ContinuationHostPolicy = owner.spec.HostPolicy
			}
			if mode == "root-notification" {
				var request harness.RunRequest
				var lease ExecutionLease
				if err = executionTx(ctx, s.Store, func(tx *sql.Tx) error {
					var err error
					request, lease, _, err = s.beginContinuationTx(ctx, tx, "owner", x.ID, note.ID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				for _, kind := range []string{"tool_start", "tool_execute", "tool_end"} {
					event := receiptEvent(request, kind, "continuation-check", "read_file")
					event.Arguments = []byte(`{"path":"configuration.json"}`)
					if kind == "tool_end" {
						event.Content = []harness.Content{{Type: "text", Text: `{"verified":true}`}}
					}
					if _, err = s.Store.Append(ctx, event); err != nil {
						t.Fatal(err)
					}
				}
				if _, err = s.finishExecutionAttempt(ctx, lease, harness.RunResult{StopReason: "end_turn"}, nil, nil); err != nil {
					t.Fatal(err)
				}
			} else if mode == "background-notification" || mode == "changed-notification" {
				c, _, err := s.PrepareBackgroundContinuation(ctx, "owner", x.ID, note.ID)
				if err != nil {
					t.Fatal(err)
				}
				bridge := &backgroundTargetFixture{owner: owner, child: child, service: s}
				if err = admitContinuationFixture(ctx, bridge, c); err != nil {
					t.Fatal(err)
				}
				resumed := &backgroundHostFixture{store: s.Store, native: native, host: owner.host, adapter: owner.adapter, spec: c.Spec, binding: c.Binding, scope: background.TaskScope{Binding: c.Binding, Attempt: 1}}
				resumed.installTaskHooks()
				resumed.start(t)
				if err = resumed.begin(resumed.scope); err != nil {
					t.Fatal(err)
				}
				learningDelegationComplete(t, resumed, "read_file")
			}
			learningTestQueue(t, s)
			if mode == "changed-notification" {
				note.Data = []byte(`"changed authentic source bytes"`)
				data, _ := json.Marshal(note)
				if _, err = s.Store.db.Exec(`UPDATE harness_background_inbox SET payload=? WHERE notification_id=?`, data, note.ID); err != nil {
					t.Fatal(err)
				}
			}
			matchingRun(t, s)
			if mode == "changed-notification" {
				var state, code string
				if err = s.Store.db.QueryRow(`SELECT state,error_code FROM df_skill_maintenance_jobs WHERE job_kind='experience_extract'`).Scan(&state, &code); err != nil || state != "failed" || code != "evidence_changed" || learningCount(t, s.Store, "df_learning_experiences") != 0 {
					t.Fatal("changed continuation provenance admitted delegated evidence", state, code, err)
				}
				return
			}
			experience := sourceExperience(t, s, x)
			if experience.Candidate.Outcome != "observed_execution" || learningCount(t, s.Store, "harness_learning_inputs") != 1 || learningCount(t, s.Store, "harness_learning_outbox") != 1 {
				t.Fatal("nested/continuation execution lost evidence or fabricated user inputs", experience)
			}
			consolidateRun(t, s)
			experienceMatch(t, s)
			enableExperienceReviewDrafts(t, e, false)
			drafts := experienceDiscoveries(t, e)
			if len(drafts) != 1 || !drafts[0].ReviewDraft || len(drafts[0].Evidence) != 1 || drafts[0].Evidence[0].RootID != parent.RootBudgetID {
				t.Fatal("nested/continuation children counted as independent roots", drafts)
			}
			proposal := experienceGenerate(t, e)
			if proposal.Status != "pending_review" {
				t.Fatal(proposal)
			}
		})
	}
}
