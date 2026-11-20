package agent

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
)

const askUserQuestionMethod = "_deerflow/ask_user_question"

func (a *Agent) supportsUserInput() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.initialized {
		return false
	}
	deerflow, ok := a.capabilities.Meta["deerflow"].(map[string]any)
	if !ok {
		return false
	}
	data, err := json.Marshal(deerflow["userInput"])
	if err != nil {
		return false
	}
	var capability struct {
		Version       int    `json:"version"`
		RequestMethod string `json:"requestMethod"`
	}
	return json.Unmarshal(data, &capability) == nil && capability.Version == 1 && capability.RequestMethod == askUserQuestionMethod
}

func (a *Agent) remoteCredentialVersion() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	df, _ := a.capabilities.Meta["deerflow"].(map[string]any)
	version, _ := df["mobileCredentials"].(float64)
	if a.initialized && (version == 1 || version == 2) {
		return int(version)
	}
	return 0
}

func (a *Agent) localCredentialForm() bool {
	if runtime.GOOS != "windows" || a.remoteCredentialVersion() != 0 || !a.supportsUserInput() {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	df, _ := a.capabilities.Meta["deerflow"].(map[string]any)
	local, _ := df["desktopCredentials"].(bool)
	return local
}

func (a *Agent) userInputContext(ctx context.Context) context.Context {
	if a.localCredentialForm() {
		ctx = secrets.WithCredentialPrompt(ctx, secrets.DesktopCredentialPrompt)
	} else {
		ctx = secrets.WithRemoteCredentialClient(ctx)
	}
	if a.supportsUserInput() {
		return interaction.WithUserInput(ctx, a.askUserQuestion)
	}
	return ctx
}

// Questions are connection-owned interactions, independent of tool approvals.
// A live connection can show only one question form at a time; callers waiting
// for that slot remain cancellable and never retain it after disconnect.
func (a *Agent) askUserQuestion(ctx context.Context, request harness.UserInputRequest) (harness.UserInputResponse, error) {
	var response harness.UserInputResponse
	if !a.supportsUserInput() {
		return response, harness.MarkToolNotExecuted(errors.New("This client does not support question forms. Ask the user in an ordinary assistant message and wait for the next reply."))
	}
	if err := harness.ValidateUserInputRequest(request); err != nil {
		return response, harness.MarkToolNotExecuted(errors.New("The user question request is invalid."))
	}
	if err := a.service.Coordinator.Authorize(request.SessionID, a.owner); err != nil {
		return response, harness.MarkToolNotExecuted(errors.New("The user question session is not attached to this connection."))
	}
	a.mu.Lock()
	if a.userInputSlot == nil {
		a.userInputSlot = make(chan struct{}, 1)
	}
	slot := a.userInputSlot
	a.mu.Unlock()
	select {
	case <-ctx.Done():
		return response, harness.MarkToolNoEffect(ctx.Err())
	case <-a.peer.Done():
		return response, harness.MarkToolNoEffect(context.Canceled)
	case slot <- struct{}{}:
	}
	defer func() { <-slot }()
	if err := a.peer.Call(ctx, askUserQuestionMethod, request, &response); err != nil {
		failure := a.connectionError(ctx, err)
		if errors.Is(failure, context.Canceled) || errors.Is(failure, context.DeadlineExceeded) {
			return harness.UserInputResponse{}, harness.MarkToolNoEffect(failure)
		}
		var rpc *protocol.Error
		if errors.As(err, &rpc) && rpc.Code == protocol.MethodNotFound {
			return harness.UserInputResponse{}, harness.MarkToolNotExecuted(errors.New("This client does not support question forms. Ask the user in an ordinary assistant message and wait for the next reply."))
		}
		return harness.UserInputResponse{}, harness.MarkToolNoEffect(errors.New("The client could not complete the user question. Ask in an ordinary assistant message if the information is still needed."))
	}
	// A Stop notification precedes the desktop's pending-question response.
	// Join it before accepting an answer or allowing the model to continue.
	if err := a.peer.DrainNotifications(ctx); err != nil {
		return harness.UserInputResponse{}, harness.MarkToolNoEffect(a.connectionError(ctx, err))
	}
	if err := ctx.Err(); err != nil {
		return harness.UserInputResponse{}, harness.MarkToolNoEffect(err)
	}
	if err := harness.ValidateUserInputResponse(request, response); err != nil {
		return harness.UserInputResponse{}, harness.MarkToolNoEffect(errors.New("The client returned an invalid user answer."))
	}
	return response, nil
}
