package runtime

import (
	"context"
	"errors"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/memory"
)

func (s *Service) FlushMemory(ctx context.Context, owner, sessionID string) error {
	status, err := s.FlushMemoryWithStatus(ctx, owner, sessionID)
	if err != nil {
		return err
	}
	return status.Err()
}

// FlushMemoryWithStatus fixes the input set while holding the foreground slot,
// then releases it before observing asynchronous work. It never dispatches work.
func (s *Service) FlushMemoryWithStatus(ctx context.Context, owner, sessionID string) (status harness.MemoryFlushStatus, err error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var set memory.FlushSet
	for {
		barrier, _, done, acquireErr := s.memoryScope(ctx, owner, sessionID, harness.MemorySession)
		if acquireErr == nil {
			if s.MemoryAsync {
				set, err = s.Memory.CaptureFlush(barrier, sessionID)
			} else {
				err = s.Memory.Flush(barrier)
			}
			done()
			if err != nil {
				return status, err
			}
			if !s.MemoryAsync {
				status.Flushed = true
				status.Reason = "complete"
				return status, nil
			}
			break
		}
		if ctx.Err() != nil {
			status.Reason = "foreground_timeout"
			if errors.Is(ctx.Err(), context.Canceled) {
				return status, ctx.Err()
			}
			return status, nil
		}
		if !errors.Is(acquireErr, harness.ErrBusy) {
			return status, acquireErr
		}
		if err = waitMemoryPoll(ctx); err != nil {
			status.Reason = "foreground_timeout"
			if errors.Is(err, context.Canceled) {
				return status, err
			}
			return status, nil
		}
	}
	return s.waitMemoryFlush(ctx, set, waitMemoryPoll)
}

func (s *Service) waitMemoryFlush(ctx context.Context, set memory.FlushSet, poll func(context.Context) error) (status harness.MemoryFlushStatus, err error) {
	for {
		observed, e := s.Memory.ObserveFlush(ctx, set)
		if e != nil {
			if ctx.Err() != nil {
				return flushDeadline(ctx, status)
			}
			return observed, e
		}
		status = observed
		if status.Pending == 0 && status.Blocked == 0 && status.Failed == 0 {
			if err = s.Memory.Flush(ctx); err != nil {
				return status, err
			}
			status.Flushed = true
			status.Reason = "complete"
			return status, nil
		}
		if status.Blocked > 0 || status.Failed > 0 {
			return status, nil
		}
		if !s.Memory.ExtractionWorkerRunning() {
			status.WaitReason = status.Reason
			status.Reason = "worker_not_running"
			return status, nil
		}
		if err = poll(ctx); err != nil {
			return flushDeadline(ctx, status)
		}
	}
}

func waitMemoryPoll(ctx context.Context) error {
	timer := time.NewTimer(25 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func flushDeadline(ctx context.Context, status harness.MemoryFlushStatus) (harness.MemoryFlushStatus, error) {
	if errors.Is(ctx.Err(), context.Canceled) {
		status.Reason = "cancelled"
		return status, ctx.Err()
	}
	status.WaitReason = status.Reason
	status.Reason = "timeout"
	return status, nil
}

func (s *Service) ImportDeerMem(ctx context.Context, owner, sessionID string, kind harness.MemoryScope, raw []byte) (harness.LegacyImportReport, error) {
	ctx, scope, done, err := s.memoryScope(ctx, owner, sessionID, kind)
	if err != nil {
		return harness.LegacyImportReport{}, err
	}
	defer done()
	return s.Memory.ImportLegacy(ctx, scope, raw)
}

func (s *Service) memoryScope(ctx context.Context, owner, sessionID string, kind harness.MemoryScope) (context.Context, memory.Scope, func(), error) {
	if s.Memory == nil {
		return nil, memory.Scope{}, nil, harness.ErrInvalidInput
	}
	if err := s.Store.requireForegroundSession(ctx, sessionID); err != nil {
		return nil, memory.Scope{}, nil, err
	}
	ctx, release, err := s.Coordinator.Begin(ctx, sessionID, owner)
	if err != nil {
		return nil, memory.Scope{}, nil, err
	}
	session, err := s.Store.Session(ctx, sessionID)
	if err != nil {
		release()
		return nil, memory.Scope{}, nil, err
	}
	var subjectSession, subjectUser string
	switch kind {
	case harness.MemorySession:
		subjectSession = sessionID
	case harness.MemoryWorkspace:
	case harness.MemoryUser:
		if s.MemoryUserID == "" {
			release()
			return nil, memory.Scope{}, nil, harness.ErrInvalidInput
		}
		subjectUser = s.MemoryUserID
	case harness.MemoryGlobal:
		if s.MemoryGlobalID == "" {
			release()
			return nil, memory.Scope{}, nil, harness.ErrInvalidInput
		}
		subjectUser = s.MemoryGlobalID
	default:
		release()
		return nil, memory.Scope{}, nil, harness.ErrInvalidInput
	}
	workspace := session.CWD
	if kind == harness.MemoryGlobal {
		workspace = ""
	}
	scope, err := memory.NewScope(memory.ScopeKind(kind), workspace, subjectSession, subjectUser, "")
	if err != nil {
		release()
		return nil, memory.Scope{}, nil, err
	}
	return ctx, scope, release, nil
}

func (s *Service) PromoteMemoryFact(ctx context.Context, owner, sessionID string, kind harness.MemoryScope, id string, expectedRevision int64) (harness.MemoryFact, error) {
	ctx, origin, done, err := s.memoryScope(ctx, owner, sessionID, kind)
	if err != nil {
		return harness.MemoryFact{}, err
	}
	defer done()
	global, err := memory.NewScope(memory.GlobalScope, "", "", s.MemoryGlobalID, "")
	if err != nil {
		return harness.MemoryFact{}, err
	}
	fact, err := s.Memory.Promote(ctx, origin, global, id, expectedRevision, memory.Source{ID: NewID(), Kind: "operator", ActorID: owner})
	return memoryFact(fact, harness.MemoryGlobal), memoryConflict(err)
}

func memoryFact(f memory.Fact, kind harness.MemoryScope) harness.MemoryFact {
	return harness.MemoryFact{ID: f.ID, Scope: kind, Revision: f.Revision, Content: f.Content, Category: f.Category, Confidence: f.Confidence, CreatedAt: f.CreatedAt, UpdatedAt: f.UpdatedAt,
		Source: harness.MemorySource{ID: f.Source.ID, Kind: f.Source.Kind, ActorID: f.Source.ActorID, RunID: f.Source.RunID, InputID: f.Source.InputID, EventSequence: f.Source.EventSequence, PolicyVersion: f.Source.PolicyVersion, Provenance: memory.Provenance(f)}}
}

func memoryConflict(err error) error {
	if errors.Is(err, memory.ErrVersionConflict) {
		return errors.Join(harness.ErrExecutionConflict, err)
	}
	return err
}

func (s *Service) MemoryFacts(ctx context.Context, owner, sessionID string, kind harness.MemoryScope, after string, limit int) (harness.MemoryPage, error) {
	ctx, scope, done, err := s.memoryScope(ctx, owner, sessionID, kind)
	if err != nil {
		return harness.MemoryPage{}, err
	}
	defer done()
	page, err := s.Memory.List(ctx, scope, after, limit)
	if err != nil {
		return harness.MemoryPage{}, err
	}
	out := harness.MemoryPage{ScopeRevision: page.ScopeRevision, Next: page.Next}
	for _, f := range page.Facts {
		out.Facts = append(out.Facts, memoryFact(f, kind))
	}
	return out, nil
}

func (s *Service) SearchMemory(ctx context.Context, owner, sessionID string, kind harness.MemoryScope, query string, limit int) ([]harness.MemoryFact, error) {
	ctx, scope, done, err := s.memoryScope(ctx, owner, sessionID, kind)
	if err != nil {
		return nil, err
	}
	defer done()
	facts, err := s.Memory.Search(ctx, scope, query, limit)
	if err != nil {
		return nil, err
	}
	out := make([]harness.MemoryFact, 0, len(facts))
	for _, f := range facts {
		out = append(out, memoryFact(f, kind))
	}
	return out, nil
}

func (s *Service) MemoryFact(ctx context.Context, owner, sessionID string, kind harness.MemoryScope, id string) (harness.MemoryFact, error) {
	ctx, scope, done, err := s.memoryScope(ctx, owner, sessionID, kind)
	if err != nil {
		return harness.MemoryFact{}, err
	}
	defer done()
	f, err := s.Memory.Get(ctx, scope, id)
	return memoryFact(f, kind), err
}

func (s *Service) CreateMemoryFact(ctx context.Context, owner, sessionID string, kind harness.MemoryScope, candidate harness.MemoryCandidate) (harness.MemoryFact, error) {
	ctx, scope, done, err := s.memoryScope(ctx, owner, sessionID, kind)
	if err != nil {
		return harness.MemoryFact{}, err
	}
	defer done()
	f, err := s.Memory.Create(ctx, scope, memory.Candidate{Content: candidate.Content, Category: candidate.Category, Confidence: candidate.Confidence, Source: memory.Source{ID: NewID(), Kind: "operator", ActorID: owner, PolicyVersion: "operator-v1"}})
	return memoryFact(f, kind), err
}

func (s *Service) ReplaceMemoryFact(ctx context.Context, owner, sessionID string, kind harness.MemoryScope, id string, expectedRevision int64, candidate harness.MemoryCandidate) (harness.MemoryFact, error) {
	ctx, scope, done, err := s.memoryScope(ctx, owner, sessionID, kind)
	if err != nil {
		return harness.MemoryFact{}, err
	}
	defer done()
	f, err := s.Memory.Replace(ctx, scope, id, expectedRevision, memory.Candidate{Content: candidate.Content, Category: candidate.Category, Confidence: candidate.Confidence, Source: memory.Source{ID: NewID(), Kind: "operator", ActorID: owner, PolicyVersion: "operator-v1"}})
	return memoryFact(f, kind), memoryConflict(err)
}

func (s *Service) DeleteMemoryFact(ctx context.Context, owner, sessionID string, kind harness.MemoryScope, id string, expectedRevision int64) error {
	ctx, scope, done, err := s.memoryScope(ctx, owner, sessionID, kind)
	if err != nil {
		return err
	}
	defer done()
	return memoryConflict(s.Memory.Delete(ctx, scope, id, expectedRevision, memory.Source{ID: NewID(), Kind: "operator", ActorID: owner, PolicyVersion: "operator-v1"}))
}

func (s *Service) ClearMemory(ctx context.Context, owner, sessionID string, kind harness.MemoryScope, expectedScopeRevision int64) (int, error) {
	ctx, scope, done, err := s.memoryScope(ctx, owner, sessionID, kind)
	if err != nil {
		return 0, err
	}
	defer done()
	count, err := s.Memory.Clear(ctx, scope, expectedScopeRevision, NewID(), owner)
	return count, memoryConflict(err)
}
