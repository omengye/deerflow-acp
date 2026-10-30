package runtime

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/assets"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

type ConversationTurn struct {
	TurnID         string `json:"turnId"`
	EventID        string `json:"eventId"`
	nativeSequence int64
	eventSequence  int64
	runID          string
}
type ConversationState struct {
	SessionID    string             `json:"sessionId"`
	HeadEventID  string             `json:"headEventId"`
	Turns        []ConversationTurn `json:"turns"`
	ReferenceMap map[string]string  `json:"referenceMap,omitempty"`
}
type ConversationBranchRequest struct {
	SessionID           string `json:"sessionId"`
	ExpectedHeadEventID string `json:"expectedHeadEventId"`
	TargetEventID       string `json:"targetEventId"`
	OperationID         string `json:"operationId"`
}

func migrateConversation(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS harness_conversation_turns (
 session_id TEXT NOT NULL REFERENCES harness_sessions(id) ON DELETE CASCADE,
 event_id TEXT NOT NULL,turn_id TEXT NOT NULL,run_id TEXT NOT NULL,
 native_sequence INTEGER NOT NULL,event_sequence INTEGER NOT NULL,ordinal INTEGER NOT NULL,active INTEGER NOT NULL DEFAULT 1,
 PRIMARY KEY(session_id,event_id));
CREATE INDEX IF NOT EXISTS harness_conversation_turn_order ON harness_conversation_turns(session_id,active,ordinal);
CREATE TABLE IF NOT EXISTS harness_conversation_hidden_events (
 session_id TEXT NOT NULL REFERENCES harness_sessions(id) ON DELETE CASCADE, first_sequence INTEGER NOT NULL,last_sequence INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS harness_conversation_operations (
 session_id TEXT NOT NULL REFERENCES harness_sessions(id) ON DELETE CASCADE,operation_id TEXT NOT NULL,
 kind TEXT NOT NULL,request BLOB NOT NULL,result BLOB NOT NULL,created_at TEXT NOT NULL,
 PRIMARY KEY(session_id,operation_id));`)
	return err
}

// Capture only after all native writes have joined. Audit events and spending
// stay in their original tables; this is an immutable model/display boundary.
func captureConversationTurnTx(ctx context.Context, tx *sql.Tx, runID string) error {
	var sessionID string
	if err := tx.QueryRowContext(ctx, `SELECT session_id FROM harness_runs WHERE id=?`, runID).Scan(&sessionID); err != nil {
		return err
	}
	var seq int64
	var payload []byte
	err := tx.QueryRowContext(ctx, `SELECT seq,payload FROM eino_session_events WHERE session_id=? ORDER BY seq DESC LIMIT 1`, sessionID).Scan(&seq, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	event, err := sqlite.DecodeSessionEvent(payload)
	if err != nil {
		return err
	}
	if !completedNativeTurn(event) {
		return nil
	}
	// An upgraded conversation can finish a new run before the first UI
	// boundary query. Recover all verifiable legacy turns before inserting the
	// latest one, otherwise that single row would suppress future backfill.
	// Ambiguous old history must not prevent an otherwise successful run from
	// finishing, and no partial backfill may leak into its transaction.
	if _, err = tx.ExecContext(ctx, `SAVEPOINT capture_conversation_backfill`); err != nil {
		return err
	}
	if backfillErr := backfillConversationTx(ctx, tx, sessionID); backfillErr != nil {
		if _, err = tx.ExecContext(ctx, `ROLLBACK TO capture_conversation_backfill`); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `RELEASE capture_conversation_backfill`); err != nil {
		return err
	}
	var cursor int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0) FROM harness_events WHERE session_id=?`, sessionID).Scan(&cursor); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO harness_conversation_turns(session_id,event_id,turn_id,run_id,native_sequence,event_sequence,ordinal) VALUES(?,?,?,?,?,?,(SELECT COALESCE(MAX(ordinal),0)+1 FROM harness_conversation_turns WHERE session_id=? AND active=1))`, sessionID, event.EventID, event.TurnID, runID, seq, cursor, sessionID)
	return err
}

func completedNativeTurn(event *adk.SessionEvent[*schema.Message]) bool {
	return event != nil && event.Kind == adk.SessionEventSessionStatusIdle && event.TurnID != "" && event.Lifecycle != nil && event.Lifecycle.State == adk.SessionRunStateIdle && event.Lifecycle.StopReason != nil && event.Lifecycle.StopReason.Type == adk.StopReasonEndTurn
}

func conversationSafeTx(ctx context.Context, tx *sql.Tx, id string) error {
	var blocked bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_runs WHERE session_id=? AND status IN ('running','waiting_input','needs_reconciliation')) OR EXISTS(SELECT 1 FROM harness_tool_receipts WHERE session_id=? AND state IN ('pending','started','uncertain'))`, id, id).Scan(&blocked)
	if err != nil {
		return err
	}
	if blocked {
		return harness.ErrBusy
	}
	tables, err := sessionTables(ctx, tx)
	if err != nil {
		return err
	}
	_, _, err = prepareBackgroundGraphTx(ctx, tx, id, tables, true)
	return err
}

func conversationStateTx(ctx context.Context, tx *sql.Tx, id string) (ConversationState, error) {
	state := ConversationState{SessionID: id, Turns: []ConversationTurn{}}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT event_id FROM eino_session_events WHERE session_id=? ORDER BY seq DESC LIMIT 1),'')`, id).Scan(&state.HeadEventID); err != nil {
		return state, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT turn_id,event_id,native_sequence,event_sequence,run_id FROM harness_conversation_turns WHERE session_id=? AND active=1 ORDER BY ordinal`, id)
	if err != nil {
		return state, err
	}
	for rows.Next() {
		var turn ConversationTurn
		if err = rows.Scan(&turn.TurnID, &turn.EventID, &turn.nativeSequence, &turn.eventSequence, &turn.runID); err != nil {
			break
		}
		state.Turns = append(state.Turns, turn)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return state, err
	}
	return state, nil
}

// Legacy histories are admitted only when every native completed turn maps
// unambiguously to one completed foreground input. No timestamp guessing.
func backfillConversationTx(ctx context.Context, tx *sql.Tx, id string) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM harness_conversation_turns WHERE session_id=?`, id).Scan(&count); err != nil || count != 0 {
		return err
	}
	var operations int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM harness_conversation_operations WHERE session_id=?`, id).Scan(&operations); err != nil || operations != 0 {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT seq,payload FROM eino_session_events WHERE session_id=? ORDER BY seq`, id)
	if err != nil {
		return err
	}
	var native []ConversationTurn
	legacy := false
	for rows.Next() {
		var seq int64
		var payload []byte
		if err = rows.Scan(&seq, &payload); err != nil {
			break
		}
		event, e := sqlite.DecodeSessionEvent(payload)
		if e != nil {
			err = e
			break
		}
		if event.Kind == adk.SessionEventRollback {
			legacy = true
		}
		if completedNativeTurn(event) {
			native = append(native, ConversationTurn{TurnID: event.TurnID, EventID: event.EventID, nativeSequence: seq})
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	if len(native) == 0 {
		return nil
	}
	rows, err = tx.QueryContext(ctx, `SELECT r.id,r.status,MIN(e.sequence),(SELECT MAX(h.sequence) FROM harness_events h WHERE h.run_id=r.id AND h.session_id=r.session_id) FROM harness_runs r JOIN harness_events e ON e.run_id=r.id AND e.session_id=r.session_id WHERE r.session_id=? GROUP BY r.id ORDER BY MIN(e.sequence)`, id)
	if err != nil {
		return err
	}
	type run struct {
		id, status  string
		first, last int64
	}
	var runs []run
	for rows.Next() {
		var r run
		if err = rows.Scan(&r.id, &r.status, &r.first, &r.last); err != nil {
			break
		}
		runs = append(runs, r)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	if legacy || len(runs) != len(native) {
		return fmt.Errorf("%w: historical turn boundaries cannot be verified", harness.ErrExecutionConflict)
	}
	for i, r := range runs {
		if r.status != "completed" {
			return fmt.Errorf("%w: historical turn did not complete", harness.ErrExecutionConflict)
		}
		n := native[i]
		if _, err = tx.ExecContext(ctx, `INSERT INTO harness_conversation_turns(session_id,event_id,turn_id,run_id,native_sequence,event_sequence,ordinal) VALUES(?,?,?,?,?,?,?)`, id, n.EventID, n.TurnID, r.id, n.nativeSequence, r.last, i+1); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) ConversationTurns(ctx context.Context, owner, id string) (ConversationState, error) {
	ctx, release, err := s.Coordinator.Begin(ctx, id, owner)
	if err != nil {
		return ConversationState{}, err
	}
	defer release()
	if err = s.Store.requireForegroundSession(ctx, id); err != nil {
		return ConversationState{}, err
	}
	tx, err := s.Store.db.BeginTx(ctx, nil)
	if err != nil {
		return ConversationState{}, err
	}
	defer tx.Rollback()
	if err = conversationSafeTx(ctx, tx, id); err != nil {
		return ConversationState{}, err
	}
	if err = backfillConversationTx(ctx, tx, id); err != nil {
		return ConversationState{}, err
	}
	result, err := conversationStateTx(ctx, tx, id)
	if err != nil {
		return result, err
	}
	return result, tx.Commit()
}

type conversationPayload struct {
	sequence             int64
	eventID, kind, runID string
	data                 []byte
}

func readConversationPayloads(ctx context.Context, tx *sql.Tx, id string, nativeThrough, eventThrough int64) ([]conversationPayload, []conversationPayload, error) {
	rows, err := tx.QueryContext(ctx, `SELECT seq,event_id,kind,payload FROM eino_session_events WHERE session_id=? AND seq<=? ORDER BY seq`, id, nativeThrough)
	if err != nil {
		return nil, nil, err
	}
	var native, business []conversationPayload
	for rows.Next() {
		var p conversationPayload
		if err = rows.Scan(&p.sequence, &p.eventID, &p.kind, &p.data); err != nil {
			break
		}
		native = append(native, p)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT sequence,run_id,event FROM harness_events e WHERE session_id=? AND sequence<=? AND NOT EXISTS(SELECT 1 FROM harness_conversation_hidden_events h WHERE h.session_id=e.session_id AND e.sequence BETWEEN h.first_sequence AND h.last_sequence) ORDER BY sequence`, id, eventThrough)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var p conversationPayload
		if err = rows.Scan(&p.sequence, &p.runID, &p.data); err != nil {
			break
		}
		business = append(business, p)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	return native, business, err
}

// Rewrite structured asset metadata, embedded JSON metadata, and validated
// asset URIs without replacing arbitrary source IDs in user prose.
func remapConversationAssets(data []byte, source, target string, mapping map[string]harness.AssetRef) ([]byte, error) {
	var root any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return nil, err
	}
	var visit func(any) any
	visit = func(value any) any {
		switch v := value.(type) {
		case map[string]any:
			// Historical built-in reader calls carry opaque IDs in their JSON
			// arguments. Rebind only these exact native read operations.
			if name, _ := v["name"].(string); name == "read_tool_output" || name == "read_attachment" || name == "analyze_attachment" {
				if raw, ok := v["arguments"].(string); ok {
					var args map[string]json.RawMessage
					var id string
					if json.Unmarshal([]byte(raw), &args) == nil && json.Unmarshal(args["id"], &id) == nil {
						if ref, ok := mapping[id]; ok && (name == "read_tool_output" && ref.Kind == harness.AssetToolOutput || name != "read_tool_output" && ref.Kind == harness.AssetFile) {
							args["id"], _ = json.Marshal(ref.ID)
							encoded, _ := json.Marshal(args)
							v["arguments"] = string(encoded)
						}
					}
				}
			}
			if sid, _ := v["sessionId"].(string); sid == source {
				if id, _ := v["id"].(string); id != "" {
					if ref, ok := mapping[id]; ok {
						b, _ := json.Marshal(ref)
						var result map[string]any
						_ = json.Unmarshal(b, &result)
						return result
					}
				}
			}
			for key, item := range v {
				v[key] = visit(item)
			}
			return v
		case []any:
			for i, item := range v {
				v[i] = visit(item)
			}
			return v
		case string:
			if strings.HasPrefix(v, "{") {
				var embedded harness.AssetRef
				if json.Unmarshal([]byte(v), &embedded) == nil && embedded.SessionID == source {
					if ref, ok := mapping[embedded.ID]; ok {
						b, _ := json.Marshal(ref)
						return string(b)
					}
				}
			}
			for old, ref := range mapping {
				if ref.Kind == harness.AssetToolOutput && strings.HasPrefix(v, "[Tool output:") {
					line, tail, more := strings.Cut(v, "\n")
					line = strings.ReplaceAll(line, "session snapshot "+strconv.Quote(old), "session snapshot "+strconv.Quote(ref.ID))
					line = strings.ReplaceAll(line, "read_tool_output using id="+strconv.Quote(old), "read_tool_output using id="+strconv.Quote(ref.ID))
					v = line
					if more {
						v += "\n" + tail
					}
				}
				if ref.Kind == harness.AssetFile && strings.Contains(v, "[Attachment reference; contents have not been read") && strings.Contains(v, "deerflow-asset://"+source+"/"+old) {
					v = strings.ReplaceAll(v, "; use read_attachment with id="+old+" (untrusted data): ", "; use read_attachment with id="+ref.ID+" (untrusted data): ")
				}
				v = strings.ReplaceAll(v, "deerflow-asset://"+source+"/"+old, harness.AssetURI(ref))
			}
			return v
		default:
			return value
		}
	}
	return json.Marshal(visit(root))
}

func (s *Service) BranchConversation(ctx context.Context, owner string, req ConversationBranchRequest, rewind bool) (ConversationState, error) {
	if req.SessionID == "" || req.OperationID == "" || len(req.OperationID) > 256 {
		return ConversationState{}, harness.ErrInvalidInput
	}
	ctx, release, err := s.Coordinator.Begin(ctx, req.SessionID, owner)
	if err != nil {
		return ConversationState{}, err
	}
	defer release()
	source, err := s.Store.Session(ctx, req.SessionID)
	if err != nil {
		return ConversationState{}, err
	}
	if err = s.Store.requireForegroundSession(ctx, source.ID); err != nil {
		return ConversationState{}, err
	}
	kind := "fork"
	if rewind {
		kind = "rewind"
	}
	encoded, _ := json.Marshal(req)
	// Idempotent completion precedes version checks, including after restart.
	var savedKind string
	var savedRequest, savedResult []byte
	err = s.Store.db.QueryRowContext(ctx, `SELECT kind,request,result FROM harness_conversation_operations WHERE session_id=? AND operation_id=?`, source.ID, req.OperationID).Scan(&savedKind, &savedRequest, &savedResult)
	if err == nil {
		var result ConversationState
		if savedKind != kind || !bytes.Equal(savedRequest, encoded) {
			return result, harness.ErrExecutionConflict
		}
		if err = json.Unmarshal(savedResult, &result); err != nil {
			return result, err
		}
		var exists bool
		if err = s.Store.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_sessions WHERE id=?)`, result.SessionID).Scan(&exists); err != nil {
			return ConversationState{}, err
		}
		if !exists {
			// Retain the operation tombstone: retrying must not recreate a
			// deleted branch or return an identity that cannot be loaded.
			return ConversationState{}, harness.ErrNotFound
		}
		return result, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ConversationState{}, err
	}
	tx, err := s.Store.db.BeginTx(ctx, nil)
	if err != nil {
		return ConversationState{}, err
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()
	if err = conversationSafeTx(ctx, tx, source.ID); err != nil {
		return ConversationState{}, err
	}
	if err = backfillConversationTx(ctx, tx, source.ID); err != nil {
		return ConversationState{}, err
	}
	state, err := conversationStateTx(ctx, tx, source.ID)
	if err != nil {
		return state, err
	}
	if state.HeadEventID != req.ExpectedHeadEventID {
		return state, harness.ErrExecutionConflict
	}
	retained := 0
	var nativeThrough, eventThrough int64
	if req.TargetEventID != "" {
		found := false
		for i, t := range state.Turns {
			if t.EventID == req.TargetEventID {
				retained = i + 1
				nativeThrough = t.nativeSequence
				eventThrough = t.eventSequence
				found = true
				break
			}
		}
		if !found {
			return state, harness.ErrExecutionConflict
		}
	}
	if rewind {
		result, err := s.rewindConversationTx(ctx, tx, req, encoded, state, retained, eventThrough)
		if err == nil {
			s.clearDecisions(owner, req.SessionID)
		}
		return result, err
	}
	native, business, err := readConversationPayloads(ctx, tx, source.ID, nativeThrough, eventThrough)
	if err != nil {
		return state, err
	}
	// Release SQL while copying files. Coordinator still fences the source;
	// recheck the physical head and full graph before committing the new owner.
	if err = tx.Commit(); err != nil {
		return state, err
	}
	target := source
	target.ID = NewID()
	target.CreatedAt = time.Now().UTC()
	target.UpdatedAt = target.CreatedAt
	var prepared *assets.Prepared
	mapping := map[string]harness.AssetRef{}
	if s.Assets != nil {
		payloads := make([][]byte, 0, len(native)+len(business))
		for _, p := range native {
			payloads = append(payloads, p.data)
		}
		for _, p := range business {
			payloads = append(payloads, p.data)
		}
		prepared, mapping, err = s.Assets.PrepareFork(ctx, source.ID, target, payloads)
		if err != nil {
			return state, err
		}
		defer func() {
			// Finish checks durable ownership on the single SQLite connection.
			// Release any failed transaction before that orphan check.
			if tx != nil {
				_ = tx.Rollback()
			}
			_ = prepared.Finish(false)
		}()
	}
	tx, err = s.Store.db.BeginTx(ctx, nil)
	if err != nil {
		return state, err
	}
	if err = conversationSafeTx(ctx, tx, source.ID); err != nil {
		return state, err
	}
	current, err := conversationStateTx(ctx, tx, source.ID)
	if err != nil {
		return state, err
	}
	if current.HeadEventID != state.HeadEventID {
		return state, harness.ErrExecutionConflict
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO harness_sessions(id,cwd,title,mode,model,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, target.ID, target.CWD, target.Title, target.Mode, target.Model, timestamp(), timestamp()); err != nil {
		return state, err
	}
	// Copy persisted configuration columns, but no approval grants or runtime
	// decisions. New conversation starts in explicit Ask mode.
	if err = copyConversationConfigTx(ctx, tx, source.ID, target.ID); err != nil {
		return state, err
	}
	if prepared != nil {
		if err = prepared.AttachFork(ctx, tx); err != nil {
			return state, err
		}
	}
	for old, ref := range mapping {
		if _, err = tx.ExecContext(ctx, `INSERT INTO harness_document_text(asset_id,sha256,version,text) SELECT ?,sha256,version,text FROM harness_document_text WHERE asset_id=?`, ref.ID, old); err != nil {
			return state, err
		}
	}
	nativePositions := map[string]int64{}
	eventPositions := map[int64]int64{}
	for _, p := range native {
		data, e := remapConversationAssets(p.data, source.ID, target.ID, mapping)
		if e != nil {
			return state, e
		}
		res, e := tx.ExecContext(ctx, `INSERT INTO eino_session_events(session_id,event_id,kind,payload) VALUES(?,?,?,?)`, target.ID, p.eventID, p.kind, data)
		if e != nil {
			return state, e
		}
		nativePositions[p.eventID], err = res.LastInsertId()
		if err != nil {
			return state, err
		}
	}
	for _, p := range business {
		data, e := remapConversationAssets(p.data, source.ID, target.ID, mapping)
		if e != nil {
			return state, e
		}
		var event harness.RunEvent
		if e = json.Unmarshal(data, &event); e != nil {
			return state, e
		}
		event.SessionID = target.ID
		event.Sequence = 0
		data, e = json.Marshal(event)
		if e != nil {
			return state, e
		}
		res, e := tx.ExecContext(ctx, `INSERT INTO harness_events(session_id,run_id,event) VALUES(?,?,?)`, target.ID, p.runID, data)
		if e != nil {
			return state, e
		}
		eventPositions[p.sequence], err = res.LastInsertId()
		if err != nil {
			return state, err
		}
	}
	for i, t := range state.Turns[:retained] {
		if _, err = tx.ExecContext(ctx, `INSERT INTO harness_conversation_turns(session_id,event_id,turn_id,run_id,native_sequence,event_sequence,ordinal) VALUES(?,?,?,?,?,?,?)`, target.ID, t.EventID, t.TurnID, t.runID, nativePositions[t.EventID], eventPositions[t.eventSequence], i+1); err != nil {
			return state, err
		}
	}
	result, err := conversationStateTx(ctx, tx, target.ID)
	if err != nil {
		return state, err
	}
	if prepared != nil {
		result.ReferenceMap = prepared.ForkReferences()
	}
	if err = saveConversationOperation(ctx, tx, req, "fork", encoded, result); err != nil {
		return state, err
	}
	if err = tx.Commit(); err != nil {
		return state, err
	}
	if prepared != nil {
		_ = prepared.Finish(true)
	}
	return result, nil
}

func copyConversationConfigTx(ctx context.Context, tx *sql.Tx, source, target string) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(harness_session_configs)`)
	if err != nil {
		return err
	}
	var columns []string
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var def any
		if err = rows.Scan(&cid, &name, &typ, &notnull, &def, &pk); err != nil {
			break
		}
		columns = append(columns, `"`+strings.ReplaceAll(name, `"`, `""`)+`"`)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	selectors := append([]string(nil), columns...)
	for i, col := range columns {
		switch col {
		case `"session_id"`:
			selectors[i] = "?"
		case `"approval_mode"`:
			selectors[i] = "'ask'"
		case `"version"`:
			selectors[i] = "1"
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO harness_session_configs(`+strings.Join(columns, ",")+`) SELECT `+strings.Join(selectors, ",")+` FROM harness_session_configs WHERE session_id=?`, target, source)
	return err
}

func saveConversationOperation(ctx context.Context, tx *sql.Tx, req ConversationBranchRequest, kind string, request []byte, result ConversationState) error {
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO harness_conversation_operations(session_id,operation_id,kind,request,result,created_at) VALUES(?,?,?,?,?,?)`, req.SessionID, req.OperationID, kind, request, data, timestamp())
	return err
}

func (s *Service) rewindConversationTx(ctx context.Context, tx *sql.Tx, req ConversationBranchRequest, encoded []byte, state ConversationState, retained int, eventThrough int64) (ConversationState, error) {
	var events []*adk.SessionEvent[*schema.Message]
	if retained == 0 {
		empty := []*schema.Message{}
		events = append(events, &adk.SessionEvent[*schema.Message]{EventID: NewID(), Kind: adk.SessionEventMessagesReplaced, MessagesReplaced: &empty, Timestamp: time.Now().UTC()})
	} else {
		events = append(events, &adk.SessionEvent[*schema.Message]{EventID: NewID(), Kind: adk.SessionEventRollback, Rollback: &adk.SessionRollbackEvent{ToEventID: req.TargetEventID, PreviousHeadCommitEventID: state.Turns[len(state.Turns)-1].EventID}, Timestamp: time.Now().UTC()})
	}
	for _, event := range events {
		data, err := sqlite.EncodeSessionEvent(event)
		if err != nil {
			return state, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO eino_session_events(session_id,event_id,kind,payload) VALUES(?,?,?,?)`, req.SessionID, event.EventID, string(event.Kind), data); err != nil {
			return state, err
		}
	}
	var head int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0) FROM harness_events WHERE session_id=?`, req.SessionID).Scan(&head); err != nil {
		return state, err
	}
	if head > eventThrough {
		if _, err := tx.ExecContext(ctx, `INSERT INTO harness_conversation_hidden_events VALUES(?,?,?)`, req.SessionID, eventThrough+1, head); err != nil {
			return state, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE harness_conversation_turns SET active=0 WHERE session_id=? AND active=1 AND ordinal>?`, req.SessionID, retained); err != nil {
		return state, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE harness_sessions SET updated_at=? WHERE id=?`, timestamp(), req.SessionID); err != nil {
		return state, err
	}
	result, err := conversationStateTx(ctx, tx, req.SessionID)
	if err != nil {
		return state, err
	}
	if err = saveConversationOperation(ctx, tx, req, "rewind", encoded, result); err != nil {
		return state, err
	}
	if err = tx.Commit(); err != nil {
		return state, err
	}
	return result, nil
}
