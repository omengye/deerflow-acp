package interaction

import (
	"context"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type userInputKey struct{}

// WithUserInput binds a live client's question capability to its current
// connection. It must not be persisted or borrowed by detached workers.
func WithUserInput(ctx context.Context, handler harness.UserInputHandler) context.Context {
	return context.WithValue(ctx, userInputKey{}, handler)
}

func UserInput(ctx context.Context) (harness.UserInputHandler, bool) {
	handler, ok := ctx.Value(userInputKey{}).(harness.UserInputHandler)
	return handler, ok && handler != nil
}
