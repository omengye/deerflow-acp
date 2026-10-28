package learning

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type sourceRef struct {
	Outbox   int64  `json:"outbox"`
	Run      string `json:"run"`
	Root     string `json:"root"`
	Input    string `json:"input"`
	InputSHA string `json:"input_sha"`
	Accepted int64  `json:"accepted"`
	Frontier int64  `json:"frontier"`
	Epoch    int64  `json:"epoch"`
	Status   string `json:"status"`
	Ended    string `json:"ended_at"`
}

type extractionSnapshot struct {
	ID                 string                     `json:"id"`
	Session            string                     `json:"session"`
	Episode            string                     `json:"episode"`
	Owner              string                     `json:"owner"`
	Epoch              int64                      `json:"epoch"`
	MaxDailyModelCalls int64                      `json:"max_daily_model_calls"`
	MaxDailyTokens     int64                      `json:"max_daily_tokens"`
	Sources            []sourceRef                `json:"sources"`
	RootPolicy         string                     `json:"root_policy,omitempty"`
	Roots              []rootCapture              `json:"roots,omitempty"`
	View               harness.SkillLearningInput `json:"view"`
}

// ErrSourceBounds distinguishes bounded-capture rejection from invalid source
// provenance, including bounds enforced by the host's root evidence reader.
var ErrSourceBounds = errors.New("learning source exceeds extraction bounds")

func sourceBounds(message string) error {
	return errors.Join(ErrSourceBounds, invalid(message))
}

func settledRoot(ctx context.Context, tx *sql.Tx, root string) (bool, error) {
	var settled bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM budget_roots WHERE id=? AND blocked_reason='' AND held_tokens=0)
 AND NOT EXISTS(SELECT 1 FROM budget_members m LEFT JOIN harness_runs r ON r.id=m.id
 WHERE m.root_id=? AND (r.id IS NULL OR r.status NOT IN ('completed','failed') OR EXISTS(
 SELECT 1 FROM harness_tool_receipts t WHERE t.run_id=r.id AND t.state IN ('pending','started','uncertain'))))
 AND NOT EXISTS(SELECT 1 FROM budget_attempts WHERE root_id=? AND state<>'ended')
 AND NOT EXISTS(SELECT 1 FROM budget_reservations r WHERE r.root_id=? AND (
 r.state NOT IN ('settled','continued','cancelled')
 OR r.state IN ('continued','cancelled') AND (r.kind<>'tool' OR r.held_tokens<>0)
 OR r.state='continued' AND NOT EXISTS(SELECT 1 FROM budget_tool_continuations c JOIN budget_reservations child ON child.id=c.child_id
 WHERE c.parent_id=r.id AND child.root_id=r.root_id AND child.member_id=r.member_id AND child.session_id=r.session_id AND child.kind='tool' AND child.held_tokens=0)))`, root, root, root, root).Scan(&settled)
	return settled, err
}

func contentText(content []harness.Content) string {
	var text strings.Builder
	for _, c := range content {
		if c.Type == "text" {
			text.WriteString(c.Text)
			text.WriteByte('\n')
		}
	}
	return text.String()
}

func inputView(ctx context.Context, tx *sql.Tx, src sourceRef) ([]harness.SkillLearningReference, error) {
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT content FROM harness_inputs WHERE id=? AND run_id=?`, src.Input, src.Run).Scan(&raw); err != nil {
		return nil, err
	}
	var input []harness.Content
	if len(raw) > 2<<20 {
		return nil, sourceBounds("input exceeds extraction bounds")
	}
	if digest(raw) != src.InputSHA || json.Unmarshal(raw, &input) != nil {
		return nil, invalid("input provenance changed")
	}
	var accepted []byte
	if err := tx.QueryRowContext(ctx, `SELECT event FROM harness_events WHERE sequence=? AND run_id=?`, src.Accepted, src.Run).Scan(&accepted); err != nil {
		return nil, err
	}
	var user harness.RunEvent
	if json.Unmarshal(accepted, &user) != nil || user.Kind != "user_message" || user.RunID != src.Run || encodedDigest(user.Content) != src.InputSHA {
		return nil, invalid("accepted input provenance changed")
	}
	userText := contentText(input)
	refs := []harness.SkillLearningReference{{ID: "input/" + src.Input, RunID: src.Run, InputID: src.Input, Sequence: src.Accepted, Kind: "user", Text: redact(userText, 6000), Truncated: len(userText) > 6000}}
	observations, err := runObservationView(ctx, tx, src.Run, src.Accepted, src.Frontier, true)
	return append(refs, observations...), err
}

// Delegated runs expose only bounded receipts and actual reads, never private
// instructions, conversations, reasoning or native task-result bodies.
func runObservationView(ctx context.Context, tx *sql.Tx, run string, after, frontier int64, includeAnswer bool) ([]harness.SkillLearningReference, error) {
	refs := []harness.SkillLearningReference{}
	var session, root string
	if err := tx.QueryRowContext(ctx, `SELECT r.session_id,m.root_id FROM harness_runs r JOIN budget_members m ON m.id=r.id AND m.session_id=r.session_id WHERE r.id=?`, run).Scan(&session, &root); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT sequence,session_id,event FROM harness_events WHERE run_id=? AND sequence>? AND sequence<=? ORDER BY sequence LIMIT 513`, run, after, frontier)
	if err != nil {
		return nil, err
	}
	var answer strings.Builder
	var finalSequence int64
	var arguments = map[string]json.RawMessage{}
	var finalReceipts = map[string]harness.ToolReceipt{}
	var events int
	var sourceBytes int
	for rows.Next() {
		var seq int64
		var eventSession string
		var data []byte
		if err = rows.Scan(&seq, &eventSession, &data); err != nil {
			break
		}
		events++
		sourceBytes += len(data)
		if events > 512 || len(data) > 2<<20 || sourceBytes > 4<<20 {
			err = sourceBounds("episode events exceed extraction bounds")
			break
		}
		var e harness.RunEvent
		if json.Unmarshal(data, &e) != nil || e.RunID != run || e.SessionID != session || eventSession != session {
			err = invalid("invalid event provenance")
			break
		}
		if e.Kind == "tool_start" {
			answer.Reset()
			arguments[e.ToolCallID] = e.Arguments
		}
		if includeAnswer && e.Kind == "text_delta" && e.AgentName == "deerflow" {
			if answer.Len()+len(e.Text) > 128<<10 {
				err = sourceBounds("answer exceeds extraction bounds")
				break
			}
			answer.WriteString(e.Text)
			finalSequence = seq
		}
		if e.Kind == "tool_end" && e.Receipt != nil {
			finalReceipts[e.ToolCallID] = *e.Receipt
		}
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if answer.Len() > 0 {
		refs = append(refs, harness.SkillLearningReference{ID: fmt.Sprintf("event/%d", finalSequence), RunID: run, Sequence: finalSequence, Kind: "assistant", Text: redact(answer.String(), 6000), Truncated: answer.Len() > 6000})
	}
	rows, err = tx.QueryContext(ctx, `SELECT tool_call_id,session_id,version,receipt FROM harness_tool_receipts WHERE run_id=? ORDER BY rowid LIMIT 33`, run)
	if err != nil {
		return nil, err
	}
	var receipts int
	for rows.Next() {
		var call, receiptSession string
		var version int64
		var data []byte
		if err = rows.Scan(&call, &receiptSession, &version, &data); err != nil {
			break
		}
		receipts++
		sourceBytes += len(data)
		if receipts > 32 || sourceBytes > 4<<20 {
			err = sourceBounds("receipts exceed extraction bounds")
			break
		}
		var receipt harness.ToolReceipt
		if len(data) > 2<<20 {
			err = sourceBounds("receipt exceeds extraction bounds")
			break
		}
		if json.Unmarshal(data, &receipt) != nil || receipt.RunID != run || receipt.SessionID != session || receiptSession != session || receipt.ToolCallID != call || receipt.Version != version || encodedDigest(receipt) != encodedDigest(finalReceipts[call]) {
			err = invalid("receipt provenance changed")
			break
		}
		// Delegated instructions and skill-management text must not become a new
		// user learning source. External results remain marked as observations.
		if receipt.ToolName == "skill_manage" || receipt.ToolName == "task" || receipt.ToolName == "background_agent" {
			continue
		}
		args, found := arguments[call]
		if !found || digest(args) != receipt.ArgumentsDigest {
			err = invalid("receipt arguments changed")
			break
		}
		// Network/MCP/unknown tools provide metadata hints only. Their result
		// bodies cannot contribute instructions or spoof a user correction.
		text := "Argument shape: " + redact(receipt.ArgumentsSummary, 1000)
		truncated := receipt.ResultTruncated
		if localObservation(receipt.ToolName) {
			result := contentText(receipt.Result)
			truncated = truncated || len(args) > 2000 || len(result) > 3000
			text = "Arguments: " + redact(string(args), 2000) + "\nObserved result: " + redact(result, 3000)
		}
		refs = append(refs, harness.SkillLearningReference{ID: "receipt/" + run + "/" + call, RunID: run, ToolCallID: call, Version: version, Kind: "tool_observation", State: string(receipt.State), ToolName: receipt.ToolName, FailureCode: receipt.FailureCode, Text: text, Truncated: truncated})
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT id,session_id,root_id,role_id,source_id,source_key,name,version,path,file_hash FROM harness_skill_reads WHERE run_id=? ORDER BY id LIMIT 33`, run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	reads := 0
	for rows.Next() {
		var id int64
		var ref harness.SkillRef
		var readSession, readRoot, role, path, hash string
		if err := rows.Scan(&id, &readSession, &readRoot, &role, &ref.SourceID, &ref.SourceIdentity, &ref.Name, &ref.Version, &path, &hash); err != nil {
			return nil, err
		}
		reads++
		if reads > 32 {
			return nil, sourceBounds("skill reads exceed extraction bounds")
		}
		if readSession != session || readRoot != root || !harness.ValidSkillName(ref.SourceID) || !harness.ValidSkillName(ref.Name) {
			return nil, invalid("skill read provenance changed")
		}
		ref.Hash = ref.Version
		refs = append(refs, harness.SkillLearningReference{ID: fmt.Sprintf("skill_read/%d", id), RunID: run, Kind: "skill_read", Skill: &ref, Path: path, Hash: hash, RoleID: role})
	}
	return refs, rows.Err()
}

func localObservation(name string) bool {
	switch name {
	case "ls", "read_file", "list_directory", "search_files", "write_file", "edit_file", "str_replace", "glob", "grep", "delete_path", "move_path", "bash":
		return true
	default:
		return false
	}
}

func (s *Store) verifySnapshotTx(ctx context.Context, tx *sql.Tx, snap extractionSnapshot) error {
	return s.verifySnapshotEvidenceTx(ctx, tx, snap, true)
}

// Inspection may survive disabled paid work, never withdrawn consent or changed
// source bytes. Admission additionally requires the current enabled host gate.
func (s *Store) verifySnapshotEvidenceTx(ctx context.Context, tx *sql.Tx, snap extractionSnapshot, admission bool) error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed || admission && !s.enabled || snap.Session == "" || snap.Episode == "" || snap.View.EpisodeID != snap.Episode || len(snap.Sources) < 1 || len(snap.Sources) > 8 || len(snap.View.References) > 528 {
		return harness.ErrPermissionDenied
	}
	var epoch int64
	var contribute, enabled bool
	var policy, workspace string
	if err := tx.QueryRowContext(ctx, `SELECT c.epoch,c.contribute,h.enabled,h.policy_version,e.workspace_key FROM harness_learning_controls c
 JOIN harness_learning_host h ON h.id=1 JOIN df_learning_episodes e ON e.session_id=c.session_id AND e.id=? WHERE c.session_id=?`, snap.Episode, snap.Session).Scan(&epoch, &contribute, &enabled, &policy, &workspace); err != nil {
		return err
	}
	if epoch != snap.Epoch || !contribute || admission && !enabled || policy != PolicyVersion || workspace != snap.Owner {
		return harness.ErrPermissionDenied
	}
	view := harness.SkillLearningInput{EpisodeID: snap.Episode, References: []harness.SkillLearningReference{}}
	var prior int64
	for _, src := range snap.Sources {
		if src.Outbox <= prior {
			return harness.ErrPermissionDenied
		}
		prior = src.Outbox
		refs, err := verifiedSourceViewTx(ctx, tx, src, snap.Session, snap.Episode, snap.Epoch)
		if err != nil {
			return err
		}
		view.References = append(view.References, refs...)
	}
	if err := s.verifyRootCaptureTx(ctx, tx, snap, &view); err != nil {
		return err
	}
	// Reconstruct the exact redacted input, not just the input IDs: changed
	// receipts, skill reads, event frontiers and text all invalidate results.
	if encodedDigest(view) != encodedDigest(snap.View) {
		return harness.ErrPermissionDenied
	}
	if admission {
		active, err := episodeTailActiveTx(ctx, tx, snap.Episode, snap.Epoch)
		if err != nil {
			return err
		}
		if active {
			return sourceActiveTx(ctx, tx)
		}
	}
	return nil
}

func verifiedSourceViewTx(ctx context.Context, tx *sql.Tx, src sourceRef, session, episode string, epoch int64) ([]harness.SkillLearningReference, error) {
	var live sourceRef
	var uncertain bool
	err := tx.QueryRowContext(ctx, `SELECT o.sequence,i.run_id,i.root_id,i.input_id,i.input_sha,i.accepted_sequence,o.frontier,o.epoch,o.status,o.ended_at,o.effects_uncertain
 FROM harness_learning_outbox o JOIN harness_learning_inputs i ON i.run_id=o.run_id
 JOIN harness_runs r ON r.id=i.run_id WHERE o.sequence=? AND i.session_id=? AND i.episode_id=? AND r.status=o.status AND r.updated_at=o.ended_at AND i.epoch=o.epoch
 AND i.policy_version=? AND o.frontier=(SELECT MAX(sequence) FROM harness_events WHERE run_id=i.run_id)`, src.Outbox, session, episode, PolicyVersion).Scan(&live.Outbox, &live.Run, &live.Root, &live.Input, &live.InputSHA, &live.Accepted, &live.Frontier, &live.Epoch, &live.Status, &live.Ended, &uncertain)
	if err != nil {
		return nil, err
	}
	if live != src || live.Epoch != epoch || uncertain || live.Status == "cancelled" {
		return nil, harness.ErrPermissionDenied
	}
	settled, err := settledRoot(ctx, tx, src.Root)
	if err != nil {
		return nil, err
	}
	if !settled {
		return nil, harness.ErrPermissionDenied
	}
	return inputView(ctx, tx, src)
}
