package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"time"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
)

func (s *TaskStore) authorizeAttempt(r *taskRecord, attempt int64) error {
	if r.task.Status != bt.StatusRunning || r.task.Attempt != attempt || r.task.CancelRequestedAt != nil || r.lease <= s.now().UnixNano() {
		return bt.ErrLeaseLost
	}
	return nil
}

func (s *TaskStore) AppendTaskEvent(ctx context.Context, req *bt.AppendTaskEventRequest) (*bt.AppendTaskEventResult, error) {
	if req == nil || req.TaskID == "" || req.Attempt <= 0 || req.EventID == "" || len(req.EventID) > 1024 || len(req.Data) == 0 {
		return nil, errors.New("backgroundtask: event requires task, attempt, event id (up to 1024 bytes), and data")
	}
	if err := s.checkSize("event data", req.Data); err != nil {
		return nil, err
	}
	result := &bt.AppendTaskEventResult{}
	_, err := s.withTask(ctx, req.TaskID, true, func(tx *sql.Tx, r *taskRecord) error {
		if err := s.authorizeAttempt(r, req.Attempt); err != nil {
			return err
		}
		var data []byte
		var created string
		err := tx.QueryRowContext(ctx, "SELECT data,created_at FROM eino_task_events WHERE task_id=? AND event_id=?", req.TaskID, req.EventID).Scan(&data, &created)
		if err == nil {
			if !bytes.Equal(data, req.Data) {
				return bt.ErrTaskEventIDConflict
			}
			at, parseErr := time.Parse(time.RFC3339Nano, created)
			if parseErr != nil {
				return parseErr
			}
			result.Event = &bt.TaskEvent{TaskID: req.TaskID, EventID: req.EventID, Data: data, CreatedAt: at}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		now := s.now().UTC()
		_, err = tx.ExecContext(ctx, "INSERT INTO eino_task_events(task_id,event_id,data,created_at) VALUES(?,?,?,?)", req.TaskID, req.EventID, req.Data, now.Format(time.RFC3339Nano))
		if err != nil {
			return err
		}
		result.Inserted = true
		result.Event = &bt.TaskEvent{TaskID: req.TaskID, EventID: req.EventID, Data: append([]byte(nil), req.Data...), CreatedAt: now}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

type taskEventCursor struct {
	Version  int    `json:"v"`
	Store    string `json:"store"`
	Task     string `json:"task"`
	End      int64  `json:"end"`
	Position int64  `json:"position"`
	Reverse  bool   `json:"reverse"`
}

func (s *TaskStore) ListTaskEvents(ctx context.Context, req *bt.ListTaskEventsRequest) (*bt.ListTaskEventsResult, error) {
	if req == nil || req.TaskID == "" {
		return nil, errors.New("backgroundtask: task id is required")
	}
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = loadTask(ctx, tx, req.TaskID); err != nil {
		return nil, err
	}
	cursor := taskEventCursor{Version: 1, Store: s.namespace, Task: req.TaskID, Reverse: req.NewestFirst}
	var max int64
	if err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq),0) FROM eino_task_events WHERE task_id=?", req.TaskID).Scan(&max); err != nil {
		return nil, err
	}
	cursor.End = max
	if req.Cursor != "" {
		cursor = taskEventCursor{}
		if err = decodeCursor(req.Cursor, &cursor); err != nil {
			return nil, err
		}
		if cursor.Version != 1 || cursor.Store != s.namespace || cursor.Task != req.TaskID || cursor.Reverse != req.NewestFirst || cursor.End <= 0 || cursor.End > max || cursor.Position <= 0 || cursor.Position > cursor.End {
			return nil, bt.ErrInvalidCursor
		}
		var count int
		if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM eino_task_events WHERE task_id=? AND seq IN (?,?)", req.TaskID, cursor.End, cursor.Position).Scan(&count); err != nil {
			return nil, err
		}
		want := 2
		if cursor.End == cursor.Position {
			want = 1
		}
		if count != want {
			return nil, bt.ErrInvalidCursor
		}
	}
	args := []any{req.TaskID, cursor.End}
	query := "SELECT seq,event_id,data,created_at FROM eino_task_events WHERE task_id=? AND seq<=?"
	if req.Cursor != "" {
		if req.NewestFirst {
			query += " AND seq<?"
		} else {
			query += " AND seq>?"
		}
		args = append(args, cursor.Position)
	}
	if req.NewestFirst {
		query += " ORDER BY seq DESC"
	} else {
		query += " ORDER BY seq ASC"
	}
	limit := pageLimit(req.Limit)
	query += " LIMIT ?"
	args = append(args, limit+1)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	result := &bt.ListTaskEventsResult{}
	for rows.Next() {
		var seq int64
		var created string
		e := &bt.TaskEvent{TaskID: req.TaskID}
		if err = rows.Scan(&seq, &e.EventID, &e.Data, &created); err != nil {
			rows.Close()
			return nil, err
		}
		if len(result.Events) == limit {
			result.NextCursor, err = encodeCursor(cursor)
			if err != nil {
				rows.Close()
				return nil, err
			}
			break
		}
		if e.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			rows.Close()
			return nil, err
		}
		result.Events = append(result.Events, e)
		cursor.Position = seq
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}
