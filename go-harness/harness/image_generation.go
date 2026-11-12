package harness

import "fmt"

// ImageGenerationError reports an attempted provider image tool failure. It
// does not change configured capabilities or authorize replay. Cause is for
// diagnostics; transports must use Kind to present safe recovery guidance.
type ImageGenerationError struct {
	Kind  string // failed, unsupported, unsupported_complexity, access_denied, quota_exceeded
	Cause error
}

func (e *ImageGenerationError) Error() string {
	if e.Cause == nil {
		return "provider image generation failed"
	}
	return fmt.Sprintf("provider image generation failed: %v", e.Cause)
}

func (e *ImageGenerationError) Unwrap() error { return e.Cause }
