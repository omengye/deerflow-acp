package interaction

import (
	"context"

	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

// SkillRead is issued by the pinned skill backend after successful validation.
// It is not a model argument, feedback assessment or publication authority.
type SkillRead struct {
	SessionID string
	RunID     string
	Role      *agents.Invocation
	Read      skills.ReadObservation
}

type SkillReadBroker interface {
	RecordSkillRead(context.Context, SkillRead) error
}
