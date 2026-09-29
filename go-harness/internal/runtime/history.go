package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

const maxHistoryPageBytes = 2 << 20

type historyCursor struct {
	Version int    `json:"v"`
	Session string `json:"s"`
	After   int64  `json:"a"`
	Through int64  `json:"t"`
}

// HistoryPage bounds row count and page memory; a single large immutable event
// is returned alone so it never becomes impossible to retrieve. The cursor is
// a read position, not authorization; Service checks current ownership.
func (s *Store) HistoryPage(ctx context.Context, id, cursor string, limit int) (harness.EventPage, error) {
	page := harness.EventPage{Events: make([]harness.RunEvent, 0)}
	if limit == 0 {
		limit = 128
	}
	if id == "" || limit < 1 || limit > 500 || len(cursor) > 1024 {
		return page, fmt.Errorf("%w: invalid history page", harness.ErrInvalidInput)
	}
	position := historyCursor{Version: 1, Session: id}
	if cursor == "" {
		if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0) FROM harness_events WHERE session_id=?`, id).Scan(&position.Through); err != nil {
			return page, err
		}
	} else {
		position = historyCursor{}
		data, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || json.Unmarshal(data, &position) != nil || position.Version != 1 || position.Session != id || position.After < 0 || position.Through < position.After {
			return page, fmt.Errorf("%w: invalid history cursor", harness.ErrInvalidInput)
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT sequence,event FROM harness_events WHERE session_id=? AND sequence>? AND sequence<=? ORDER BY sequence LIMIT ?`, id, position.After, position.Through, limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	bytesRead := 0
	for rows.Next() {
		var seq int64
		var data []byte
		if err = rows.Scan(&seq, &data); err != nil {
			return page, err
		}
		if len(page.Events) == limit || (len(page.Events) > 0 && bytesRead+len(data) > maxHistoryPageBytes) {
			encoded, err := json.Marshal(position)
			if err != nil {
				return page, err
			}
			page.NextCursor = base64.RawURLEncoding.EncodeToString(encoded)
			break
		}
		var event harness.RunEvent
		if err = json.Unmarshal(data, &event); err != nil {
			return page, err
		}
		event.Sequence = seq
		page.Events = append(page.Events, event)
		bytesRead += len(data)
		position.After = seq
	}
	return page, rows.Err()
}

func (s *Service) HistoryPage(ctx context.Context, owner, id, cursor string, limit int) (harness.EventPage, error) {
	ctx, release, err := s.Coordinator.Begin(ctx, id, owner)
	if err != nil {
		return harness.EventPage{}, err
	}
	defer release()
	return s.Store.HistoryPage(ctx, id, cursor, limit)
}
