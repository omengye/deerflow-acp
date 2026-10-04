package harness

import "context"

const (
	DefaultTaskArtifactReadLimit = 256 * 1024
	MaxTaskArtifactReadLimit     = 1024 * 1024
)

// An artifact ID identifies an immutable committed snapshot. Clients never
// supply a filesystem path or an AssetRef. Reads after the first chunk must
// carry the SHA256 returned by that first read.
type TaskArtifactReadRequest struct {
	RootID         string `json:"rootId"`
	NodeID         string `json:"nodeId"`
	ArtifactID     string `json:"artifactId"`
	Offset         int64  `json:"offset"`
	Limit          int    `json:"limit,omitempty"`
	ExpectedSHA256 string `json:"expectedSha256,omitempty"`
}

type TaskArtifactChunk struct {
	RootID     string           `json:"rootId"`
	NodeID     string           `json:"nodeId"`
	Artifact   TaskTreeArtifact `json:"artifact"`
	SHA256     string           `json:"sha256"`
	Encoding   string           `json:"encoding"`
	Data       []byte           `json:"data"`
	Offset     int64            `json:"offset"`
	NextOffset int64            `json:"nextOffset"`
	EOF        bool             `json:"eof"`
}

// TaskArtifactReader reads node artifacts with durable attribution.
// Reads require an attached, idle root session; an active prompt returns ErrBusy.
type TaskArtifactReader interface {
	ReadTaskArtifact(context.Context, TaskActor, TaskArtifactReadRequest) (TaskArtifactChunk, error)
}

type TaskArtifactQuery struct {
	RootID string `json:"rootId"`
	NodeID string `json:"nodeId"`
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

type TaskArtifactPage struct {
	RootID      string             `json:"rootId"`
	NodeID      string             `json:"nodeId"`
	Artifacts   []TaskTreeArtifact `json:"artifacts"`
	NextCursor  string             `json:"nextCursor,omitempty"`
	HasMore     bool               `json:"hasMore"`
	Consistency string             `json:"consistency"`
}

type TaskArtifactLister interface {
	TaskArtifacts(context.Context, TaskActor, TaskArtifactQuery) (TaskArtifactPage, error)
}
