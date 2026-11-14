package eino

// Harness behavior evaluation set.
//
// Every JSON file under testdata/behavior-evals is one scenario: a scripted
// model, fixture tools and outcome assertions run through the production
// runtime path (durable SQLite session store, checkpoints and budget ledger).
// The scenarios describe what the harness must do for an agent, not how it is
// implemented, so they stay valid across refactors. CI runs them as a named
// gate (`go test ./internal/engine/eino -run '^TestBehaviorEval$'`).
//
// Add a scenario by dropping a new file in the directory; no Go change is
// needed unless it requires a new kind of step or assertion.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	openaiapi "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/assets"
	durablebudget "github.com/omengye/deerflow-acp/go-harness/internal/budget"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

const behaviorEvalDir = "testdata/behavior-evals"

// The gate refuses to pass on a thinned-out set.
const minBehaviorEvalScenarios = 20

type evalScenario struct {
	Name           string                    `json:"name"`
	Description    string                    `json:"description"`
	Category       string                    `json:"category"`
	Budget         *evalBudget               `json:"budget,omitempty"`
	Compaction     *harness.CompactionConfig `json:"compaction,omitempty"`
	PermissionMode harness.PermissionMode    `json:"permissionMode,omitempty"`
	Tools          []evalTool                `json:"tools"`
	Model          []evalModelStep           `json:"model"`
	Turns          []evalTurn                `json:"turns"`
	Expect         evalExpect                `json:"expect"`
}

type evalBudget struct {
	MaxModelCalls   int   `json:"maxModelCalls"`
	MaxToolCalls    int   `json:"maxToolCalls"`
	MaxTokens       int64 `json:"maxTokens"`
	MaxOutputTokens int   `json:"maxOutputTokens"`
}

// evalTool answers each invocation with the next result; the last result
// repeats once the list is exhausted.
type evalTool struct {
	Name    string           `json:"name"`
	Results []evalToolResult `json:"results"`
}

type evalToolResult struct {
	Output string `json:"output,omitempty"`
	// RepeatOutput multiplies Output, for oversized results.
	RepeatOutput int    `json:"repeatOutput,omitempty"`
	Error        string `json:"error,omitempty"`
	// Hang blocks until the run is cancelled or the process "crashes".
	Hang bool `json:"hang,omitempty"`
}

// evalModelStep is served for one root model call (summarization calls are
// answered automatically and are not part of the script).
type evalModelStep struct {
	Text      string          `json:"text,omitempty"`
	ToolCalls []evalToolCall  `json:"toolCalls,omitempty"`
	Error     *evalModelError `json:"error,omitempty"`
	// Repeat serves this step N times in total (default 1).
	Repeat int `json:"repeat,omitempty"`
	// Optional steps may remain unused, e.g. when a guard decides how many
	// loop iterations happen.
	Optional bool `json:"optional,omitempty"`
	// Turn pins the step to one scenario turn (0-based); unpinned steps are
	// served in order to whichever turn needs a model call.
	Turn   *int            `json:"turn,omitempty"`
	Expect *evalInputCheck `json:"expectInput,omitempty"`
}

type evalToolCall struct {
	Name string `json:"name"`
	Args string `json:"args,omitempty"`
}

type evalModelError struct {
	Status  int    `json:"status"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type evalInputCheck struct {
	Contains       []string `json:"contains,omitempty"`
	Absent         []string `json:"absent,omitempty"`
	SystemContains []string `json:"systemContains,omitempty"`
	ClosedHistory  bool     `json:"closedHistory,omitempty"`
	MaxBytes       int      `json:"maxBytes,omitempty"`
}

type evalTurn struct {
	// Action is "prompt" (default), "resume" (the latest waiting execution),
	// "restart" (clean process restart over the same database),
	// "cancelDuringTool" or "crashDuringTool" (prompt whose run is stopped
	// once a hanging tool starts; crash restarts the process on the same DB).
	Action     string         `json:"action,omitempty"`
	Prompt     string         `json:"prompt,omitempty"`
	Permission string         `json:"permission,omitempty"` // allow (default), reject, cancel
	Expect     evalTurnExpect `json:"expect"`
}

type evalTurnExpect struct {
	StopReason      string   `json:"stopReason,omitempty"`
	Limit           string   `json:"limit,omitempty"`
	Error           string   `json:"error,omitempty"` // substring; "*" means any error
	TextContains    []string `json:"textContains,omitempty"`
	EventKinds      []string `json:"eventKinds,omitempty"`
	Permissions     *int     `json:"permissions,omitempty"`
	ExecutionStatus string   `json:"executionStatus,omitempty"`
}

type evalExpect struct {
	ToolRuns      map[string]int `json:"toolRuns,omitempty"`
	ModelCalls    *int           `json:"modelCalls,omitempty"`
	MinModelCalls int            `json:"minModelCalls,omitempty"`
	MaxModelCalls int            `json:"maxModelCalls,omitempty"`
	Summaries     *int           `json:"summaries,omitempty"`
}

func TestBehaviorEval(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(behaviorEvalDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < minBehaviorEvalScenarios {
		t.Fatalf("behavior eval set has %d scenarios; the gate requires at least 20", len(files))
	}
	names := map[string]string{}
	for _, file := range files {
		scenario := loadEvalScenario(t, file)
		if previous, ok := names[scenario.Name]; ok {
			t.Fatalf("scenario %q defined in %s and %s", scenario.Name, previous, file)
		}
		names[scenario.Name] = file
		t.Run(scenario.Name, func(t *testing.T) {
			t.Parallel()
			runEvalScenario(t, scenario)
		})
	}
}

func loadEvalScenario(t *testing.T, file string) evalScenario {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var scenario evalScenario
	if err = decoder.Decode(&scenario); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	if scenario.Name == "" || scenario.Description == "" || scenario.Category == "" || len(scenario.Turns) == 0 || len(scenario.Model) == 0 {
		t.Fatalf("%s: name, description, category, model and turns are required", file)
	}
	return scenario
}

// evalHarness is one scenario's world: the database outlives simulated
// process restarts, the model script and tool fixtures are shared.
type evalHarness struct {
	t        *testing.T
	scenario evalScenario
	path     string
	model    *evalModel
	tools    []tool.BaseTool
	fixtures map[string]*evalFixtureTool
	service  *hr.Service
	session  harness.Session
	// releaseLocks drops the current process's OS-level locks, as the kernel
	// does when a process dies.
	releaseLocks func()
}

func runEvalScenario(t *testing.T, scenario evalScenario) {
	h := &evalHarness{t: t, scenario: scenario, path: filepath.Join(t.TempDir(), "runtime.db"), fixtures: map[string]*evalFixtureTool{}}
	h.model = newEvalModel(t, scenario.Model)
	for _, spec := range scenario.Tools {
		fixture := &evalFixtureTool{spec: spec, hung: make(chan struct{}, 16)}
		h.fixtures[spec.Name] = fixture
		h.tools = append(h.tools, fixture)
	}
	ctx := context.Background()
	h.service = h.startProcess()
	session, err := h.service.NewSession(ctx, "owner", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h.session = session
	for i, turn := range scenario.Turns {
		h.model.setTurn(i)
		h.runTurn(i, turn)
	}
	h.model.finish()
	for name, want := range scenario.Expect.ToolRuns {
		fixture := h.fixtures[name]
		if fixture == nil {
			t.Fatalf("expect.toolRuns names undeclared tool %q", name)
		}
		if got := fixture.runs(); got != want {
			t.Errorf("tool %s ran %d times, want %d", name, got, want)
		}
	}
	calls, summaries := h.model.counts()
	if want := scenario.Expect.ModelCalls; want != nil && calls != *want {
		t.Errorf("model calls=%d, want %d", calls, *want)
	}
	if min := scenario.Expect.MinModelCalls; min > 0 && calls < min {
		t.Errorf("model calls=%d, want at least %d", calls, min)
	}
	if max := scenario.Expect.MaxModelCalls; max > 0 && calls > max {
		t.Errorf("model calls=%d, want at most %d", calls, max)
	}
	if want := scenario.Expect.Summaries; want != nil && summaries != *want {
		t.Errorf("summaries=%d, want %d", summaries, *want)
	}
}

// startProcess opens a fresh "process": its own database handle, in-memory
// locks, engine and runtime service over the scenario's durable database.
func (h *evalHarness) startProcess() *hr.Service {
	t, ctx := h.t, context.Background()
	native, err := sqlite.Open(h.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { native.Close() })
	store, err := hr.NewStore(ctx, native.DB())
	if err != nil {
		t.Fatal(err)
	}
	// Production always wires the asset store for tool-output snapshots.
	assetStore, err := assets.NewStore(ctx, filepath.Join(filepath.Dir(h.path), "assets"), native.DB())
	if err != nil {
		t.Fatal(err)
	}
	h.releaseLocks = func() { assetStore.Close() }
	t.Cleanup(h.releaseLocks)
	ledger, err := durablebudget.New(native.DB(), durablebudget.Config{})
	if err != nil {
		t.Fatal(err)
	}
	limits := harness.BudgetLimits{MaxModelCalls: 40, MaxToolCalls: 40, MaxTokens: 1_000_000, MaxOutputTokens: 1000}
	if b := h.scenario.Budget; b != nil {
		limits = harness.BudgetLimits{MaxModelCalls: b.MaxModelCalls, MaxToolCalls: b.MaxToolCalls, MaxTokens: b.MaxTokens, MaxOutputTokens: b.MaxOutputTokens}
		if limits.MaxTokens == 0 {
			limits.MaxTokens = 1_000_000
		}
		if limits.MaxOutputTokens == 0 {
			limits.MaxOutputTokens = 1000
		}
	}
	store.BudgetLedger, store.BudgetLimits = ledger, limits
	config := Config{ChatModel: h.model, Tools: h.tools, Budget: limits, BudgetLedger: ledger, SessionStore: native, CheckpointStore: native, DisableSubAgent: true, PermissionMode: h.scenario.PermissionMode, ToolOutputStore: assetStore}
	if h.scenario.Compaction != nil {
		config.Compaction = *h.scenario.Compaction
	}
	e, err := New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	s := hr.NewService(store, e, "test")
	s.PermissionMode = h.scenario.PermissionMode
	return s
}

type evalTurnOutcome struct {
	result      harness.RunResult
	err         error
	text        strings.Builder
	kinds       map[string]bool
	permissions int
}

func (h *evalHarness) runTurn(index int, turn evalTurn) {
	t := h.t
	label := fmt.Sprintf("turn %d (%s)", index, cmpOr(turn.Action, "prompt"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	outcome := &evalTurnOutcome{kinds: map[string]bool{}}
	var mu sync.Mutex
	emit := func(_ context.Context, event harness.RunEvent) error {
		mu.Lock()
		defer mu.Unlock()
		outcome.kinds[event.Kind] = true
		if event.Kind == "text_delta" {
			outcome.text.WriteString(event.Text)
		}
		return nil
	}
	approve := func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		mu.Lock()
		outcome.permissions++
		mu.Unlock()
		switch turn.Permission {
		case "reject":
			return harness.RejectOnce, nil
		case "cancel":
			return harness.PermissionCancelled, nil
		}
		return harness.AllowOnce, nil
	}
	input := []harness.Content{{Type: "text", Text: turn.Prompt}}
	switch turn.Action {
	case "", "prompt":
		outcome.result, outcome.err = h.service.Run(ctx, "owner", h.session.ID, input, emit, approve)
	case "resume":
		state, err := h.service.Execution(ctx, "owner", h.session.ID, "")
		if err != nil {
			t.Fatalf("%s: no execution to resume: %v", label, err)
		}
		outcome.result, outcome.err = h.service.ResumeExecution(ctx, "owner", h.session.ID, harness.ResumeExecutionRequest{RunID: state.RunID, ExpectedVersion: state.Version}, emit, approve)
	case "cancelDuringTool", "crashDuringTool":
		runCtx, stop := context.WithCancel(ctx)
		defer stop()
		done := make(chan struct{})
		go func() {
			defer close(done)
			outcome.result, outcome.err = h.service.Run(runCtx, "owner", h.session.ID, input, emit, approve)
		}()
		if !h.waitForHang(ctx, done) {
			t.Fatalf("%s: no hanging tool started", label)
		}
		if turn.Action == "cancelDuringTool" {
			if err := h.service.Cancel(ctx, "owner", h.session.ID); err != nil {
				t.Fatalf("%s: cancel: %v", label, err)
			}
			<-done
			break
		}
		// The old process is abandoned: its goroutine is released only at
		// scenario end and nothing it holds in memory is reused.
		t.Cleanup(func() { stop(); <-done })
		h.releaseLocks()
		h.service = h.startProcess()
		if err := h.service.Store.ReconcileInterrupted(ctx); err != nil {
			t.Fatalf("%s: startup reconciliation: %v", label, err)
		}
		if _, err := h.service.Load(ctx, "owner", h.session.ID, h.session.CWD, false, nil); err != nil {
			t.Fatalf("%s: load after restart: %v", label, err)
		}
		mu.Lock()
		outcome.result, outcome.err = harness.RunResult{StopReason: "crashed"}, nil
		mu.Unlock()
	case "restart":
		// A clean process restart between turns.
		h.releaseLocks()
		h.service = h.startProcess()
		if err := h.service.Store.ReconcileInterrupted(ctx); err != nil {
			t.Fatalf("%s: startup reconciliation: %v", label, err)
		}
		if _, err := h.service.Load(ctx, "owner", h.session.ID, h.session.CWD, false, nil); err != nil {
			t.Fatalf("%s: load after restart: %v", label, err)
		}
		outcome.result.StopReason = "restarted"
	default:
		t.Fatalf("%s: unknown action", label)
	}
	h.model.checkErrors(label)
	mu.Lock()
	defer mu.Unlock()
	want := turn.Expect
	if want.Error == "" && outcome.err != nil {
		t.Fatalf("%s: unexpected error: %v (result %+v)", label, outcome.err, outcome.result)
	}
	if want.Error != "" {
		if outcome.err == nil {
			t.Fatalf("%s: expected error %q, got result %+v", label, want.Error, outcome.result)
		}
		if want.Error != "*" && !strings.Contains(outcome.err.Error(), want.Error) {
			t.Fatalf("%s: error %q does not contain %q", label, outcome.err, want.Error)
		}
	}
	if want.StopReason != "" && outcome.result.StopReason != want.StopReason {
		t.Fatalf("%s: stopReason=%q, want %q (limit=%q err=%v)", label, outcome.result.StopReason, want.StopReason, outcome.result.Limit, outcome.err)
	}
	if want.Limit != "" && outcome.result.Limit != want.Limit {
		t.Fatalf("%s: limit=%q, want %q", label, outcome.result.Limit, want.Limit)
	}
	for _, text := range want.TextContains {
		if !strings.Contains(outcome.text.String(), text) {
			t.Fatalf("%s: streamed text %q lacks %q", label, outcome.text.String(), text)
		}
	}
	for _, kind := range want.EventKinds {
		if !outcome.kinds[kind] {
			t.Fatalf("%s: no %q event (saw %v)", label, kind, mapsKeys(outcome.kinds))
		}
	}
	if want.Permissions != nil && outcome.permissions != *want.Permissions {
		t.Fatalf("%s: permission requests=%d, want %d", label, outcome.permissions, *want.Permissions)
	}
	if want.ExecutionStatus != "" {
		state, err := h.service.Execution(ctx, "owner", h.session.ID, "")
		if err != nil || string(state.Status) != want.ExecutionStatus {
			t.Fatalf("%s: execution status=%q err=%v, want %q", label, state.Status, err, want.ExecutionStatus)
		}
	}
}

func (h *evalHarness) waitForHang(ctx context.Context, done <-chan struct{}) bool {
	cases := make(chan struct{}, 1)
	for _, fixture := range h.fixtures {
		go func(f *evalFixtureTool) {
			select {
			case <-f.hung:
				cases <- struct{}{}
			case <-ctx.Done():
			}
		}(fixture)
	}
	select {
	case <-cases:
		return true
	case <-done:
		return false
	case <-ctx.Done():
		return false
	}
}

type evalFixtureTool struct {
	spec  evalTool
	mu    sync.Mutex
	count int
	hung  chan struct{}
}

func (f *evalFixtureTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: f.spec.Name, Desc: "behavior eval fixture " + f.spec.Name, ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{"value": {Type: schema.String}})}, nil
}

func (f *evalFixtureTool) InvokableRun(ctx context.Context, _ string, _ ...tool.Option) (string, error) {
	f.mu.Lock()
	index := f.count
	f.count++
	f.mu.Unlock()
	if len(f.spec.Results) == 0 {
		return "ok", nil
	}
	result := f.spec.Results[min(index, len(f.spec.Results)-1)]
	if result.Hang {
		f.hung <- struct{}{}
		<-ctx.Done()
		return "", ctx.Err()
	}
	if result.Error != "" {
		return "", errors.New(result.Error)
	}
	if result.RepeatOutput > 1 {
		return strings.Repeat(result.Output, result.RepeatOutput), nil
	}
	return result.Output, nil
}

func (f *evalFixtureTool) runs() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.count
}

// evalModel serves the expanded script. Input checks run inside the model
// call; failures are collected and reported on the test goroutine.
type evalModel struct {
	*scriptedModel
	mu        sync.Mutex
	steps     []evalModelStep
	used      []bool
	turn      int
	served    int
	summaries int
	failures  []string
	t         *testing.T
}

func newEvalModel(t *testing.T, script []evalModelStep) *evalModel {
	m := &evalModel{t: t}
	for _, step := range script {
		for range max(step.Repeat, 1) {
			m.steps = append(m.steps, step)
		}
	}
	m.used = make([]bool, len(m.steps))
	m.scriptedModel = &scriptedModel{stream: m.stream}
	return m
}

func (m *evalModel) setTurn(turn int) {
	m.mu.Lock()
	m.turn = turn
	m.mu.Unlock()
}

func (m *evalModel) stream(_ context.Context, _ int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(input) > 0 && (strings.Contains(input[0].Content, "tasked with summarizing conversations") || strings.Contains(input[0].Content, "负责总结对话")) {
		m.summaries++
		return textStream("Summary of earlier work: the task is in progress."), nil
	}
	next := -1
	for i, step := range m.steps {
		if !m.used[i] && (step.Turn == nil || *step.Turn == m.turn) {
			next = i
			break
		}
	}
	if next < 0 {
		m.failures = append(m.failures, fmt.Sprintf("model call %d in turn %d has no remaining script step", m.served, m.turn))
		m.served++
		return textStream("script exhausted"), nil
	}
	index := m.served
	step := m.steps[next]
	m.used[next] = true
	m.served++
	if step.Expect != nil {
		m.checkInput(index, step.Expect, input)
	}
	switch {
	case step.Error != nil:
		err := &openaiapi.APIError{HTTPStatusCode: step.Error.Status, Code: step.Error.Code, Message: cmpOr(step.Error.Message, http.StatusText(step.Error.Status))}
		return nil, err
	case len(step.ToolCalls) > 0:
		calls := make([]schema.ToolCall, 0, len(step.ToolCalls))
		for i, call := range step.ToolCalls {
			calls = append(calls, schema.ToolCall{ID: fmt.Sprintf("call-%d-%d", index, i), Type: "function", Function: schema.FunctionCall{Name: call.Name, Arguments: cmpOr(call.Args, `{"value":"x"}`)}})
		}
		return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, Content: step.Text, ToolCalls: calls}}), nil
	}
	return textStream(step.Text), nil
}

func (m *evalModel) checkInput(index int, check *evalInputCheck, input []*schema.Message) {
	var all, system strings.Builder
	for _, message := range input {
		if message == nil {
			continue
		}
		all.WriteString(message.Content)
		all.WriteByte('\n')
		if message.Role == schema.System {
			system.WriteString(message.Content)
		}
	}
	for _, text := range check.Contains {
		if !strings.Contains(all.String(), text) {
			m.failures = append(m.failures, fmt.Sprintf("model call %d input lacks %q", index, text))
		}
	}
	for _, text := range check.Absent {
		if strings.Contains(all.String(), text) {
			m.failures = append(m.failures, fmt.Sprintf("model call %d input unexpectedly contains %q", index, text))
		}
	}
	for _, text := range check.SystemContains {
		if !strings.Contains(system.String(), text) {
			m.failures = append(m.failures, fmt.Sprintf("model call %d system context lacks %q", index, text))
		}
	}
	if check.MaxBytes > 0 && all.Len() > check.MaxBytes {
		m.failures = append(m.failures, fmt.Sprintf("model call %d input is %d bytes, want at most %d", index, all.Len(), check.MaxBytes))
	}
	if check.ClosedHistory {
		pending := map[string]bool{}
		for _, message := range input {
			if message == nil {
				continue
			}
			if message.Role != schema.Tool && len(pending) > 0 {
				m.failures = append(m.failures, fmt.Sprintf("model call %d: message follows unfinished tool calls %v", index, mapsKeys(pending)))
			}
			for _, call := range message.ToolCalls {
				pending[call.ID] = true
			}
			if message.Role == schema.Tool {
				if !pending[message.ToolCallID] {
					m.failures = append(m.failures, fmt.Sprintf("model call %d: orphaned tool result %s", index, message.ToolCallID))
				}
				delete(pending, message.ToolCallID)
			}
		}
		if len(pending) > 0 {
			m.failures = append(m.failures, fmt.Sprintf("model call %d: unfinished tool calls %v", index, mapsKeys(pending)))
		}
	}
}

func (m *evalModel) checkErrors(label string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.failures) > 0 {
		m.t.Fatalf("%s: %s", label, strings.Join(m.failures, "; "))
	}
}

func (m *evalModel) finish() {
	m.checkErrors("end")
	m.mu.Lock()
	defer m.mu.Unlock()
	unused := 0
	for i, step := range m.steps {
		if !step.Optional && !m.used[i] {
			unused++
		}
	}
	if unused > 0 {
		m.t.Errorf("model script has %d unused required steps (served %d)", unused, m.served)
	}
}

func (m *evalModel) counts() (calls, summaries int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.served, m.summaries
}

func cmpOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func mapsKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
