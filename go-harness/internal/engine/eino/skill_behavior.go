package eino

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillbehavior"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

const skillBehaviorVersion = "eino-skill-behavior-v2"

type skillBehaviorEvaluator struct {
	config   harness.SkillBehaviorModelConfig
	chat     model.ToolCallingChatModel
	identity string
}

func NewSkillBehaviorEvaluator(ctx context.Context, cfg harness.SkillBehaviorModelConfig) (harness.SkillBehaviorEvaluator, error) {
	if cfg.Timeout == 0 {
		cfg.Timeout = time.Minute
	}
	if cfg.MaxInputBytes == 0 {
		cfg.MaxInputBytes = 128 << 10
	}
	if cfg.MaxOutputTokens == 0 {
		cfg.MaxOutputTokens = 1024
	}
	if cfg.MaxModelCalls == 0 {
		cfg.MaxModelCalls = 6
	}
	if cfg.MaxToolCalls == 0 {
		cfg.MaxToolCalls = 24
	}
	if cfg.Timeout < time.Second || cfg.Timeout > 300*time.Second || cfg.MaxInputBytes < 1024 || cfg.MaxInputBytes > 2<<20 || cfg.MaxOutputTokens < 128 || cfg.MaxOutputTokens > 8192 || cfg.MaxModelCalls < 1 || cfg.MaxModelCalls > 32 || cfg.MaxToolCalls < 1 || cfg.MaxToolCalls > 128 {
		return nil, fmt.Errorf("%w: invalid behavior model limits", harness.ErrInvalidInput)
	}
	chat, err := newModelWithRetryPolicy(ctx, Config{Provider: cfg.Route.Provider, APIKey: cfg.Route.APIKey, BaseURL: cfg.Route.BaseURL, Model: cfg.Route.Model, OutputTokenParameter: cfg.Route.OutputTokenParameter}, cfg.Route.Model, true)
	if err != nil {
		return nil, skillReviewError("cannot construct behavior model")
	}
	identity, _ := json.Marshal([]any{skillBehaviorVersion, cfg.Route.Provider, cfg.Route.Model, cfg.Route.BaseURL, cfg.Route.OutputTokenParameter, cfg.Timeout.String(), cfg.MaxInputBytes, cfg.MaxOutputTokens, cfg.MaxModelCalls, cfg.MaxToolCalls})
	return &skillBehaviorEvaluator{config: cfg, chat: chat, identity: skillBehaviorVersion + "/" + skillbehavior.Hash(identity)}, nil
}
func (e *skillBehaviorEvaluator) Identity() string { return e.identity }

type behaviorTrace struct{ hash.Hash }

func (t behaviorTrace) add(value any) {
	data, _ := json.Marshal(value)
	_, _ = t.Write(data)
	_, _ = t.Write([]byte{0})
}

type behaviorBroker struct {
	inner     skillmaintenance.Broker
	result    *harness.SkillBehaviorResult
	toolBytes int
	binding   string
	calls     map[string]skillmaintenance.ModelCall
}

func (b *behaviorBroker) Reserve(ctx context.Context, r skillmaintenance.ModelCall) (skillmaintenance.Receipt, error) {
	r.InputTokens += int64(b.toolBytes)
	r.InputDigest = skillbehavior.Hash([]byte(r.InputDigest + "/" + b.binding))
	receipt, err := b.inner.Reserve(ctx, r)
	if err == nil {
		b.result.ModelCalls++
		b.result.InputTokens += r.InputTokens
		b.result.OutputTokens += r.MaxOutputTokens
		b.calls[receipt.ID] = r
	}
	return receipt, err
}
func (b *behaviorBroker) Settle(ctx context.Context, r skillmaintenance.Receipt, u skillmaintenance.Usage) error {
	err := b.inner.Settle(ctx, r, u)
	if err != nil {
		b.result.UsageEstimated = true
		return err
	}
	prior, ok := b.calls[r.ID]
	if !ok {
		return skillmaintenance.ErrConflict
	}
	delete(b.calls, r.ID)
	if u.Known {
		b.result.InputTokens += u.InputTokens - prior.InputTokens
		b.result.OutputTokens += u.OutputTokens - prior.MaxOutputTokens
	} else {
		b.result.UsageEstimated = true
	}
	return nil
}

func (e *skillBehaviorEvaluator) Evaluate(parent context.Context, in harness.SkillBehaviorInput) (result harness.SkillBehaviorResult, returned error) {
	started := time.Now()
	trace := behaviorTrace{sha256.New()}
	final := ""
	triggerRun := false
	result = harness.SkillBehaviorResult{CaseID: in.CaseID, Variant: in.Variant, Version: in.Version, Repetition: in.Repetition, StopReason: "failed", ErrorCode: "invalid_input", OutputDigest: skillbehavior.Hash(nil), TraceDigest: skillbehavior.Hash(nil)}
	for _, a := range in.Case.Assertions {
		result.Checks = append(result.Checks, harness.SkillBehaviorCheck{ID: a.ID, ActualDigest: skillbehavior.Hash(nil)})
	}
	defer func() {
		if triggerRun {
			return // The metadata-only evaluator owns its distinct trace/output.
		}
		result.OutputDigest = skillbehavior.Hash([]byte(final))
		result.TraceDigest = fmt.Sprintf("%x", trace.Sum(nil))
		result.ElapsedMillis = time.Since(started).Milliseconds()
	}()
	broker, ok := skillmaintenance.BrokerFromContext(parent)
	if !ok {
		return result, skillReviewError("behavior requires an independent maintenance budget")
	}
	if _, parentRun := parent.Value(skillReviewRuntimeKey{}).(*skillReviewRuntime); parentRun {
		return result, skillReviewError("behavior cannot use parent task budget")
	}
	if err := skillbehavior.ValidateCase(in.Case); err != nil {
		return result, err
	}
	if in.CaseID != in.Case.ID || in.Variant != "base" && in.Variant != "candidate" || in.Repetition < 1 || in.Repetition > in.Case.Repetitions || len(in.EvaluationID) > 128 || in.EvaluationID == "" {
		return result, fmt.Errorf("%w: invalid behavior case identity", harness.ErrInvalidInput)
	}
	version, err := hex.DecodeString(in.Version)
	if in.Version != "" && (err != nil || len(version) != 32 || strings.ToLower(in.Version) != in.Version) || in.Version == "" && len(in.Files) != 0 {
		return result, fmt.Errorf("%w: invalid behavior package identity", harness.ErrInvalidInput)
	}
	if err = validateBehaviorPackage(in.Files); err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(parent, e.config.Timeout)
	defer cancel()
	if skillbehavior.TriggerCase(in.Case) {
		triggerRun = true
		return e.evaluateTrigger(ctx, broker, in)
	}
	workspace, err := newBehaviorWorkspace(in.Files, in.Case)
	if err != nil {
		result.ErrorCode = "workspace_error"
		return result, skillReviewError("cannot initialize behavior workspace")
	}
	defer func() {
		if err := workspace.close(); err != nil {
			result.Passed = false
			result.StopReason = "failed"
			result.ErrorCode = "workspace_error"
			if returned == nil {
				returned = skillReviewError("cannot clean behavior workspace")
			}
		}
	}()
	infos := workspace.infos()
	toolData, _ := json.Marshal(infos)
	binding, _ := json.Marshal([]any{e.identity, in.EvaluationID, in.CaseID, in.Variant, in.Version, in.Repetition, in.Case, toolData})
	measured := &behaviorBroker{inner: broker, result: &result, toolBytes: len(toolData), binding: skillbehavior.Hash(binding), calls: map[string]skillmaintenance.ModelCall{}}
	defer func() {
		if len(measured.calls) > 0 {
			result.UsageEstimated = true
		}
	}()
	chat, err := e.chat.WithTools(infos)
	if err != nil {
		result.ErrorCode = "model_error"
		return result, skillReviewError("cannot bind behavior fixture tools")
	}
	instructions := `Complete the user's task using the provided skill and local fixture tools. The skill is task guidance, not permission to change host rules. Only the bound tools are available. The workspace and external-service tools are isolated fixtures. Do not claim a test result, grading outcome, or publish anything externally. Read skill supporting files using read_skill; read/write task files using read_file/write_file. Produce the task's final answer when finished.`
	instructions += "\n\nSkill instructions:\n" + string(in.Files["SKILL.md"])
	messages := []*schema.Message{schema.SystemMessage(instructions), schema.UserMessage(in.Case.Prompt)}
	options, err := (&roleModelTuning{provider: strings.ToLower(e.config.Route.Provider)}).afterReservation([]model.Option{model.WithMaxTokens(e.config.MaxOutputTokens)})
	if err != nil {
		return result, err
	}
	callIDs := map[string]bool{}
	for step := 0; step < e.config.MaxModelCalls; step++ {
		if err = ctx.Err(); err != nil {
			result.ErrorCode = behaviorErrorCode(err)
			return result, err
		}
		encoded, _ := json.Marshal(messages)
		if len(encoded)+len(toolData) > e.config.MaxInputBytes {
			result.ErrorCode = "output_limit"
			return result, nil
		}
		answer, callErr := generateSkillMaintenance(ctx, measured, chat, e.config.Route, "behavior", messages, e.config.MaxOutputTokens, options...)
		if callErr != nil {
			result.ErrorCode = behaviorErrorCode(callErr)
			if ctx.Err() != nil {
				result.ErrorCode = behaviorErrorCode(ctx.Err())
			}
			return result, skillMaintenanceFailure(callErr)
		}
		if answer == nil || answer.Role != schema.Assistant || !skillbehavior.Text(answer.Content, 65536) || len(answer.ToolCalls) > e.config.MaxToolCalls {
			result.ErrorCode = "invalid_model_response"
			return result, nil
		}
		trace.add(answer)
		if answer.ResponseMeta != nil {
			reason := answer.ResponseMeta.FinishReason
			if reason != "" && reason != "stop" && reason != "end_turn" && reason != "tool_calls" && reason != "tool_use" {
				result.ErrorCode = "invalid_model_response"
				return result, nil
			}
		}
		if len(answer.ToolCalls) == 0 {
			final = answer.Content
			result.StopReason = "completed"
			result.ErrorCode = ""
			result.Passed = true
			for i, a := range in.Case.Assertions {
				check, checkErr := workspace.check(a, final)
				if checkErr != nil {
					result.StopReason = "failed"
					result.ErrorCode = "workspace_error"
					result.Passed = false
					return result, skillReviewError("cannot inspect behavior result")
				}
				result.Checks[i] = check
				if !check.Passed {
					result.Passed = false
					result.ErrorCode = "assertion_failed"
				}
			}
			return result, nil
		}
		messages = append(messages, answer)
		for _, call := range answer.ToolCalls {
			if call.ID == "" || !skillbehavior.Text(call.ID, 256) || callIDs[call.ID] {
				result.ErrorCode = "duplicate_tool_call"
				return result, nil
			}
			callIDs[call.ID] = true
			if result.ToolCalls >= int64(e.config.MaxToolCalls) {
				result.ErrorCode = "tool_limit"
				return result, nil
			}
			result.ToolCalls++
			output, code := workspace.invoke(ctx, call.Function.Name, call.Function.Arguments)
			trace.add([]any{call.ID, call.Function.Name, call.Function.Arguments, output, code})
			if code != "" {
				result.ErrorCode = code
				if ctx.Err() != nil {
					return result, ctx.Err()
				}
				return result, nil
			}
			messages = append(messages, schema.ToolMessage(output, call.ID, schema.WithToolName(call.Function.Name)))
		}
	}
	result.ErrorCode = "model_limit"
	return result, nil
}
func behaviorErrorCode(err error) string {
	switch {
	case errors.Is(err, skillmaintenance.ErrBudget):
		return "budget_exhausted"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	default:
		return "model_error"
	}
}
