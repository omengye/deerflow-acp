package eino

import "context"

// CurrentArtifactInvocation captures host role and attempt identity at the
// actual tool execution boundary. Model arguments and output cannot set it.
func CurrentArtifactInvocation(ctx context.Context) (rootID, invocationID, attemptID string, err error) {
	in, active, err := CurrentRole(ctx)
	if err != nil || !active {
		return "", "", "", err
	}
	work := ctx.Value(roleWorkKey{}).(roleWork)
	return in.RootID, in.ID, work.attempt.ID(), nil
}
