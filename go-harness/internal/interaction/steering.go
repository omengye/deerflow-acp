package interaction

import (
	"context"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// SteeringFeed is installed only by the owning foreground execution lease.
// The cursor is persisted in native agent state alongside its messages.
type SteeringFeed struct {
	Read    func(context.Context, int64) ([]harness.SteeringRecord, error)
	Applied func(context.Context, []int64) error
}

type steeringKey struct{}

func WithSteering(ctx context.Context, feed SteeringFeed) context.Context {
	return context.WithValue(ctx, steeringKey{}, feed)
}

func Steering(ctx context.Context) (SteeringFeed, bool) {
	feed, ok := ctx.Value(steeringKey{}).(SteeringFeed)
	return feed, ok
}
