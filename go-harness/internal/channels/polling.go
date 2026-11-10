package channels

import (
	"context"
	"errors"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/channelproto"
)

type channelPoller struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// Only plugins advertising independent_poll may use a separate process. Each
// process still speaks serial protocol v1; no long poll holds the send pipe.
// The poller commits one batch before fetching the next, independently of queued
// model work and outbound retries. It never dispatches work or sends messages.
func (m *Manager) startPollerLocked(ctx context.Context, i *instance) {
	if i.poller != nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	poller := &channelPoller{cancel: cancel, done: make(chan struct{})}
	i.poller = poller
	manifest := i.manifest
	go func() {
		defer close(poller.done)
		defer cancel()
		var p transport
		defer func() {
			if p != nil {
				p.Close()
			}
		}()
		for ctx.Err() == nil {
			i.mu.Lock()
			auth, cfg, cursor := i.auth, i.cfg, i.cursor
			i.mu.Unlock()
			if !cfg.Enabled || len(auth.Secret) == 0 {
				return
			}
			var err error
			if p == nil {
				var descriptor channelproto.Descriptor
				p, descriptor, err = m.start(ctx, manifest)
				if err == nil && !descriptor.IndependentPoll {
					err = userError("channel no longer supports independent polling")
				}
			}
			var batch channelproto.Batch
			if err == nil {
				err = p.Call(ctx, "poll", channelproto.Poll{Auth: auth, Cursor: cursor, Options: cfg.Options}, &batch)
			}
			if err == nil && (len(batch.Messages) > 500 || len(batch.Cursor) > 65536) {
				err = userError("plugin batch exceeds bounds")
			}
			if err == nil {
				err = m.accept(ctx, i, auth, cfg, batch)
			}
			if ctx.Err() != nil {
				return
			}
			delay := time.Second
			if err != nil {
				if p != nil {
					p.Close()
					p = nil
				}
				m.workerError(i, err)
				delay = 10 * time.Second
			} else {
				i.mu.Lock()
				if i.auth.Account == auth.Account && len(i.auth.Secret) > 0 {
					i.connected = true
					i.lastSuccess = time.Now().UTC()
					i.lastError = ""
				}
				i.mu.Unlock()
				if len(batch.Messages) > 0 {
					m.wakeWorker(i)
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
	}()
}

func (m *Manager) stopPoller(i *instance) {
	i.mu.Lock()
	poller := i.poller
	if poller != nil {
		poller.cancel()
	}
	i.mu.Unlock()
	if poller == nil {
		return
	}
	<-poller.done
	i.mu.Lock()
	if i.poller == poller {
		i.poller = nil
	}
	i.mu.Unlock()
}

func (m *Manager) wakeWorker(i *instance) {
	i.mu.Lock()
	defer i.mu.Unlock()
	select {
	case i.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) workerError(i *instance, err error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.connected = false
	i.lastError = "Channel unavailable; retrying"
	if errors.Is(err, channelproto.ErrAuthExpired) {
		if i.active != nil {
			i.active.cancel()
		}
		i.auth = channelproto.Auth{}
		i.lastError = "Login expired; sign in again"
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = m.persist(ctx, i)
	}
}
