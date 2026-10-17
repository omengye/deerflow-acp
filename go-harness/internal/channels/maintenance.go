package channels

import (
	"context"
	"time"
)

// Update verifies a new trusted executable before stopping the instance. The
// previous manifest remains persisted for an explicit rollback.
func (m *Manager) Update(ctx context.Context, id string, revision int64, next Manifest, rollback bool) (any, error) {
	i, err := m.get(id)
	if err != nil {
		return nil, err
	}
	i.op.Lock()
	defer i.op.Unlock()
	if !m.available() {
		return nil, userError("channels stopped")
	}
	i.mu.Lock()
	if revision != i.revision {
		i.mu.Unlock()
		return nil, userError("channel changed; refresh before update")
	}
	previous := i.manifest
	if rollback {
		if previous.Previous == nil {
			i.mu.Unlock()
			return nil, userError("no previous plugin version")
		}
		next = *previous.Previous
	}
	i.mu.Unlock()
	if next.ID != previous.ID {
		return nil, userError("plugin identity cannot change")
	}
	next.Previous = nil
	probe, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	p, d, err := m.start(probe, next)
	if err != nil {
		return nil, err
	}
	p.Close()
	next.Descriptor = d
	old := previous
	old.Previous = nil
	next.Previous = &old
	m.stopWorker(i)
	i.mu.Lock()
	defer i.mu.Unlock()
	i.manifest = next
	i.revision++
	i.login = ""
	i.pairCode = ""
	if err = m.persist(ctx, i); err != nil {
		i.manifest = previous
		i.revision--
		m.startWorkerLocked(i)
		return nil, err
	}
	i.lastError = ""
	m.startWorkerLocked(i)
	return map[string]any{"updated": true, "revision": i.revision, "descriptor": d}, nil
}
