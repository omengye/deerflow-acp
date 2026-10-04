package deerflow

import (
	"context"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	einoengine "github.com/omengye/deerflow-acp/go-harness/internal/engine/eino"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

func skillReadObserver(req harness.RunRequest) skills.ReadObserver {
	return func(ctx context.Context, read skills.ReadObservation) error {
		hooks := interaction.FromContext(ctx)
		if hooks == nil {
			return nil
		}
		broker, ok := hooks.Broker.(interaction.SkillReadBroker)
		if !ok {
			return nil
		}
		role, active, err := einoengine.CurrentRole(ctx)
		if err != nil {
			return err
		}
		evidence := interaction.SkillRead{SessionID: req.Session.ID, RunID: req.RunID, Read: read}
		if active {
			evidence.Role = &role
		}
		return broker.RecordSkillRead(ctx, evidence)
	}
}
