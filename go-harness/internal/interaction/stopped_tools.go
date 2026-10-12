package interaction

import "context"

type stoppedToolRecoveryKey struct{}

// WithStoppedToolRecovery marks host-verified cancellation of earlier runs.
// It is guidance for a fresh turn, never permission to replay those runs.
func WithStoppedToolRecovery(ctx context.Context) context.Context {
	return context.WithValue(ctx, stoppedToolRecoveryKey{}, true)
}

func HasStoppedToolRecovery(ctx context.Context) bool {
	marked, _ := ctx.Value(stoppedToolRecoveryKey{}).(bool)
	return marked
}
