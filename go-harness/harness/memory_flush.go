package harness

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// MemoryFlushStatus counts input decisions, not extracted facts. Barrier names
// an immutable set for this call; it is not a resumable cursor or vector barrier.
type MemoryFlushStatus struct {
	Flushed    bool   `json:"flushed"`
	Pending    int    `json:"pending"`
	Failed     int    `json:"failed"`
	Skipped    int    `json:"skipped"`
	Discarded  int    `json:"discarded"`
	Blocked    int    `json:"blocked"`
	Completed  int    `json:"completed"`
	Total      int    `json:"total"`
	Reason     string `json:"reason"`
	WaitReason string `json:"waitReason,omitempty"`
	Barrier    string `json:"barrier,omitempty"`
	NextAt     string `json:"nextAt,omitempty"`
}

var (
	ErrMemoryFlushPending = errors.New("memory flush pending")
	ErrMemoryFlushFailed  = errors.New("memory flush failed")
	ErrMemoryFlushBlocked = errors.New("memory flush blocked")
	ErrMemoryFlushState   = errors.New("memory flush state unavailable or inconsistent")
)

type MemoryFlushError struct{ Status MemoryFlushStatus }

func (e *MemoryFlushError) Error() string {
	return fmt.Sprintf("memory flush incomplete: %s (pending=%d blocked=%d failed=%d)", e.Status.Reason, e.Status.Pending, e.Status.Blocked, e.Status.Failed)
}
func (e *MemoryFlushError) Is(target error) bool {
	return target == ErrMemoryFlushPending && (e.Status.Pending > 0 || e.Status.Reason == "foreground_timeout") || target == ErrMemoryFlushBlocked && e.Status.Blocked > 0 || target == ErrMemoryFlushFailed && e.Status.Failed > 0 || target == context.DeadlineExceeded && (e.Status.Reason == "timeout" || e.Status.Reason == "foreground_timeout")
}
func (s MemoryFlushStatus) Err() error {
	if s.Flushed {
		return nil
	}
	return &MemoryFlushError{Status: s}
}

// Zero fields select defaults: 3 attempts, one minute initial delay, one hour cap.
type MemoryIndexConfig struct {
	MaxAttempts   int
	RetryDelay    time.Duration
	MaxRetryDelay time.Duration
}

func (cfg MemoryIndexConfig) WithDefaults() (MemoryIndexConfig, error) {
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = 3
	}
	if cfg.RetryDelay == 0 {
		cfg.RetryDelay = time.Minute
	}
	if cfg.MaxRetryDelay == 0 {
		cfg.MaxRetryDelay = time.Hour
	}
	if cfg.MaxAttempts < 1 || cfg.MaxAttempts > 20 || cfg.RetryDelay < time.Second || cfg.MaxRetryDelay < cfg.RetryDelay || cfg.MaxRetryDelay > 24*time.Hour {
		return cfg, ErrInvalidInput
	}
	return cfg, nil
}

type MemoryIndexFailure struct {
	FactID    string    `json:"factId"`
	Revision  int64     `json:"revision"`
	Model     string    `json:"model"`
	Attempts  int       `json:"attempts"`
	NextAt    time.Time `json:"nextAt"`
	ErrorCode string    `json:"errorCode"`
	Terminal  bool      `json:"terminal"`
}
