package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// SessionResources owns client-supplied processes/connections for one attached
// session. Bind runs under its lifecycle lease. Implementations must copy input,
// preserve the prior binding on failure, and never publish credentials.
type SessionResources interface {
	Bind(context.Context, string, string, string, []harness.MCPServer) error
	Release(context.Context, string, string) error
	ReleaseOwner(context.Context, string) error
}

func (s *Service) MCPCapabilities() (http, sse bool) {
	if caps, ok := s.Resources.(interface{ MCPCapabilities() (bool, bool) }); ok {
		return caps.MCPCapabilities()
	}
	return false, false
}

func (s *Service) releaseResourceOwner(owner string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Resources.ReleaseOwner(ctx, owner); err != nil {
		return err
	}
	// A session close may have failed while owner-wide retirement subsequently
	// succeeded. Retry only idempotent resource cleanup to release its lease.
	return s.Coordinator.DisconnectWithCleanup(ctx, owner, func(ctx context.Context, id string) error { return s.Resources.Release(ctx, owner, id) })
}

func (s *Service) bindResources(ctx context.Context, owner string, x harness.Session, servers []harness.MCPServer) error {
	if s.Resources == nil {
		if len(servers) > 0 {
			return fmt.Errorf("%w: client MCP is not configured", harness.ErrInvalidInput)
		}
		return nil
	}
	return s.Resources.Bind(ctx, owner, x.ID, x.CWD, servers)
}

func (s *Service) CloseSession(ctx context.Context, owner, id string) error {
	return s.Coordinator.DetachWithCleanup(ctx, id, owner, func(cleanup context.Context) error {
		s.clearDecisions(owner, id)
		if s.Resources != nil {
			return s.Resources.Release(cleanup, owner, id)
		}
		return nil
	})
}

// A failed fresh binding must become closing before its lifecycle lease is
// released. Otherwise another prompt can enter between release and detach.
func (s *Service) abandonBinding(owner, id string, release func(), discardNew ...bool) error {
	cleanupResources := func(ctx context.Context) error {
		s.clearDecisions(owner, id)
		if s.Resources != nil {
			if err := s.Resources.Release(ctx, owner, id); err != nil {
				return err
			}
		}
		if len(discardNew) > 0 && discardNew[0] {
			return s.Store.discardUnstartedSession(ctx, id)
		}
		return nil
	}
	stop, cancel := context.WithCancel(context.Background())
	cancel()
	_ = s.Coordinator.DetachWithCleanup(stop, id, owner, cleanupResources)
	release()
	cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
	defer done()
	err := s.Coordinator.DetachWithCleanup(cleanup, id, owner, cleanupResources)
	if errors.Is(err, harness.ErrNotAttached) {
		return nil
	}
	return err
}

// Before a session has a lifecycle lease, cleanup is independent of the failed
// request context. Resource binding failures use abandonBinding instead.
func (s *Service) discardNewSession(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return s.Store.discardUnstartedSession(ctx, id)
}

// Failed session/new setup must not leave a phantom session in session/list.
// Run/event checks preserve any unexpected state instead of deleting user work.
func (s *Store) discardUnstartedSession(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var used bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_runs WHERE session_id=?) OR EXISTS(SELECT 1 FROM harness_events WHERE session_id=?)`, id, id).Scan(&used); err != nil {
		return err
	}
	if used {
		return harness.ErrBusy
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM harness_session_configs WHERE session_id=?`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM harness_sessions WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}
