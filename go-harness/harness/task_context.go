package harness

import "context"

type TaskNote struct {
	Key      string `json:"key"`
	Content  string `json:"content"`
	Revision int64  `json:"revision"`
}

type TaskHistoryEntry struct {
	ID         int64  `json:"source_id"`
	Role       string `json:"role"`
	Text       string `json:"text"`
	Offset     int    `json:"offset"`
	NextOffset int    `json:"next_offset,omitempty"`
	Truncated  bool   `json:"truncated"`
}

type TaskHistoryPage struct {
	Entries    []TaskHistoryEntry `json:"entries"`
	NextBefore int64              `json:"next_before,omitempty"`
	Through    int64              `json:"through"`
}

// Implementations receive the host-bound request, never a model-supplied session.
type TaskContextStore interface {
	TaskNotes(context.Context, RunRequest) ([]TaskNote, error)
	SetTaskNote(context.Context, RunRequest, string, string, int64) (TaskNote, error)
	SearchTaskHistory(context.Context, RunRequest, string, string, int64) (TaskHistoryPage, error)
	ReadTaskHistory(context.Context, RunRequest, int64, int) (TaskHistoryEntry, error)
}
