package assets

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
)

// ArtifactOwner is host-supplied provenance, separate from model-facing
// Content and tool JSON. Snapshot publication commits it with the tool receipt.
type ArtifactOwner struct {
	Scope                                                  budget.Scope
	RootSessionID, InvocationID, RoleAttemptID, ToolCallID string
}

func (o ArtifactOwner) NodeID() string {
	if o.InvocationID != "" {
		return o.InvocationID
	}
	return "root/" + o.Scope.RootBudgetID
}

type ownerQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Store) StageAttributedArtifacts(ctx context.Context, x harness.Session, runID, callID string, paths []string, rootID, invocationID, roleAttemptID string) ([]harness.Content, error) {
	scope, ok := budget.ScopeFromContext(ctx)
	if !ok || scope.MemberID != runID || scope.SessionID != x.ID || scope.RootBudgetID == "" || (invocationID != "" && scope.RootBudgetID != rootID) || (invocationID == "") != (roleAttemptID == "") {
		return nil, harness.ErrExecutionConflict
	}
	owner := ArtifactOwner{Scope: scope, InvocationID: invocationID, RoleAttemptID: roleAttemptID, ToolCallID: callID}
	if err := s.db.QueryRowContext(ctx, `SELECT session_id FROM budget_roots WHERE id=?`, scope.RootBudgetID).Scan(&owner.RootSessionID); err != nil {
		return nil, err
	}
	if err := CheckArtifactOwner(ctx, s.db, owner, true); err != nil {
		return nil, err
	}
	return s.stageArtifacts(ctx, x, runID, callID, paths, &owner)
}

// CheckArtifactOwner validates the saved relationships, including historical
// attempts on reads. Live publication additionally requires both active leases.
func CheckArtifactOwner(ctx context.Context, q ownerQuery, o ArtifactOwner, live bool) error {
	if o.Scope.RootBudgetID == "" || o.Scope.MemberID == "" || o.Scope.SessionID == "" || o.Scope.AttemptID == "" || o.Scope.Fence < 1 || o.RootSessionID == "" || o.ToolCallID == "" || (o.InvocationID == "") != (o.RoleAttemptID == "") {
		return harness.ErrExecutionConflict
	}
	var matches bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM budget_attempts a JOIN budget_members m ON m.id=a.member_id AND m.root_id=a.root_id AND m.session_id=a.session_id JOIN budget_roots r ON r.id=a.root_id JOIN harness_runs h ON h.id=a.member_id AND h.session_id=a.session_id WHERE a.id=? AND a.root_id=? AND a.member_id=? AND a.session_id=? AND a.fence=? AND r.session_id=? AND (?=0 OR a.state='active'))`, o.Scope.AttemptID, o.Scope.RootBudgetID, o.Scope.MemberID, o.Scope.SessionID, o.Scope.Fence, o.RootSessionID, live).Scan(&matches)
	if err != nil {
		return err
	}
	if !matches {
		return harness.ErrExecutionConflict
	}
	if o.InvocationID == "" {
		// A private background session must retain its logical role attribution.
		if o.Scope.SessionID != o.RootSessionID {
			return harness.ErrExecutionConflict
		}
		return nil
	}
	err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_agent_attempts a JOIN harness_agent_invocations i ON i.id=a.invocation_id WHERE a.id=? AND i.id=? AND i.root_id=? AND (?=0 OR (a.state='active' AND a.started_ns+a.reserved_ns>?)))`, o.RoleAttemptID, o.InvocationID, o.Scope.RootBudgetID, live, time.Now().UnixNano()).Scan(&matches)
	if err != nil {
		return err
	}
	if !matches {
		return harness.ErrExecutionConflict
	}
	return nil
}

// ReadArtifactOwner never infers ownership from the node's newest run. It joins
// the exact committed asset, call and attempt which produced the snapshot.
func ReadArtifactOwner(ctx context.Context, q ownerQuery, id string) (ArtifactOwner, harness.AssetRef, error) {
	var o ArtifactOwner
	var ref harness.AssetRef
	var data []byte
	var node string
	err := q.QueryRowContext(ctx, `SELECT a.metadata,o.session_id,o.run_id,o.tool_call_id,o.root_id,o.root_session_id,o.node_id,o.attempt_id,o.fence,o.invocation_id,o.role_attempt_id FROM harness_artifact_owners o JOIN harness_artifacts p ON p.asset_id=o.asset_id AND p.run_id=o.run_id AND p.tool_call_id=o.tool_call_id AND p.session_id=o.session_id JOIN harness_assets a ON a.id=o.asset_id AND a.session_id=o.session_id WHERE o.asset_id=?`, id).Scan(&data, &o.Scope.SessionID, &o.Scope.MemberID, &o.ToolCallID, &o.Scope.RootBudgetID, &o.RootSessionID, &node, &o.Scope.AttemptID, &o.Scope.Fence, &o.InvocationID, &o.RoleAttemptID)
	if errors.Is(err, sql.ErrNoRows) {
		return o, ref, harness.ErrNotFound
	}
	if err != nil {
		return o, ref, err
	}
	if json.Unmarshal(data, &ref) != nil || ref.ID != id || ref.SessionID != o.Scope.SessionID || ref.Kind != harness.AssetArtifact || node != o.NodeID() {
		return o, ref, harness.ErrExecutionConflict
	}
	if err := CheckArtifactOwner(ctx, q, o, false); err != nil {
		return o, ref, err
	}
	return o, ref, nil
}
