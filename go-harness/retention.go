package deerflow

import (
	"context"
	"errors"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// ArchiveExpiredSessions makes one bounded-memory pass over the persistent
// foreground session inventory. Busy sessions, background parents and unknown
// tool effects remain for a later pass; one blocked ID cannot starve others.
func (c *Client) ArchiveExpiredSessions(ctx context.Context) (int, error) {
	done, err := c.operation()
	if err != nil {
		return 0, err
	}
	defer done()
	if !c.retention.Enabled {
		return 0, nil
	}
	now := time.Now().UTC()
	archived := 0
	cursor := ""
	var failures error
	for {
		ids, next, pageErr := c.service.Store.ExpiredSessionIDsPage(ctx, now, c.retention, cursor, 256)
		if pageErr != nil {
			return archived, errors.Join(failures, pageErr)
		}
		for _, id := range ids {
			retired, archiveErr := c.service.ArchiveExpiredSession(ctx, id, now, c.retention)
			if retired {
				archived++
			}
			if archiveErr != nil && !errors.Is(archiveErr, harness.ErrBusy) && !errors.Is(archiveErr, harness.ErrReceiptConflict) {
				failures = errors.Join(failures, archiveErr)
			}
		}
		if next == "" {
			return archived, failures
		}
		cursor = next
	}
}

// CleanupExpiredSessions is the compatibility entry point for host maintenance.
// Its count is archived sessions, never deleted sessions.
// Deprecated: use ArchiveExpiredSessions.
func (c *Client) CleanupExpiredSessions(ctx context.Context) (int, error) {
	return c.ArchiveExpiredSessions(ctx)
}
