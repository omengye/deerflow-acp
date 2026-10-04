package runtime

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strings"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/assets"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
)

func validateTaskArtifactRead(r harness.TaskArtifactReadRequest) (int, error) {
	limit := r.Limit
	if limit == 0 {
		limit = harness.DefaultTaskArtifactReadLimit
	}
	if !treeIdentity(r.RootID) || !treeIdentity(r.NodeID) || !treeIdentity(r.ArtifactID) || len(r.ArtifactID) > 256 || strings.ContainsAny(r.ArtifactID, "/\\") || r.Offset < 0 || limit < 1 || limit > harness.MaxTaskArtifactReadLimit || r.Offset > math.MaxInt64-int64(limit) {
		return 0, harness.ErrInvalidInput
	}
	if r.ExpectedSHA256 != "" {
		if len(r.ExpectedSHA256) != 64 {
			return 0, harness.ErrInvalidInput
		}
		if bytes, err := hex.DecodeString(r.ExpectedSHA256); err != nil || len(bytes) != 32 {
			return 0, harness.ErrInvalidInput
		}
	}
	if r.Offset > 0 && r.ExpectedSHA256 == "" {
		return 0, harness.ErrInvalidInput
	}
	return limit, nil
}

// ReadTaskArtifact holds the root connection's lifecycle lease through both
// attribution checks and byte verification. A child session cannot be attached
// or supplied by the caller. Historical native attempts remain readable under
// their original logical node after a continuation becomes its newest task.
func (s *Service) ReadTaskArtifact(ctx context.Context, actor harness.TaskActor, request harness.TaskArtifactReadRequest) (harness.TaskArtifactChunk, error) {
	var out harness.TaskArtifactChunk
	limit, err := validateTaskArtifactRead(request)
	if err != nil {
		return out, err
	}
	ctx, release, err := s.Coordinator.Begin(ctx, actor.SessionID, actor.OwnerID)
	if err != nil {
		return out, err
	}
	defer release()
	if s.Assets == nil {
		return out, harness.ErrNotFound
	}
	var ref harness.AssetRef
	err = withExecutionTransaction(ctx, s.Store, func(tx *sql.Tx) error {
		node, err := s.Store.TaskTreeNodeTx(ctx, tx, actor.SessionID, request.RootID, request.NodeID)
		if err != nil {
			return err
		}
		tables, err := treeTables(ctx, tx)
		if err != nil {
			return err
		}
		ref, err = readTaskArtifactTx(ctx, tx, node, request.ArtifactID, tables)
		if err != nil {
			return err
		}
		if request.ExpectedSHA256 != "" && request.ExpectedSHA256 != ref.SHA256 {
			return harness.ErrExecutionConflict
		}
		return s.Coordinator.Authorize(actor.SessionID, actor.OwnerID)
	})
	if err != nil {
		return out, err
	}
	data, err := s.Assets.ReadArtifactRange(ctx, ref.SessionID, ref, request.Offset, limit)
	if err != nil {
		return out, err
	}
	if err = s.Coordinator.Authorize(actor.SessionID, actor.OwnerID); err != nil {
		return out, err
	}
	if err = ctx.Err(); err != nil {
		return out, err
	}
	out = harness.TaskArtifactChunk{RootID: request.RootID, NodeID: request.NodeID, Artifact: harness.TaskTreeArtifact{ID: ref.ID, Name: ref.Name, MimeType: ref.MimeType, Size: ref.Size}, SHA256: ref.SHA256, Encoding: "base64", Data: data, Offset: request.Offset, NextOffset: request.Offset + int64(len(data))}
	out.EOF = out.NextOffset == ref.Size
	return out, nil
}

func readTaskArtifactTx(ctx context.Context, tx *sql.Tx, node harness.TaskTreeNode, artifactID string, tables map[string]bool) (ref harness.AssetRef, err error) {
	if tables["harness_artifact_owners"] {
		o, asset, ownerErr := assets.ReadArtifactOwner(ctx, tx, artifactID)
		if ownerErr == nil {
			if o.RootSessionID != node.RootSessionID || o.Scope.RootBudgetID != node.RootID || o.NodeID() != node.ID {
				return ref, harness.ErrNotFound
			}
			return asset, nil
		}
		if !errors.Is(ownerErr, harness.ErrNotFound) {
			return ref, ownerErr
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_artifact_owners WHERE asset_id=?)`, artifactID).Scan(&exists); err != nil {
			return ref, err
		}
		if exists {
			return ref, harness.ErrExecutionConflict
		}
	}
	// Older snapshots have only a native task binding. Preserve that exact
	// container relationship; never invent a foreground or root attribution.
	if node.Mode != "background" || !tables["harness_background_bindings"] {
		return ref, harness.ErrNotFound
	}
	var metadata, bindingJSON, taskJSON []byte
	var sessionID, runID, childID, parentID string
	err = tx.QueryRowContext(ctx, `SELECT a.metadata,r.session_id,r.run_id,b.payload,b.child_session_id,b.parent_session_id,t.payload
FROM harness_artifacts r JOIN harness_assets a ON a.id=r.asset_id AND a.session_id=r.session_id
JOIN harness_background_bindings b ON b.task_id=r.run_id AND b.child_session_id=r.session_id
JOIN eino_background_tasks t ON t.id=b.task_id
WHERE r.asset_id=? AND json_extract(CAST(b.payload AS TEXT),'$.RootBudgetID')=?
AND COALESCE(NULLIF(json_extract(CAST(b.payload AS TEXT),'$.InvocationID'),''),'background/'||b.task_id)=?`, artifactID, node.RootID, node.ID).Scan(&metadata, &sessionID, &runID, &bindingJSON, &childID, &parentID, &taskJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return ref, harness.ErrNotFound
	}
	if err != nil {
		return ref, err
	}
	var binding background.Binding
	var task bt.Task
	if json.Unmarshal(metadata, &ref) != nil || json.Unmarshal(bindingJSON, &binding) != nil || json.Unmarshal(taskJSON, &task) != nil {
		return ref, harness.ErrExecutionConflict
	}
	if binding.RootSession() != node.RootSessionID || binding.RootBudgetID != node.RootID || binding.RoleID() != node.ID || binding.TaskID != runID || binding.ChildSessionID != sessionID || childID != sessionID || binding.ParentSessionID != parentID || task.Spec.ID != runID || task.Spec.SessionID != parentID || ref.ID != artifactID || ref.SessionID != sessionID || ref.Kind != harness.AssetArtifact {
		return ref, harness.ErrPermissionDenied
	}
	return ref, nil
}
