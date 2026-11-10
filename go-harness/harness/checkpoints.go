package harness

import (
	"errors"
	"fmt"
)

var (
	ErrCheckpointTooLarge = errors.New("execution checkpoint exceeds its size limit")
	ErrCheckpointInvalid  = errors.New("execution checkpoint is invalid")
	ErrCheckpointCommit   = errors.New("execution checkpoint could not be committed")
)

// CheckpointSizeError describes a host-side limit, never a client parameter error.
type CheckpointSizeError struct {
	Representation string
	Bytes          uint64
	Limit          uint64
}

func (e *CheckpointSizeError) Error() string {
	return fmt.Sprintf("%s: %s bytes=%d limit=%d", ErrCheckpointTooLarge, e.Representation, e.Bytes, e.Limit)
}

func (e *CheckpointSizeError) Is(target error) bool { return target == ErrCheckpointTooLarge }

// CheckpointCommitError retains the original cause for diagnostics and recovery.
// A failed durable write must not become an ordinary recoverable tool failure.
type CheckpointCommitError struct{ Cause error }

func (e *CheckpointCommitError) Error() string {
	return fmt.Sprintf("%s: %v", ErrCheckpointCommit, e.Cause)
}
func (e *CheckpointCommitError) Unwrap() error            { return e.Cause }
func (e *CheckpointCommitError) Is(target error) bool     { return target == ErrCheckpointCommit }
func (e *CheckpointCommitError) PersistenceFailure() bool { return true }
