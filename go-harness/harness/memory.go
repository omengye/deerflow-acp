package harness

import "time"

type MemoryScope string

// LegacyImportReport records what an explicit DeerMem JSON migration accepted.
// Summary buckets and the old FTS index are not imported as facts.
type LegacyImportReport struct {
	Read             int            `json:"read"`
	Imported         int            `json:"imported"`
	Existing         int            `json:"existing"`
	Duplicates       int            `json:"duplicates"`
	Rejected         int            `json:"rejected"`
	RejectionReasons map[string]int `json:"rejectionReasons,omitempty"`
}

const (
	MemorySession   MemoryScope = "session"
	MemoryWorkspace MemoryScope = "workspace"
	MemoryUser      MemoryScope = "user"
)

// MemoryCandidate is a descriptive fact entered by the attached operator.
// Memory cannot confer tool permissions or change session configuration.
type MemoryCandidate struct {
	Content    string  `json:"content"`
	Category   string  `json:"category"`
	Confidence float64 `json:"confidence"`
}

type MemorySource struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	ActorID       string `json:"actorId,omitempty"`
	RunID         string `json:"runId,omitempty"`
	InputID       string `json:"inputId,omitempty"`
	EventSequence int64  `json:"eventSequence,omitempty"`
	PolicyVersion string `json:"policyVersion,omitempty"`
}

type MemoryFact struct {
	ID         string       `json:"id"`
	Scope      MemoryScope  `json:"scope"`
	Revision   int64        `json:"revision"`
	Content    string       `json:"content"`
	Category   string       `json:"category"`
	Confidence float64      `json:"confidence"`
	Source     MemorySource `json:"source"`
	CreatedAt  time.Time    `json:"createdAt"`
	UpdatedAt  time.Time    `json:"updatedAt"`
}

type MemoryPage struct {
	Facts         []MemoryFact `json:"facts"`
	ScopeRevision int64        `json:"scopeRevision"`
	Next          string       `json:"next,omitempty"`
}
