package harness

const (
	ApprovalAsk          = "ask"
	ApprovalAllowAlways  = "allow_always"
	ApprovalRejectAlways = "reject_always"
	ApprovalReadOnly     = "read_only"
)

// ConfigValue describes an allowlisted choice without exposing provider secrets.
type ConfigValue struct {
	Value       string `json:"value"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// ConfigOption is transport-independent metadata for a select control. Values
// only become available when the runtime actually consumes them.
type ConfigOption struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Description  string        `json:"description,omitempty"`
	Category     string        `json:"category,omitempty"`
	Type         string        `json:"type"`
	CurrentValue string        `json:"currentValue"`
	Options      []ConfigValue `json:"options"`
}
