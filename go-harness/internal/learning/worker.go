package learning

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func (s *Store) RunOne(ctx context.Context) (bool, error) {
	if s.maintenance == nil || s.shared {
		return false, nil
	}
	return s.maintenance.RunKinds(ctx, s.learningKinds(), s.handleLearning)
}

func (s *Store) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return invalid("worker is closed")
	}
	if !s.enabled || s.done != nil {
		return nil
	}
	if s.extractor == nil || s.maintenance == nil {
		return invalid("extractor is not configured")
	}
	if s.options.Consolidation != nil && s.consolidator == nil {
		return invalid("consolidator is not configured")
	}
	if s.options.Matching != nil && s.matcher == nil {
		return invalid("matcher is not configured")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel, s.done = cancel, make(chan struct{})
	var workers sync.WaitGroup
	loop := func(work func(context.Context) (bool, error)) {
		defer workers.Done()
		warned := false
		for {
			worked, err := work(ctx)
			if ctx.Err() != nil {
				return
			}
			delay := time.Second
			if err != nil {
				if !warned {
					slog.Warn("skill experience maintenance deferred; user task state is unchanged")
				}
				warned, delay = true, 5*time.Second
			} else {
				warned = false
				if worked {
					delay = 10 * time.Millisecond
				}
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}
	workers.Add(1)
	go loop(s.QueueLearning)
	if !s.shared {
		for range s.options.Budget.Workers {
			workers.Add(1)
			go loop(s.RunOne)
		}
	}
	go func() { workers.Wait(); close(s.done) }()
	return nil
}

func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	s.closed = true
	if s.cancel != nil {
		s.cancel()
	}
	done := s.done
	s.mu.Unlock()
	if done != nil {
		<-done
	}
	return nil
}

func (s *Store) Status(ctx context.Context) (harness.SkillLearningStatus, error) {
	result := harness.SkillLearningStatus{Version: PolicyVersion, Enabled: s.enabled, Mode: "shadow", IdleSeconds: int64(s.options.Idle / time.Second), ConsolidationEnabled: s.enabled && s.consolidator != nil}
	result.MatchingEnabled = s.enabled && s.matcher != nil
	result.CrossSessionEnabled = s.enabled && s.consolidator != nil && s.options.CrossSession
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM harness_learning_outbox WHERE state IN ('pending','queued')`).Scan(&result.Pending)
	err = errors.Join(err, s.db.QueryRowContext(ctx, `SELECT count(*) FROM df_learning_experiences`).Scan(&result.Experiences))
	err = errors.Join(err, s.db.QueryRowContext(ctx, `SELECT count(*) FROM df_learning_patterns`).Scan(&result.Patterns))
	err = errors.Join(err, s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM df_learning_matches WHERE result_sha<>'')
 + (SELECT count(*) FROM df_learning_adjudications)`).Scan(&result.Matches))
	return result, err
}
