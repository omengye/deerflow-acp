package deerflow

import (
	"context"
	"errors"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// CleanupExpiredSessions makes one bounded-memory pass over the persistent
// foreground session inventory. Busy sessions, background parents and unknown
// tool effects remain for a later pass; one blocked ID cannot starve others.
func (c *Client) CleanupExpiredSessions(ctx context.Context) (int, error) {
	done, err := c.operation()
	if err != nil {
		return 0, err
	}
	defer done()
	if !c.retention.Enabled {
		return 0, nil
	}
	now := time.Now().UTC()
	deleted := 0
	cursor := ""
	var failures error
	for {
		ids, next, pageErr := c.service.Store.ExpiredSessionIDsPage(ctx, now, c.retention, cursor, 256)
		if pageErr != nil {
			return deleted, errors.Join(failures, pageErr)
		}
		for _, id := range ids {
			removed, removeErr := c.service.DeleteExpiredSession(ctx, id, now, c.retention)
			if removed {
				deleted++
			}
			if removeErr != nil && !errors.Is(removeErr, harness.ErrBusy) && !errors.Is(removeErr, harness.ErrReceiptConflict) {
				failures = errors.Join(failures, removeErr)
			}
		}
		if next == "" {
			return deleted, failures
		}
		cursor = next
	}
}
