package automation

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

// Source is host-authored provenance, never an argument of a model tool.
// Grant pins the local authorization that admitted a persistent execution.
type Source struct {
	Kind         string `json:"kind"`
	Account      string `json:"account,omitempty"`
	Sender       string `json:"sender,omitempty"`
	Workspace    string `json:"workspace"`
	Conversation string `json:"conversation,omitempty"`
	MessageID    string `json:"message_id,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	Grant        string `json:"grant,omitempty"`
}

func (s Source) Principal() string {
	raw, _ := json.Marshal([]string{s.Kind, s.Account, s.Sender, s.Workspace})
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func (s Source) RequestKey(message, operation string) string {
	raw, _ := json.Marshal([]string{s.Principal(), message, operation})
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func (s *Scheduler) Get(ctx context.Context, id string) (Schedule, error) {
	return scanSchedule(s.db.QueryRowContext(ctx, scheduleSelect+` WHERE s.id=?`, id))
}
