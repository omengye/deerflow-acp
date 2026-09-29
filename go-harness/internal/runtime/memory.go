package runtime

import (
	"context"
	"errors"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/memory"
)

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
	default:
		release()
		return nil, memory.Scope{}, nil, harness.ErrInvalidInput
	}
	scope, err := memory.NewScope(memory.ScopeKind(kind), session.CWD, subjectSession, subjectUser, "")
	if err != nil {
		release()
		return nil, memory.Scope{}, nil, err
	}
	return ctx, scope, release, nil
}

func memoryFact(f memory.Fact, kind harness.MemoryScope) harness.MemoryFact {
	return harness.MemoryFact{ID: f.ID, Scope: kind, Revision: f.Revision, Content: f.Content, Category: f.Category, Confidence: f.Confidence, CreatedAt: f.CreatedAt, UpdatedAt: f.UpdatedAt,
		Source: harness.MemorySource{ID: f.Source.ID, Kind: f.Source.Kind, ActorID: f.Source.ActorID, RunID: f.Source.RunID, InputID: f.Source.InputID, EventSequence: f.Source.EventSequence, PolicyVersion: f.Source.PolicyVersion}}
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
