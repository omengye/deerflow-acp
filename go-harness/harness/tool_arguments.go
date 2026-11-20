package harness

// ToolArgumentError is an argument preparation failure whose message is safe
// to show the model and to persist. Its message must never echo argument
// values, because rejected arguments may carry secrets.
type ToolArgumentError struct {
	Code    string
	Message string
}

func (e *ToolArgumentError) Error() string { return e.Message }

func (e *ToolArgumentError) Is(target error) bool { return target == ErrInvalidInput }
