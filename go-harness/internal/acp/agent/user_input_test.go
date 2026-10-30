package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

const userInputMethod = "_deerflow/ask_user_question"

func initializeUserInput(t *testing.T, c *client, capability any) {
	t.Helper()
	c.success(t, c.request(t, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{"_meta": map[string]any{"deerflow": map[string]any{"userInput": capability}}}}))
}

func supportedUserInput() map[string]any {
	return map[string]any{"version": 1, "requestMethod": userInputMethod}
}

func userQuestion(req harness.RunRequest) harness.UserInputRequest {
	return harness.UserInputRequest{SessionID: req.Session.ID, RunID: req.RunID, ToolCallID: "question-call", Questions: []harness.UserInputQuestion{{ID: "q1", Header: "Environment", Question: "Which environment should I use?", Options: []harness.UserInputOption{{Label: "Staging", Description: "Test environment"}, {Label: "Production"}}}}}
}

func userQuestionMessage(t *testing.T, c *client) wireMessage {
	t.Helper()
	for {
		message := c.read(t)
		if message.Method == userInputMethod {
			return message
		}
		if message.Method != "session/update" {
			t.Fatalf("expected question form, received %+v", message)
		}
	}
}

func TestACPUserInputRequiresNegotiatedCapability(t *testing.T) {
	for _, tc := range []struct {
		name       string
		capability any
		want       bool
	}{
		{"missing", nil, false},
		{"wrong-version", map[string]any{"version": 2, "requestMethod": userInputMethod}, false},
		{"wrong-method", map[string]any{"version": 1, "requestMethod": "_other/ask_user_question"}, false},
		{"malformed", "unsupported", false},
		{"supported", supportedUserInput(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, engineFunc(func(ctx context.Context, _ harness.RunRequest, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
				handler, supported := interaction.UserInput(ctx)
				if supported != tc.want || (handler != nil) != tc.want {
					t.Errorf("question capability=%v handler=%v want=%v", supported, handler != nil, tc.want)
				}
				return harness.RunResult{StopReason: "end_turn"}, nil
			}))
			c := connect(t, f.service)
			initializeUserInput(t, c, tc.capability)
			sid := c.newSession(t, f.cwd)
			stopReason(t, executionPromptResult(t, c, c.request(t, "session/prompt", promptParams(sid, "inspect"))), "end_turn")
		})
	}
}

func TestACPUserInputAnswerAndDismissalReturnToPrompt(t *testing.T) {
	for _, outcome := range []string{"accepted", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			var resumed atomic.Bool
			f := newFixture(t, engineFunc(func(ctx context.Context, req harness.RunRequest, emit harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
				handler, ok := interaction.UserInput(ctx)
				if !ok {
					return harness.RunResult{}, errors.New("missing question handler")
				}
				response, err := handler(ctx, userQuestion(req))
				if err != nil {
					return harness.RunResult{}, err
				}
				if response.Outcome != outcome || outcome == "accepted" && (len(response.Answers) != 1 || response.Answers[0].QuestionID != "q1" || len(response.Answers[0].Answers) != 1 || response.Answers[0].Answers[0] != "Custom staging") {
					t.Errorf("answer was altered: %+v", response)
				}
				resumed.Store(true)
				return harness.RunResult{StopReason: "end_turn"}, emit(ctx, harness.RunEvent{Kind: "text_delta", Text: "Question completed."})
			}))
			c := connect(t, f.service)
			initializeUserInput(t, c, supportedUserInput())
			sid := c.newSession(t, f.cwd)
			prompt := c.request(t, "session/prompt", promptParams(sid, "inspect"))
			question := userQuestionMessage(t, c)
			var request harness.UserInputRequest
			if err := json.Unmarshal(question.Params, &request); err != nil || request.SessionID != sid || request.RunID == "" || request.ToolCallID != "question-call" || len(request.Questions) != 1 || request.Questions[0].ID != "q1" || request.Questions[0].Header != "Environment" {
				t.Fatalf("question wire contract=%+v err=%v", request, err)
			}
			response := harness.UserInputResponse{Outcome: outcome}
			if outcome == "accepted" {
				response.Answers = []harness.UserInputAnswer{{QuestionID: "q1", Answers: []string{"Custom staging"}}}
			}
			c.send(t, map[string]any{"jsonrpc": "2.0", "id": question.ID, "result": response})
			stopReason(t, executionPromptResult(t, c, prompt), "end_turn")
			if !resumed.Load() {
				t.Fatal("prompt did not receive the question result")
			}
			var approvals int
			if err := f.db.DB().QueryRow(`SELECT count(*) FROM harness_approvals WHERE session_id=?`, sid).Scan(&approvals); err != nil || approvals != 0 {
				t.Fatalf("question became a permission approval: %d %v", approvals, err)
			}
		})
	}
}

func TestACPUserInputRejectsInvalidAndUnsupportedResponses(t *testing.T) {
	for _, mode := range []string{"invalid-answer", "method-not-found", "rpc-error"} {
		t.Run(mode, func(t *testing.T) {
			observed := make(chan error, 1)
			f := newFixture(t, engineFunc(func(ctx context.Context, req harness.RunRequest, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
				handler, _ := interaction.UserInput(ctx)
				_, err := handler(ctx, userQuestion(req))
				observed <- err
				return harness.RunResult{StopReason: "end_turn"}, nil
			}))
			c := connect(t, f.service)
			initializeUserInput(t, c, supportedUserInput())
			sid := c.newSession(t, f.cwd)
			prompt := c.request(t, "session/prompt", promptParams(sid, "inspect"))
			question := userQuestionMessage(t, c)
			response := map[string]any{"jsonrpc": "2.0", "id": question.ID}
			if mode == "invalid-answer" {
				response["result"] = harness.UserInputResponse{Outcome: "accepted", Answers: []harness.UserInputAnswer{{QuestionID: "unknown", Answers: []string{"private-fixture-secret"}}}}
			} else {
				code := protocol.InternalError
				if mode == "method-not-found" {
					code = protocol.MethodNotFound
				}
				response["error"] = &protocol.Error{Code: code, Message: "private-fixture-secret"}
			}
			c.send(t, response)
			stopReason(t, executionPromptResult(t, c, prompt), "end_turn")
			err := <-observed
			var noEffect *harness.ToolNoEffectError
			var notExecuted *harness.ToolNotExecutedError
			if err == nil || strings.Contains(err.Error(), "private-fixture-secret") || !errors.As(err, &noEffect) && !errors.As(err, &notExecuted) {
				t.Fatalf("unsafe question error: %v", err)
			}
		})
	}
}

func TestACPUserInputStopAndDisconnectJoinQuestion(t *testing.T) {
	for _, mode := range []string{"stop", "stop-with-late-answer", "disconnect"} {
		t.Run(mode, func(t *testing.T) {
			observed := make(chan error, 1)
			f := newFixture(t, engineFunc(func(ctx context.Context, req harness.RunRequest, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
				handler, _ := interaction.UserInput(ctx)
				_, err := handler(ctx, userQuestion(req))
				observed <- err
				return harness.RunResult{}, err
			}))
			c := connect(t, f.service)
			initializeUserInput(t, c, supportedUserInput())
			sid := c.newSession(t, f.cwd)
			prompt := c.request(t, "session/prompt", promptParams(sid, "inspect"))
			question := userQuestionMessage(t, c)
			if mode == "disconnect" {
				if err := c.agent.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				cancel, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]string{"sessionId": sid}})
				frames := append(cancel, '\n')
				if mode == "stop-with-late-answer" {
					answer, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": question.ID, "result": harness.UserInputResponse{Outcome: "accepted", Answers: []harness.UserInputAnswer{{QuestionID: "q1", Answers: []string{"Staging"}}}}})
					frames = append(append(frames, answer...), '\n')
				}
				if _, err := c.in.Write(frames); err != nil {
					t.Fatal(err)
				}
				stopReason(t, executionPromptResult(t, c, prompt), "cancelled")
			}
			select {
			case err := <-observed:
				var noEffect *harness.ToolNoEffectError
				if !errors.Is(err, context.Canceled) || !errors.As(err, &noEffect) {
					t.Fatalf("question cancellation was swallowed: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("question did not join after cancellation")
			}
		})
	}
}

func TestACPUserInputSerializesConcurrentQuestionForms(t *testing.T) {
	started := make(chan struct{}, 2)
	f := newFixture(t, engineFunc(func(ctx context.Context, req harness.RunRequest, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		handler, _ := interaction.UserInput(ctx)
		results := make(chan error, 2)
		for _, callID := range []string{"question-a", "question-b"} {
			go func() {
				request := userQuestion(req)
				request.ToolCallID = callID
				started <- struct{}{}
				response, err := handler(ctx, request)
				if err == nil && response.Outcome != "accepted" {
					err = errors.New("question was not accepted")
				}
				results <- err
			}()
		}
		return harness.RunResult{StopReason: "end_turn"}, errors.Join(<-results, <-results)
	}))
	c := connect(t, f.service)
	initializeUserInput(t, c, supportedUserInput())
	sid := c.newSession(t, f.cwd)
	prompt := c.request(t, "session/prompt", promptParams(sid, "inspect"))
	first := userQuestionMessage(t, c)
	for range 2 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent question did not start")
		}
	}
	select {
	case message := <-c.messages:
		t.Fatalf("a second form was sent before the first was answered: %+v", message)
	case <-time.After(50 * time.Millisecond):
	}
	answer := harness.UserInputResponse{Outcome: "accepted", Answers: []harness.UserInputAnswer{{QuestionID: "q1", Answers: []string{"Staging"}}}}
	c.send(t, map[string]any{"jsonrpc": "2.0", "id": first.ID, "result": answer})
	second := userQuestionMessage(t, c)
	if string(first.ID) == string(second.ID) {
		t.Fatal("question forms reused a request identity")
	}
	c.send(t, map[string]any{"jsonrpc": "2.0", "id": second.ID, "result": answer})
	stopReason(t, executionPromptResult(t, c, prompt), "end_turn")
}
