package runtime

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type artifactCursor struct {
	Version int    `json:"v"`
	Owner   string `json:"owner"`
	Session string `json:"session"`
	Root    string `json:"root"`
	Node    string `json:"node"`
	After   string `json:"after"`
}

func artifactMetadata(ref harness.AssetRef) harness.TaskTreeArtifact {
	return harness.TaskTreeArtifact{ID: ref.ID, Name: ref.Name, MimeType: ref.MimeType, Size: ref.Size}
}

func taskArtifactIDs(ctx context.Context, tx *sql.Tx, n harness.TaskTreeNode, tables map[string]bool, after string, limit int) ([]string, error) {
	if !tables["harness_artifacts"] || !tables["harness_assets"] {
		return []string{}, nil
	}
	var queries []string
	var args []any
	if tables["harness_artifact_owners"] {
		queries = append(queries, `SELECT asset_id FROM harness_artifact_owners WHERE root_id=? AND root_session_id=? AND node_id=?`)
		args = append(args, n.RootID, n.RootSessionID, n.ID)
	}
	if n.Mode == "background" && tables["harness_background_bindings"] {
		legacy := `SELECT r.asset_id FROM harness_artifacts r JOIN harness_background_bindings b ON b.task_id=r.run_id AND b.child_session_id=r.session_id WHERE json_extract(CAST(b.payload AS TEXT),'$.RootBudgetID')=? AND COALESCE(NULLIF(json_extract(CAST(b.payload AS TEXT),'$.InvocationID'),''),'background/'||b.task_id)=?`
		if tables["harness_artifact_owners"] {
			legacy += ` AND NOT EXISTS(SELECT 1 FROM harness_artifact_owners o WHERE o.asset_id=r.asset_id)`
		}
		queries = append(queries, legacy)
		args = append(args, n.RootID, n.ID)
	}
	if len(queries) == 0 {
		return []string{}, nil
	}
	args = append(args, after, limit)
	rows, err := tx.QueryContext(ctx, `SELECT asset_id FROM (`+strings.Join(queries, ` UNION `)+`) WHERE asset_id>? ORDER BY asset_id LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func treeArtifactSummary(ctx context.Context, tx *sql.Tx, tables map[string]bool, n *harness.TaskTreeNode) error {
	ids, err := taskArtifactIDs(ctx, tx, *n, tables, "", 32)
	if err != nil {
		return err
	}
	for _, id := range ids {
		ref, err := readTaskArtifactTx(ctx, tx, *n, id, tables)
		if err != nil {
			return err
		}
		n.Artifacts = append(n.Artifacts, artifactMetadata(ref))
	}
	return nil
}

// TaskArtifacts pages immutable snapshots across every attempt belonging to
// this logical node. New writes require a new traversal to find earlier IDs.
func (s *Service) TaskArtifacts(ctx context.Context, actor harness.TaskActor, q harness.TaskArtifactQuery) (page harness.TaskArtifactPage, err error) {
	page = harness.TaskArtifactPage{RootID: q.RootID, NodeID: q.NodeID, Artifacts: []harness.TaskTreeArtifact{}, Consistency: "live_keyset"}
	if q.Limit == 0 {
		q.Limit = 32
	}
	if !treeIdentity(q.RootID) || !treeIdentity(q.NodeID) || q.Limit < 1 || q.Limit > 100 || len(q.Cursor) > 16384 {
		return page, harness.ErrInvalidInput
	}
	cursor := artifactCursor{Version: 1, Owner: treeOwner(actor.OwnerID), Session: actor.SessionID, Root: q.RootID, Node: q.NodeID}
	if q.Cursor != "" {
		data, e := base64.RawURLEncoding.DecodeString(q.Cursor)
		if e != nil {
			return page, harness.ErrInvalidInput
		}
		var parsed artifactCursor
		if json.Unmarshal(data, &parsed) != nil || parsed.Version != 1 || parsed.Owner != cursor.Owner || parsed.Session != cursor.Session || parsed.Root != cursor.Root || parsed.Node != cursor.Node || !treeIdentity(parsed.After) || len(parsed.After) > 256 {
			return page, harness.ErrInvalidInput
		}
		cursor = parsed
	}
	if err = s.AuthorizeTaskAccess(ctx, actor); err != nil {
		return page, err
	}
	tx, err := s.Store.db.BeginTx(ctx, nil)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	n, err := s.Store.TaskTreeNodeTx(ctx, tx, actor.SessionID, q.RootID, q.NodeID)
	if err != nil {
		return page, err
	}
	tables, err := treeTables(ctx, tx)
	if err != nil {
		return page, err
	}
	ids, err := taskArtifactIDs(ctx, tx, n, tables, cursor.After, q.Limit+1)
	if err != nil {
		return page, err
	}
	page.HasMore = len(ids) > q.Limit
	if page.HasMore {
		ids = ids[:q.Limit]
	}
	for _, id := range ids {
		ref, e := readTaskArtifactTx(ctx, tx, n, id, tables)
		if e != nil {
			return page, e
		}
		page.Artifacts = append(page.Artifacts, artifactMetadata(ref))
		cursor.After = id
	}
	if page.HasMore {
		data, e := json.Marshal(cursor)
		if e != nil {
			return page, e
		}
		page.NextCursor = base64.RawURLEncoding.EncodeToString(data)
	}
	if err = s.AuthorizeTaskAccess(ctx, actor); err != nil {
		return page, err
	}
	err = tx.Commit()
	return page, err
}
