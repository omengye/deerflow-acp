package channels

// UserError contains only host-authored diagnostics safe for local management.
// Provider responses and database/OS errors must not be wrapped in this type.
type UserError struct{ Message string }

func (e *UserError) Error() string   { return e.Message }
func userError(message string) error { return &UserError{Message: message} }
