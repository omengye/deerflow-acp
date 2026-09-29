package runtime

import (
	"context"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type taskActorKey struct{}

// TaskActorFromContext returns the host identity attached by RunService after
// admission. The model's tool arguments never supply owner/session authority.
func TaskActorFromContext(ctx context.Context) (harness.TaskActor, bool) {
	actor, ok := ctx.Value(taskActorKey{}).(harness.TaskActor)
	return actor, ok && actor.OwnerID != "" && actor.SessionID != ""
}

func (s *Service) AuthorizeTaskAccess(ctx context.Context, actor harness.TaskActor) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.Coordinator.Authorize(actor.SessionID, actor.OwnerID)
}
