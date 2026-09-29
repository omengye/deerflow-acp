package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

var _ adk.SessionEventStore[*schema.Message] = (*Store)(nil)

// AppendEvents validates the whole batch and commits it atomically. An exact
// retry of persisted bytes is a no-op; conflicting IDs and duplicate IDs in an
// input batch are rejected with Eino's sentinel.
// Ordering is the committed append position, never timestamps or event IDs.
func (s *Store) AppendEvents(ctx context.Context, sessionID string, events []*adk.SessionEvent[*schema.Message]) error {
	if sessionID == "" {
		return errors.New("sqlite: session id is empty")
	}
	type encoded struct {
		id   string
		kind adk.SessionEventKind
		data []byte
	}
	pending := make([]encoded, 0, len(events))
	seen := make(map[string]bool, len(events))
	for _, event := range events {
		if event == nil || event.EventID == "" {
			return adk.ErrInvalidEventID
		}
		if seen[event.EventID] {
			return adk.ErrDuplicateEventID
		}
		seen[event.EventID] = true
		// Normalize a copy; callers retain ownership of their event envelope.
		e := *event
		if err := adk.NormalizeSessionEventKind(&e); err != nil {
			return err
		}
		data, err := s.serializer.Marshal(&e)
		if err != nil {
			return fmt.Errorf("sqlite: encode session event: %w", err)
		}
		pending = append(pending, encoded{e.EventID, e.Kind, data})
	}
	if len(pending) == 0 {
		return ctx.Err()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, event := range pending {
		var persisted []byte
		err = tx.QueryRowContext(ctx, "SELECT payload FROM eino_session_events WHERE session_id=? AND event_id=?", sessionID, event.id).Scan(&persisted)
		if err == nil {
			if bytes.Equal(persisted, event.data) {
				continue
			}
			return adk.ErrDuplicateEventID
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO eino_session_events(session_id,event_id,kind,payload) VALUES(?,?,?,?)", sessionID, event.id, string(event.kind), event.data); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) LoadEvents(ctx context.Context, sessionID string, req *adk.LoadSessionEventsRequest) (*adk.LoadSessionEventsResult[*schema.Message], error) {
	if sessionID == "" {
		return nil, errors.New("sqlite: session id is empty")
	}
	if req == nil {
		req = &adk.LoadSessionEventsRequest{}
	}
	if req.Limit < 0 {
		return nil, errors.New("sqlite: negative session event limit")
	}
	// Session logs are append-only, so a resolved append position stays valid.
	args := []any{sessionID}
	query := "SELECT event_id,payload FROM eino_session_events WHERE session_id=?"
	if req.After != "" {
		var seq int64
		err := s.db.QueryRowContext(ctx, "SELECT seq FROM eino_session_events WHERE session_id=? AND event_id=?", sessionID, req.After).Scan(&seq)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, adk.ErrEventIDOutOfRange
		}
		if err != nil {
			return nil, err
		}
		if req.Reverse {
			query += " AND seq<?"
		} else {
			query += " AND seq>?"
		}
		args = append(args, seq)
	}
	if len(req.Kinds) > 0 {
		query += " AND kind IN (" + strings.TrimSuffix(strings.Repeat("?,", len(req.Kinds)), ",") + ")"
		for _, kind := range req.Kinds {
			args = append(args, string(kind))
		}
	}
	if req.Reverse {
		query += " ORDER BY seq DESC"
	} else {
		query += " ORDER BY seq ASC"
	}
	if req.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, req.Limit+1)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := &adk.LoadSessionEventsResult[*schema.Message]{}
	for rows.Next() {
		var id string
		var data []byte
		if err = rows.Scan(&id, &data); err != nil {
			return nil, err
		}
		if req.Limit > 0 && len(result.Events) == req.Limit {
			result.Next = result.Events[len(result.Events)-1].EventID
			break
		}
		var event adk.SessionEvent[*schema.Message]
		if err = s.serializer.Unmarshal(data, &event); err != nil {
			return nil, fmt.Errorf("sqlite: decode event %q: %w", id, err)
		}
		result.Events = append(result.Events, &event)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
