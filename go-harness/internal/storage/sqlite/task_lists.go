package sqlite

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
)

func pageLimit(limit int) int {
	if limit <= 0 {
		return 100
	}
	if limit > 1000 {
		return 1000
	}
	return limit
}

func encodeCursor(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}
func decodeCursor(value string, v any) error {
	if len(value) > 64<<10 {
		return bt.ErrInvalidCursor
	}
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return bt.ErrInvalidCursor
	}
	if err = json.Unmarshal(data, v); err != nil {
		return bt.ErrInvalidCursor
	}
	return nil
}

type taskListCursor struct {
	Version int       `json:"v"`
	Store   string    `json:"store"`
	Status  bt.Status `json:"status"`
	Keys    []string  `json:"keys"`
	Last    string    `json:"last"`
}

func (s *TaskStore) ListPending(ctx context.Context, req *bt.ListPendingRequest) (*bt.ListPendingResult, error) {
	if req == nil {
		return nil, errors.New("backgroundtask: pending list request is required")
	}
	tasks, next, err := s.listTasks(ctx, req.ExecutorKeys, req.Cursor, req.Limit, bt.StatusPending)
	if err != nil {
		return nil, err
	}
	return &bt.ListPendingResult{Tasks: tasks, NextCursor: next}, nil
}
func (s *TaskStore) ListSuspended(ctx context.Context, req *bt.ListSuspendedRequest) (*bt.ListSuspendedResult, error) {
	if req == nil {
		return nil, errors.New("backgroundtask: suspended list request is required")
	}
	tasks, next, err := s.listTasks(ctx, req.ExecutorKeys, req.Cursor, req.Limit, bt.StatusSuspended)
	if err != nil {
		return nil, err
	}
	return &bt.ListSuspendedResult{Tasks: tasks, NextCursor: next}, nil
}

func (s *TaskStore) listTasks(ctx context.Context, keys []string, value string, limit int, status bt.Status) ([]*bt.Task, string, error) {
	if len(keys) == 0 {
		return nil, "", errors.New("backgroundtask: list requires executor keys")
	}
	keys = slices.Clone(keys)
	slices.Sort(keys)
	keys = slices.Compact(keys)
	if keys[0] == "" {
		return nil, "", errors.New("backgroundtask: executor key is empty")
	}
	cursor := taskListCursor{Version: 1, Store: s.namespace, Status: status, Keys: keys}
	if value != "" {
		cursor = taskListCursor{}
		if err := decodeCursor(value, &cursor); err != nil {
			return nil, "", err
		}
		if cursor.Version != 1 || cursor.Store != s.namespace || cursor.Status != status || cursor.Last == "" || !slices.Equal(cursor.Keys, keys) {
			return nil, "", bt.ErrInvalidCursor
		}
	}
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()
	if cursor.Last != "" {
		var found int
		if err = tx.QueryRowContext(ctx, "SELECT 1 FROM eino_background_tasks WHERE id=?", cursor.Last).Scan(&found); err != nil {
			return nil, "", bt.ErrInvalidCursor
		}
	}
	args := []any{cursor.Last}
	for _, key := range keys {
		args = append(args, key)
	}
	// Running tasks may have expired into pending. Read candidate IDs first so
	// no rows remain open while resolving and writing their lease transitions.
	query := "SELECT id FROM eino_background_tasks WHERE id>? AND executor_key IN (" + strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",") + ") AND status IN (?,?) ORDER BY id"
	args = append(args, string(status), string(bt.StatusRunning))
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, "", err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, "", err
	}
	limit = pageLimit(limit)
	var out []*bt.Task
	var next string
	for _, id := range ids {
		r, err := loadTask(ctx, tx, id)
		if err != nil {
			return nil, "", err
		}
		before := r.task.Version
		previous := cloneTaskSnapshot(r.task)
		if s.expire(r) {
			if err = s.transitionHook(ctx, tx, previous, r.task); err != nil {
				return nil, "", err
			}
			if err = saveTask(ctx, tx, r, before); err != nil {
				return nil, "", err
			}
			if err = s.enqueueLifecycle(ctx, tx, r.task, statusNotification(r.task.Status)); err != nil {
				return nil, "", err
			}
		}
		if r.task.Status != status {
			continue
		}
		if len(out) == limit {
			cursor.Last = out[len(out)-1].Spec.ID
			next, err = encodeCursor(cursor)
			if err != nil {
				return nil, "", err
			}
			break
		}
		out = append(out, r.task)
	}
	if err = tx.Commit(); err != nil {
		return nil, "", err
	}
	return out, next, nil
}
