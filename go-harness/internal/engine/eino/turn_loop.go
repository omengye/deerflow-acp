package eino

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// turnItem records durable input identity and the immutable execution policy.
// Runtime.BeginRun commits the actual accepted content before Engine.Run.
// An explicit resume never substitutes new prompt text for the interrupted item.
type turnItem struct {
	InputID, SessionID        string
	ConfigVersion             int64
	Model, Mode, ApprovalMode string
	Subagents                 bool
	Workspace, Contract       string
}

func itemFor(req harness.RunRequest) turnItem {
	inputID := req.InputID
	if inputID == "" {
		inputID = req.RunID
	}
	return turnItem{InputID: inputID, SessionID: req.Session.ID, ConfigVersion: req.Session.ConfigVersion, Model: req.Session.Model, Mode: req.Session.Mode, ApprovalMode: req.Session.ApprovalMode, Subagents: req.Session.Subagents, Workspace: req.Session.CWD}
}

func (e *Engine) executionContract(infos []*schema.ToolInfo, extension json.RawMessage) (string, error) {
	data, err := json.Marshal(struct {
		Provider, BaseURL, Model, Instruction string
		MaxIterations                         int
		Budget                                harness.BudgetLimits
		DisableSubagent                       bool
		Tools                                 []*schema.ToolInfo
		Extension                             json.RawMessage
		Media                                 harness.MediaConfig
	}{e.config.Provider, e.config.BaseURL, e.config.Model, e.config.Instruction, e.config.MaxIterations, e.config.Budget, e.config.DisableSubAgent, infos, extension, e.config.Media})
	if err != nil {
		return "", fmt.Errorf("encode execution contract: %w", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

func (e *Engine) turnLoop(ctx context.Context, req harness.RunRequest, resumeID, contract string, extension json.RawMessage, budget *runBudget, saved *checkpointEnvelope, agent adk.Agent, consume func(*adk.AsyncIterator[*adk.AgentEvent]) error) (*adk.TurnLoop[turnItem, *schema.Message], *checkedCheckpoints, error) {
	key := CheckpointID(req.RunID)
	if resumeID != "" {
		key = resumeID
		if saved == nil {
			return nil, nil, fmt.Errorf("%w: checkpoint was not loaded", harness.ErrInvalidInput)
		}
	}
	item := itemFor(req)
	item.Contract = contract
	var input []*schema.Message
	var err error
	if resumeID == "" {
		input, err = e.input(ctx, req)
		if err != nil {
			return nil, nil, err
		}
	}
	checkpoints := &checkedCheckpoints{inner: e.config.CheckpointStore, key: key, budget: budget, extension: append(json.RawMessage(nil), extension...), cached: saved}
	loop := adk.NewTurnLoop(adk.TurnLoopConfig[turnItem, *schema.Message]{
		Store: checkpoints, CheckpointID: key, SessionID: req.Session.ID, SessionStore: e.config.SessionStore,
		GenInput: func(_ context.Context, _ *adk.TurnLoop[turnItem, *schema.Message], items []turnItem) (*adk.GenInputResult[turnItem, *schema.Message], error) {
			if resumeID != "" {
				return nil, fmt.Errorf("%w: checkpoint has no suspended execution", harness.ErrInvalidInput)
			}
			if len(items) != 1 || items[0] != item {
				return nil, fmt.Errorf("%w: unexpected persisted input identity", harness.ErrInvalidInput)
			}
			checkpoints.allowWrites()
			return &adk.GenInputResult[turnItem, *schema.Message]{Input: &adk.AgentInput{Messages: input, EnableStreaming: true}, Consumed: items}, nil
		},
		GenResume: func(_ context.Context, _ *adk.TurnLoop[turnItem, *schema.Message], interrupted, unhandled, newItems []turnItem) (*adk.GenResumeResult[turnItem, *schema.Message], error) {
			if resumeID == "" {
				return nil, fmt.Errorf("%w: explicit checkpoint resume is required", harness.ErrInvalidInput)
			}
			if len(interrupted) != 1 || len(unhandled) != 0 || len(newItems) != 1 || newItems[0] != item {
				return nil, fmt.Errorf("%w: unexpected checkpoint input identity", harness.ErrInvalidInput)
			}
			previous := interrupted[0]
			previous.InputID = item.InputID
			if previous != item {
				return nil, fmt.Errorf("%w: checkpoint session or policy changed", harness.ErrInvalidInput)
			}
			checkpoints.allowWrites()
			return &adk.GenResumeResult[turnItem, *schema.Message]{Consumed: interrupted, Decision: adk.TurnLoopResumeDecisionResume}, nil
		},
		PrepareAgent: func(context.Context, *adk.TurnLoop[turnItem, *schema.Message], []turnItem) (adk.Agent, error) {
			return agent, nil
		},
		OnAgentEvents: func(_ context.Context, tc *adk.TurnContext[turnItem, *schema.Message], events *adk.AsyncIterator[*adk.AgentEvent]) error {
			err := consume(events)
			// One accepted foreground input owns this invocation. Background
			// notifications will be admitted explicitly by the runtime scheduler.
			tc.Loop.Stop()
			return err
		},
	})
	if accepted, _ := loop.Push(item); !accepted {
		return nil, nil, errors.New("turn loop refused accepted input")
	}
	return loop, checkpoints, nil
}

// The harness envelope leaves Eino's native encoding opaque and pins the
// execution budget and extension state alongside it. Older development formats
// are rejected rather than restoring execution with reset counters or policies.
type checkpointEnvelope struct {
	Version   int             `json:"version"`
	Native    []byte          `json:"native"`
	Budget    *budgetSnapshot `json:"budget"`
	Extension json.RawMessage `json:"extension,omitempty"`
}

func decodeCheckpointEnvelope(data []byte) (*checkpointEnvelope, error) {
	var saved checkpointEnvelope
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil, fmt.Errorf("%w: decode harness checkpoint: %v", harness.ErrInvalidInput, err)
	}
	if saved.Version != 1 || len(saved.Native) == 0 || saved.Budget == nil {
		return nil, fmt.Errorf("%w: unsupported or empty harness checkpoint", harness.ErrInvalidInput)
	}
	return &saved, nil
}

func loadCheckpointEnvelope(ctx context.Context, store adk.CheckPointStore, key string) (*checkpointEnvelope, error) {
	if !strings.HasPrefix(key, "harness/turn/v1/") {
		return nil, fmt.Errorf("%w: unsupported checkpoint format", harness.ErrInvalidInput)
	}
	data, found, err := store.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	if !found || len(data) == 0 {
		return nil, harness.ErrNotFound
	}
	return decodeCheckpointEnvelope(data)
}

func readCheckpointExtension(ctx context.Context, store adk.CheckPointStore, key string) (json.RawMessage, error) {
	saved, err := loadCheckpointEnvelope(ctx, store, key)
	if err != nil {
		return nil, err
	}
	return append(json.RawMessage(nil), saved.Extension...), nil
}

// Native TurnLoop may Delete after a rejected/corrupt resume, and its iterator
// can end before provider I/O and resource cleanup. Buffer all mutations until
// the complete harness invocation has succeeded. Failed invocations leave the
// original bytes untouched, including failures after native resume validation.
// Consequently, attempts that fail after spending resources do not advance this
// checkpoint's budget; charging retries requires a separate durable ledger.
type checkedCheckpoints struct {
	inner        adk.CheckPointStore
	mu           sync.Mutex
	err          error
	writeAllowed bool
	key          string
	budget       *runBudget
	extension    json.RawMessage
	cached       *checkpointEnvelope
	pending      bool
	delete       bool
	data         []byte
}

func (s *checkedCheckpoints) allowWrites() { s.mu.Lock(); s.writeAllowed = true; s.mu.Unlock() }
func (s *checkedCheckpoints) record(err error) {
	if err != nil {
		s.mu.Lock()
		s.err = errors.Join(s.err, err)
		s.mu.Unlock()
	}
}
func (s *checkedCheckpoints) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if key == s.key && s.cached != nil {
		return append([]byte(nil), s.cached.Native...), true, nil
	}
	data, ok, err := s.inner.Get(ctx, key)
	if err == nil && ok && len(data) > 0 {
		var saved *checkpointEnvelope
		saved, err = decodeCheckpointEnvelope(data)
		if err == nil {
			data = saved.Native
		}
	}
	s.record(err)
	return data, ok, err
}
func (s *checkedCheckpoints) Set(_ context.Context, key string, data []byte) error {
	return s.stage(key, data, false)
}
func (s *checkedCheckpoints) Delete(_ context.Context, key string) error {
	return s.stage(key, nil, true)
}
func (s *checkedCheckpoints) stage(key string, data []byte, remove bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.writeAllowed {
		return nil
	}
	if key != s.key {
		err := fmt.Errorf("unexpected checkpoint mutation: %q", key)
		s.err = errors.Join(s.err, err)
		return err
	}
	s.pending, s.delete, s.data = true, remove, append([]byte(nil), data...)
	return nil
}
func (s *checkedCheckpoints) commit(ctx context.Context, deleteAllowed bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if !s.pending {
		return nil
	}
	var err error
	if s.delete {
		// A provider can return context.Canceled without native resumable state.
		// Cancellation never consumes the previous checkpoint just because alpha
		// cleanup requested its deletion; only a completed turn can do so.
		if !deleteAllowed {
			return nil
		}
		if d, ok := s.inner.(adk.CheckPointDeleter); ok {
			err = d.Delete(ctx, s.key)
		} else {
			// Eino's Get/Set-only store contract uses empty data as no state.
			err = s.inner.Set(ctx, s.key, nil)
		}
	} else {
		var snapshot budgetSnapshot
		snapshot, err = s.budget.snapshot()
		if err == nil {
			var data []byte
			data, err = json.Marshal(checkpointEnvelope{Version: 1, Native: s.data, Budget: &snapshot, Extension: s.extension})
			if err == nil {
				err = s.inner.Set(ctx, s.key, data)
			}
		}
	}
	if err != nil {
		s.err = errors.Join(s.err, err)
		return err
	}
	s.pending = false
	return nil
}
func (s *checkedCheckpoints) failure() error { s.mu.Lock(); defer s.mu.Unlock(); return s.err }
