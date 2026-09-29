package deerflow

import (
	"context"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// HistoryPage reads durable events from an attached, idle session. Limit is
// 1..500 (zero uses 128). Pass the returned cursor to continue the same snapshot.
func (c *Client) HistoryPage(ctx context.Context, sessionID, cursor string, limit int) (harness.EventPage, error) {
	done, err := c.operation()
	if err != nil {
		return harness.EventPage{}, err
	}
	defer done()
	return c.service.HistoryPage(ctx, c.owner, sessionID, cursor, limit)
}
