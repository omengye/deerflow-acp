package eino

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstream"
)

const progressStateKey = "deerflow_progress_v1"

type progressState struct {
	Last     string   `json:"last"`
	Recent   []string `json:"recent"`
	Failures []bool   `json:"failures,omitempty"`
	Warning  string   `json:"warning"`
	Warned   bool     `json:"warned"`
}

type noProgressError struct{}

func (*noProgressError) Unwrap() error { return harness.ErrNoProgress }

func (*noProgressError) Error() string {
	return "Agent stopped without completing the task: repeated tool calls produced identical observations despite a change-strategy warning. Inspect the results and send a new instruction to continue; no side effects were automatically retried."
}

type progressGuard struct {
	adk.BaseChatModelAgentMiddleware
}

func loadProgress(ctx context.Context) (progressState, error) {
	var s progressState
	v, ok, err := adk.GetRunLocalValue(ctx, progressStateKey)
	if err != nil || !ok {
		return s, err
	}
	raw, ok := v.(string)
	if !ok {
		return s, errors.New("invalid progress checkpoint")
	}
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return s, err
	}
	if len(s.Recent) > 20 {
		return s, errors.New("oversized progress checkpoint")
	}
	return s, nil
}

func (*progressGuard) BeforeModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, _ *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	s, err := loadProgress(ctx)
	if err != nil {
		return ctx, state, err
	}
	advanceProgress(&s, state.Messages)
	stop := shouldStopWithoutProgress(s)
	s.Warned = s.Warning != ""
	encoded, err := json.Marshal(s)
	if err == nil {
		err = adk.SetRunLocalValue(ctx, progressStateKey, string(encoded))
	}
	if err != nil {
		return ctx, state, err
	}
	if stop {
		return ctx, state, &noProgressError{}
	}
	return ctx, state, nil
}

func advanceProgress(s *progressState, messages []*schema.Message) {
	start := 0
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m != nil && m.Role == schema.Tool && m.ToolCallID == s.Last && s.Last != "" {
			start = i + 1
			break
		}
		if m != nil && m.Role == schema.User && !isOutputContinuation(m) && !isGoalContinuation(m) {
			start = i + 1
			break
		}
	}
	args := make(map[string]string)
	for _, m := range messages {
		if m == nil {
			continue
		}
		for _, call := range m.ToolCalls {
			var v any
			decoder := json.NewDecoder(strings.NewReader(call.Function.Arguments))
			decoder.UseNumber()
			if decoder.Decode(&v) == nil {
				b, _ := json.Marshal(v)
				args[call.ID] = string(b)
			}
		}
	}
	for _, m := range messages[start:] {
		if m == nil || m.Role != schema.Tool || m.ToolCallID == "" {
			continue
		}
		s.Last = m.ToolCallID
		if m.ToolName == "task_wait" || m.ToolName == "task_status" {
			s.Recent, s.Failures, s.Warning, s.Warned = nil, nil, "", false
			continue
		}
		// Successful mutations/commands may change state despite returning "OK".
		failed := failedToolObservation(m.Content)
		eligible := failed
		switch m.ToolName {
		case "read_file", "search_files", "web_search", "web_fetch", "glob", "grep", "ls", "list_directory", "history_search":
			eligible = true
		}
		arguments, known := args[m.ToolCallID]
		if !eligible || !known {
			s.Recent = nil
			s.Failures = nil
			s.Warning = ""
			s.Warned = false
			continue
		}
		b, _ := json.Marshal([]string{m.ToolName, arguments, stableObservation(m.Content)})
		s.Recent = append(s.Recent, fmt.Sprintf("%x", sha256.Sum256(b)))
		s.Failures = append(s.Failures, failed)
		if len(s.Recent) > 20 {
			s.Recent = s.Recent[len(s.Recent)-20:]
		}
		if len(s.Failures) > 20 {
			s.Failures = s.Failures[len(s.Failures)-20:]
		}
		if repeatedCycles(s.Recent) >= 3 {
			s.Warning = "Progress warning: the same tool calls and observations are repeating without new evidence. Assess whether another attempt is useful, change the approach, ask for necessary information, or clearly explain the blocker. Inspect possible partial effects before repeating a mutation."
			s.Warning += " Continued identical observations will stop this run as incomplete."
		} else {
			s.Warning = ""
			s.Warned = false
		}
	}
}

// Identical successful observations stop after five cycles. Identical failures
// get more room for deliberate recovery (for example waiting for a service),
// but an unchanged failing call can no longer loop without bound.
const (
	noProgressStopCycles       = 5
	failedNoProgressStopCycles = 8
)

func shouldStopWithoutProgress(s progressState) bool {
	if !s.Warned {
		return false
	}
	cycles := repeatedCycles(s.Recent)
	if hasRecoveryObservations(s) {
		return cycles >= failedNoProgressStopCycles
	}
	return cycles >= noProgressStopCycles
}

// failedToolObservation recognizes host-written failure text, including a
// failure whose oversized text was replaced by a snapshot marker and preview.
// Tool content that merely mentions the phrase is not a failure.
func failedToolObservation(content string) bool {
	if strings.HasPrefix(content, "[Tool output:") {
		_, content, _ = strings.Cut(content, "\n")
	}
	return strings.HasPrefix(content, "Tool failed:") || strings.HasPrefix(content, "Tool permission denied;")
}

// Snapshot references are random per stored output. Remove them so repeated
// identical oversized observations still compare equal; the byte count and
// preview remain part of the observation.
var toolOutputSnapshotID = regexp.MustCompile(`session snapshot "[^"]*"|id="[^"]*"`)

func stableObservation(content string) string {
	if !strings.HasPrefix(content, "[Tool output:") {
		return content
	}
	line, rest, _ := strings.Cut(content, "\n")
	return toolOutputSnapshotID.ReplaceAllString(line, "") + "\n" + rest
}

func hasRecoveryObservations(s progressState) bool {
	for _, failed := range s.Failures {
		if failed {
			return true
		}
	}
	return false
}

func repeatedCycles(values []string) int {
	best := 0
	for width := 1; width <= 4 && width <= len(values); width++ {
		count := 1
		for end := len(values) - width; end >= width; end -= width {
			equal := true
			for j := 0; j < width; j++ {
				if values[end-width+j] != values[len(values)-width+j] {
					equal = false
					break
				}
			}
			if !equal {
				break
			}
			count++
		}
		if count > best {
			best = count
		}
	}
	return best
}

func (*progressGuard) WrapModel(_ context.Context, inner model.BaseModel[*schema.Message], _ *adk.ModelContext) (model.BaseModel[*schema.Message], error) {
	return &progressModel{inner: inner, nativeEndpoint: true}, nil
}

type progressModel struct {
	inner          model.BaseModel[*schema.Message]
	nativeEndpoint bool
}

func progressInput(ctx context.Context, input []*schema.Message) ([]*schema.Message, error) {
	s, err := loadProgress(ctx)
	if err != nil || s.Warning == "" {
		return input, err
	}
	// Request-only hint: never replace the real active user message in history.
	return append(append([]*schema.Message(nil), input...), schema.UserMessage(s.Warning)), nil
}

func (m *progressModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	if m.nativeEndpoint {
		// Eino's synchronous endpoint has no Stream method implementation. The
		// terminal outputTokenModel still enforces streaming provider transport.
		input, err := progressInput(ctx, input)
		if err != nil {
			return nil, err
		}
		return m.inner.Generate(ctx, input, opts...)
	}
	return modelstream.Generate(ctx, m, input, opts...)
}
func (m *progressModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	input, err := progressInput(ctx, input)
	if err != nil {
		return nil, err
	}
	return m.inner.Stream(ctx, input, opts...)
}
