package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
	"github.com/omengye/deerflow-acp/go-harness/internal/learning"
)

type learningRootMember struct {
	learning.RootObservation
	Origin string
	Raw    []byte
}

// LearningRootObservationsTx reconstructs host ownership inside the caller's
// transaction. Synthetic inputs and native/private result bodies stay out of
// the model view; their hashes only detect changed capture provenance.
func (s *Store) LearningRootObservationsTx(ctx context.Context, tx *sql.Tx, root, session, source string) ([]learning.RootObservation, error) {
	var rootRun, rootSession, workspace string
	if err := tx.QueryRowContext(ctx, `SELECT b.root_run_id,b.session_id,s.cwd FROM budget_roots b JOIN harness_sessions s ON s.id=b.session_id WHERE b.id=?`, root).Scan(&rootRun, &rootSession, &workspace); err != nil {
		return nil, err
	}
	if rootRun != source || rootSession != session {
		return nil, harness.ErrPermissionDenied
	}
	rows, err := tx.QueryContext(ctx, `SELECT m.id,m.session_id,m.origin_id,m.parent_id,m.kind,r.input_id,r.status,r.updated_at,i.content,
 (SELECT COALESCE(MAX(sequence),0) FROM harness_events WHERE run_id=m.id)
 FROM budget_members m LEFT JOIN harness_runs r ON r.id=m.id AND r.session_id=m.session_id
 LEFT JOIN harness_inputs i ON i.id=r.input_id AND i.run_id=r.id WHERE m.root_id=? ORDER BY m.id LIMIT 10`, root)
	if err != nil {
		return nil, err
	}
	members := []learningRootMember{}
	for rows.Next() {
		var m learningRootMember
		if err = rows.Scan(&m.Run, &m.Session, &m.Origin, &m.Parent, &m.Kind, &m.Input, &m.Status, &m.Ended, &m.Raw, &m.Frontier); err != nil {
			err = errors.Join(harness.ErrPermissionDenied, err)
			break
		}
		if len(m.Raw) > 2<<20 || !json.Valid(m.Raw) {
			err = harness.ErrInvalidInput
			break
		}
		m.InputSHA = executionDigest(m.Raw)
		members = append(members, m)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if len(members) < 1 || len(members) > 9 {
		return nil, harness.ErrInvalidInput
	}
	byID := map[string]learningRootMember{}
	foundSource := false
	for _, m := range members {
		byID[m.Run] = m
		if m.Run == source {
			foundSource = m.Session == session && m.Kind == "run" && m.Origin == source && m.Parent == "" && (m.Status == "completed" || m.Status == "failed")
		}
	}
	if !foundSource {
		return nil, harness.ErrPermissionDenied
	}
	out := []learning.RootObservation{}
	for _, m := range members {
		if m.Run == source {
			continue
		}
		if m.Origin != m.Parent || m.Parent == "" || m.Status != "completed" && m.Status != "failed" {
			return nil, harness.ErrPermissionDenied
		}
		current := m
		for depth := 0; current.Run != source; depth++ {
			parent, ok := byID[current.Parent]
			if depth >= 8 || !ok || current.Origin != parent.Run {
				return nil, harness.ErrPermissionDenied
			}
			current = parent
		}
		if _, err = time.Parse(time.RFC3339Nano, m.Ended); err != nil {
			return nil, harness.ErrInvalidInput
		}
		child, err := scanSession(tx.QueryRowContext(ctx, `SELECT s.id,s.cwd,s.title,s.mode,s.model,s.created_at,s.updated_at,c.approval_mode,c.subagents,c.version,c.reasoning_effort FROM harness_sessions s JOIN harness_session_configs c ON c.session_id=s.id WHERE s.id=?`, m.Session))
		if err != nil {
			return nil, err
		}
		if child.CWD != workspace {
			return nil, harness.ErrPermissionDenied
		}
		var evidence any
		switch m.Kind {
		case "task", "background_continuation":
			evidence, err = s.learningBackgroundEvidenceTx(ctx, tx, root, session, m, byID)
		case "continuation":
			var row executionRow
			row, err = readExecution(ctx, tx, m.Session, m.Run)
			if err == nil && (row.RootBudgetID != root || row.State.InputID != m.Input || row.InputDigest != m.InputSHA || row.State.Status != harness.ExecutionStatus(m.Status) || row.State.BlockedReason != "" || executionConfigDigest(row.Config) != executionConfigDigest(child)) {
				err = harness.ErrPermissionDenied
			}
			if err == nil {
				var source *continuationSource
				source, err = loadContinuationSource(ctx, tx, row)
				if err == nil && (source == nil || source.OriginRunID != m.Origin || source.RootBudgetID != root) {
					err = harness.ErrPermissionDenied
				}
				evidence = []any{row, source}
			}
		default:
			err = harness.ErrPermissionDenied
		}
		if err != nil {
			return nil, err
		}
		// All sessions and member edges are checked, not only cited children.
		m.EvidenceSHA = learningRootDigest([]any{learning.RootEvidencePolicyVersion, root, session, m.RootObservation, m.Origin, executionConfigDigest(child), evidence})
		out = append(out, m.RootObservation)
	}
	return out, nil
}

func learningRootDigest(value any) string {
	data, _ := json.Marshal(value)
	return executionDigest(data)
}

func (s *Store) learningBackgroundEvidenceTx(ctx context.Context, tx *sql.Tx, root, session string, m learningRootMember, members map[string]learningRootMember) (any, error) {
	parent, ok := members[m.Parent]
	if !ok {
		return nil, harness.ErrPermissionDenied
	}
	b, spec, _, _, err := readContinuationOrigin(ctx, tx, parent.Session, m.Run)
	if err != nil {
		return nil, err
	}
	if b.RootBudgetID != root || b.RootSession() != session || b.ChildSessionID != m.Session || b.OriginRunID != m.Parent || backgroundInputID(b) != m.Input || learningRootDigest(spec.Input) != m.InputSHA || (b.ContinuationOf != "") != (m.Kind == "background_continuation") {
		return nil, harness.ErrPermissionDenied
	}
	if spec.Continuation == nil && (b.ParentTaskID == "") != (parent.Kind == "run" || parent.Kind == "continuation") {
		return nil, harness.ErrPermissionDenied
	}
	if spec.Continuation == nil && b.ParentTaskID != "" && b.ParentTaskID != parent.Run {
		return nil, harness.ErrPermissionDenied
	}
	host := &BackgroundExecutionStore{store: s}
	if err = host.CheckParentPolicyTx(ctx, tx, b, spec); err != nil {
		return nil, err
	}
	if _, err = backgroundChildPolicy(ctx, tx, b, spec); err != nil {
		return nil, err
	}
	var treeRoot, treeParent string
	var treeDepth int
	if err = tx.QueryRowContext(ctx, `SELECT root_session_id,parent_task_id,depth FROM harness_background_tree WHERE task_id=?`, m.Run).Scan(&treeRoot, &treeParent, &treeDepth); err != nil {
		return nil, err
	}
	if treeRoot != b.RootSession() || treeParent != b.ParentTaskID || treeDepth != b.TaskDepth() {
		return nil, harness.ErrPermissionDenied
	}
	var taskBytes, bindingBytes, specBytes []byte
	if err = tx.QueryRowContext(ctx, `SELECT t.payload,b.payload,s.payload FROM eino_background_tasks t JOIN harness_background_bindings b ON b.task_id=t.id JOIN harness_background_specs s ON s.task_id=t.id WHERE t.id=?`, m.Run).Scan(&taskBytes, &bindingBytes, &specBytes); err != nil {
		return nil, err
	}
	if len(taskBytes) > 2<<20 || len(bindingBytes) > 32<<10 || len(specBytes) > 128<<10 || !json.Valid(taskBytes) || !json.Valid(bindingBytes) || !json.Valid(specBytes) {
		return nil, harness.ErrInvalidInput
	}
	task, err := readBackgroundNativeTask(ctx, tx, m.Run)
	if err != nil {
		return nil, err
	}
	if task.Spec.SessionID != parent.Session || task.Version < 1 || task.Attempt < 1 || string(task.Status) != m.Status || task.DoneAt == nil || task.Status != bt.StatusCompleted && task.Status != bt.StatusFailed {
		return nil, harness.ErrPermissionDenied
	}
	var settled, unsafe bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM budget_attempts WHERE root_id=? AND member_id=? AND session_id=? AND fence=? AND state='ended' AND outcome=?),
 EXISTS(SELECT 1 FROM harness_background_execution_failures WHERE task_id=? AND persistence_failure<>0)
 OR EXISTS(SELECT 1 FROM harness_background_stops WHERE task_id=?)
 OR EXISTS(SELECT 1 FROM harness_automatic_stops WHERE root_id=?)`, root, m.Run, m.Session, task.Attempt, m.Status, m.Run, m.Run, root).Scan(&settled, &unsafe); err != nil {
		return nil, err
	}
	if !settled || unsafe {
		return nil, harness.ErrPermissionDenied
	}
	if err = agents.CheckStoppedIfConfigured(ctx, tx, root, b.RoleID()); err != nil {
		return nil, err
	}
	var role any
	if spec.Role != nil {
		invocation, err := agents.ReadInvocation(ctx, tx, b.RoleID(), root)
		if err != nil {
			return nil, err
		}
		parentRole := b.ParentInvocationID
		if parentRole == "" && b.ParentTaskID != "" {
			parentRole = "background/" + b.ParentTaskID
		}
		if spec.Continuation != nil {
			parentRole = spec.ParentInvocationID
		}
		if invocation.Depth != b.TaskDepth() || invocation.ParentID != parentRole || learningRootDigest(invocation.Spec) != learningRootDigest(*spec.Role) {
			return nil, harness.ErrPermissionDenied
		}
		role = invocation
	}
	var origin any
	if spec.Continuation == nil {
		origin, err = learningDelegationReceiptTx(ctx, tx, b, spec)
	} else {
		origin, err = readBackgroundContinuationSourceTx(ctx, tx, b, spec)
	}
	if err != nil {
		return nil, err
	}
	return []any{b, spec, executionDigest(taskBytes), executionDigest(bindingBytes), executionDigest(specBytes), role, origin}, nil
}

func learningDelegationReceiptTx(ctx context.Context, tx *sql.Tx, b background.Binding, spec BackgroundExecutionSpec) (any, error) {
	r, err := readReceipt(ctx, tx, b.ParentSessionID, b.OriginRunID, b.OriginToolCallID)
	if err != nil {
		return nil, err
	}
	if r.State != harness.ReceiptCompleted || r.ToolName != "background_agent" || r.ConfigVersion != b.ConfigVersion || r.ArgumentsDigest != executionDigest([]byte(spec.OriginArguments)) {
		return nil, harness.ErrPermissionDenied
	}
	rows, err := tx.QueryContext(ctx, `SELECT event FROM harness_events WHERE run_id=? AND json_valid(CAST(event AS TEXT)) AND json_extract(CAST(event AS TEXT),'$.toolCallId')=? ORDER BY sequence LIMIT 513`, b.OriginRunID, b.OriginToolCallID)
	if err != nil {
		return nil, err
	}
	var started bool
	var end *harness.ToolReceipt
	var events, bytes int
	for rows.Next() {
		var data []byte
		var e harness.RunEvent
		if err = rows.Scan(&data); err != nil {
			break
		}
		events++
		bytes += len(data)
		if events > 512 || bytes > 4<<20 || len(data) > 2<<20 || json.Unmarshal(data, &e) != nil || e.SessionID != b.ParentSessionID || e.RunID != b.OriginRunID || e.ToolCallID != b.OriginToolCallID || e.ToolName != "background_agent" {
			err = harness.ErrPermissionDenied
			break
		}
		if e.Kind == "tool_start" {
			started = string(e.Arguments) == spec.OriginArguments
		}
		if e.Kind == "tool_end" {
			end = e.Receipt
		}
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if !started || end == nil || learningRootDigest(r) != learningRootDigest(*end) {
		return nil, harness.ErrPermissionDenied
	}
	return r, nil
}
