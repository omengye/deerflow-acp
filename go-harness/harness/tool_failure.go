package harness

// ToolFailureError carries a host-selected, sanitized classification. It never
// certifies absence of effects and does not retain private network error text.
type ToolFailureError struct{ Code, Message string }

func (e *ToolFailureError) Error() string { return e.Message }

func ClassifiedToolFailure(code, message string) error {
	return &ToolFailureError{Code: code, Message: message}
}
