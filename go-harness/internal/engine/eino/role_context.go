package eino

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/adk"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
)

type roleContextMiddleware struct {
	adk.BaseChatModelAgentMiddleware
	request harness.RunRequest
}

func (m *roleContextMiddleware) AfterAgent(ctx context.Context, state *adk.ChatModelAgentState) (context.Context, error) {
	if state != nil {
		observeRoleTreeResult(ctx, state.Messages)
	}
	hooks := interaction.FromContext(ctx)
	if hooks == nil {
		return ctx, nil
	}
	broker, ok := hooks.Broker.(interaction.RoleContextBroker)
	if !ok {
		return ctx, nil
	}
	invocation, active, err := CurrentRole(ctx)
	if err != nil {
		return ctx, err
	}
	if !active || state == nil || len(state.Messages) == 0 {
		return ctx, fmt.Errorf("%w: missing completed role context", harness.ErrExecutionConflict)
	}
	data, err := json.Marshal(state.Messages)
	if err != nil {
		return ctx, err
	}
	if err = broker.StageRoleContext(ctx, interaction.RoleContext{Invocation: invocation, SessionID: m.request.Session.ID, RunID: m.request.RunID, Messages: data}); err != nil {
		return ctx, err
	}
	return ctx, nil
}
