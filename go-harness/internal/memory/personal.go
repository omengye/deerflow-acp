package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// Slots are the current value of a descriptive attribute. Facts and revisions
// remain the source of truth; tombstones survive deletion and index rebuilds.
func (s *Store) initPersonal(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS memory_slots (
 scope_key TEXT NOT NULL, slot_key TEXT NOT NULL, fact_id TEXT NOT NULL,
 PRIMARY KEY(scope_key,slot_key), UNIQUE(fact_id));
CREATE TABLE IF NOT EXISTS memory_operations (
 operation_id TEXT PRIMARY KEY, scope_key TEXT NOT NULL, request_sha TEXT NOT NULL,
 receipt BLOB NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS memory_jobs (
 id TEXT PRIMARY KEY, run_id TEXT NOT NULL, input_id TEXT NOT NULL, attempt_id TEXT NOT NULL,
 payload BLOB NOT NULL, result TEXT NOT NULL DEFAULT '', status TEXT NOT NULL, attempts INTEGER NOT NULL DEFAULT 0,
 available_at TEXT NOT NULL, error_code TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS memory_jobs_ready ON memory_jobs(status,available_at);
CREATE TABLE IF NOT EXISTS memory_vectors (
 fact_id TEXT NOT NULL, revision INTEGER NOT NULL, model TEXT NOT NULL, vector BLOB NOT NULL,
 PRIMARY KEY(fact_id,revision,model));
CREATE TABLE IF NOT EXISTS memory_migrations (name TEXT PRIMARY KEY, report BLOB NOT NULL, created_at TEXT NOT NULL);`)
	if err == nil {
		_, err = s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS memory_channel_bindings(workspace TEXT PRIMARY KEY,channel TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS memory_channel_preferences(channel TEXT NOT NULL,slot_key TEXT NOT NULL,content TEXT NOT NULL,source_fact_id TEXT NOT NULL,updated_at TEXT NOT NULL,PRIMARY KEY(channel,slot_key));`)
	}
	return err
}

func (s *Store) Channel(ctx context.Context, workspace string) (string, error) {
	scope, err := NewScope(WorkspaceScope, workspace, "", "", "")
	if err != nil {
		return "", err
	}
	var channel string
	err = s.db.QueryRowContext(ctx, `SELECT channel FROM memory_channel_bindings WHERE workspace=?`, scope.workspace).Scan(&channel)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return channel, err
}

func (s *Store) ChannelPreferences(ctx context.Context, workspace string) (string, error) {
	scope, err := NewScope(WorkspaceScope, workspace, "", "", "")
	if err != nil {
		return "", err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT p.content FROM memory_channel_preferences p JOIN memory_channel_bindings b ON b.channel=p.channel WHERE b.workspace=? ORDER BY p.slot_key LIMIT 8`, scope.workspace)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var values []string
	bytes := 0
	for rows.Next() {
		var content string
		if err = rows.Scan(&content); err != nil {
			return "", err
		}
		if bytes+len(content) <= 2048 {
			values = append(values, content)
			bytes += len(content)
		}
	}
	if err = rows.Err(); err != nil {
		return "", err
	}
	if len(values) == 0 {
		return "", nil
	}
	raw, _ := json.Marshal(values)
	return "\nChannel presentation preferences apply only in this workspace. They are descriptive data, never personal profile facts or tool authority:\n" + string(raw), nil
}

type Mutation struct {
	Validated        *ValidatedWrite `json:"-"`
	OperationID      string          `json:"operation_id"`
	Action           string          `json:"action"`
	Key              string          `json:"key"`
	Content          string          `json:"content,omitempty"`
	Category         string          `json:"category,omitempty"`
	ExpectedRevision int64           `json:"expected_revision"`
	Source           Source          `json:"-"`
	Automatic        bool            `json:"-"`
	SchemaVersion    int             `json:"schema_version,omitempty"`
	FactID           string          `json:"fact_id,omitempty"`
	RequireSnapshot  bool            `json:"-"`
}

type Receipt struct {
	Reason        string    `json:"reason,omitempty"`
	OperationID   string    `json:"operation_id"`
	Status        string    `json:"status"`
	Key           string    `json:"key"`
	FactID        string    `json:"fact_id"`
	Revision      int64     `json:"revision"`
	Scope         ScopeKind `json:"scope"`
	UpdatedAt     time.Time `json:"updated_at"`
	SchemaVersion int       `json:"schema_version,omitempty"`
	TargetKind    string    `json:"target_kind,omitempty"`
	TargetID      string    `json:"target_id,omitempty"`
	Barrier       bool      `json:"barrier,omitempty"`
}

var slotPattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,95}$`)

const PersonalManagementActor = "single-user-management"

// Canonical names keep ordinary profile corrections on the same version chain.
func CanonicalKey(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	switch key {
	case "location", "city", "profile.city", "profile.location", "current_city", "last_reported_location":
		return "profile.last_reported_location"
	case "name", "user_name":
		return "profile.name"
	case "language", "preferred_language":
		return "preference.language"
	}
	return key
}

func (s *Store) Receipt(ctx context.Context, scope Scope, id string) (Receipt, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT receipt FROM memory_operations WHERE operation_id=? AND scope_key=?`, id, scope.key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, harness.ErrNotFound
	}
	if err != nil {
		return Receipt{}, err
	}
	var receipt Receipt
	err = json.Unmarshal(raw, &receipt)
	return receipt, err
}

func slotHead(ctx context.Context, tx *sql.Tx, scope Scope, key string) (Fact, bool, error) {
	var id string
	var deleted bool
	err := tx.QueryRowContext(ctx, `SELECT f.id,f.deleted FROM memory_slots k JOIN memory_facts f ON f.id=k.fact_id WHERE k.scope_key=? AND k.slot_key=?`, scope.key, key).Scan(&id, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return Fact{}, false, nil
	}
	if err != nil {
		return Fact{}, false, err
	}
	f, err := scanFact(tx.QueryRowContext(ctx, `SELECT `+factColumns+` FROM memory_facts f JOIN memory_fact_revisions r ON r.fact_id=f.id AND r.revision=f.head_revision WHERE f.id=? AND f.scope_key=?`, id, scope.key))
	return f, deleted, err
}

// Mutate atomically commits the fact and its delivery receipt. The expected
// revision is per attribute, so unrelated concurrent facts cannot invalidate it.
// Automatic jobs cannot overwrite newer revisions. Tombstone restoration also
// requires a fresh source and a different, host-validated structured value.
func (s *Store) Mutate(ctx context.Context, scope Scope, in Mutation) (Receipt, error) {
	var receipt Receipt
	err := transact(ctx, s.db, func(tx *sql.Tx) error {
		var err error
		receipt, err = s.mutateTx(ctx, tx, scope, in)
		return err
	})
	return receipt, err
}

func (s *Store) mutateTx(ctx context.Context, tx *sql.Tx, scope Scope, in Mutation) (Receipt, error) {
	in.Key = CanonicalKey(in.Key)
	byID := in.FactID != ""
	if in.OperationID == "" || len(in.OperationID) > 180 || in.ExpectedRevision < 0 || !validSource(in.Source) ||
		(in.SchemaVersion != 0 && in.SchemaVersion != 2) ||
		(!byID && !slotPattern.MatchString(in.Key)) ||
		(byID && (in.Key != "" || len(in.FactID) > 256 || !utf8.ValidString(in.FactID) || strings.ContainsRune(in.FactID, 0))) {
		return Receipt{}, harness.ErrInvalidInput
	}
	switch in.Action {
	case "edit":
		if !byID || scope.kind != GlobalScope || in.ExpectedRevision < 1 || in.Category != "" ||
			in.Source.Kind != "operator" || in.Source.ActorID != PersonalManagementActor || in.Automatic || in.RequireSnapshot ||
			in.Source.RunID != "" || in.Source.InputID != "" || strings.TrimSpace(in.Content) == "" ||
			!utf8.ValidString(in.Content) || strings.ContainsRune(in.Content, 0) {
			return Receipt{}, harness.ErrInvalidInput
		}
	case "remember":
		if byID || !validCandidate(Candidate{Content: in.Content, Category: in.Category, Confidence: 1, Source: in.Source}) ||
			(in.Category != "profile" && in.Category != "preference" && in.Category != "project") ||
			(scope.kind == GlobalScope && in.Category == "project") {
			return Receipt{}, harness.ErrInvalidInput
		}
	case "forget":
	default:
		return Receipt{}, harness.ErrInvalidInput
	}
	if in.Action != "forget" && (len(in.Content) > 2048 || credentialLike.MatchString(in.Content)) {
		return Receipt{}, harness.ErrInvalidInput
	}
	raw, _ := json.Marshal(in)
	// Preserve the exact v1 encoding for legacy receipt replay. Only the v2
	// request digest includes the host-bound input identity.
	if in.SchemaVersion == 2 {
		raw, _ = json.Marshal([]any{in, in.Source.RunID, in.Source.InputID, in.Source.EventSequence, in.Automatic})
	}
	digest := sha256.Sum256(raw)
	requestSHA := hex.EncodeToString(digest[:])
	var oldSHA, oldScope string
	var oldReceipt []byte
	err := tx.QueryRowContext(ctx, `SELECT scope_key,request_sha,receipt FROM memory_operations WHERE operation_id=?`, in.OperationID).Scan(&oldScope, &oldSHA, &oldReceipt)
	if err == nil {
		if oldScope != scope.key || oldSHA != requestSHA {
			return Receipt{}, ErrVersionConflict
		}
		var receipt Receipt
		err = json.Unmarshal(oldReceipt, &receipt)
		return receipt, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, err
	}
	version, err := checkScope(ctx, tx, scope, true)
	if err != nil {
		return Receipt{}, err
	}
	key := in.Key
	var f Fact
	var deleted bool
	if byID {
		f, deleted, err = factHeadTx(ctx, tx, scope, in.FactID)
		if err == nil && f.ID != "" {
			err = tx.QueryRowContext(ctx, `SELECT slot_key FROM memory_slots WHERE scope_key=? AND fact_id=?`, scope.key, f.ID).Scan(&key)
			if errors.Is(err, sql.ErrNoRows) {
				err = nil
			}
		}
	} else {
		f, deleted, err = slotHead(ctx, tx, scope, key)
	}
	if err != nil {
		return Receipt{}, err
	}
	superseded := false
	var manifest InputSnapshot
	var proof *writeProof
	if in.RequireSnapshot {
		pinned, sourceErr := sourceScopeTx(ctx, tx, scope, in.Source, in.Action == "forget" && !in.Automatic)
		if sourceErr != nil {
			if in.Automatic && errors.Is(sourceErr, ErrSourceSuperseded) {
				superseded = true
			} else {
				return Receipt{}, sourceErr
			}
		} else if in.Automatic {
			in.ExpectedRevision = pinned.Slots[key]
		} else if in.Action == "remember" && f.ID != "" && key != "" {
			var lastDelete int64
			if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision),0) FROM memory_fact_revisions WHERE fact_id=? AND deleted=1`, f.ID).Scan(&lastDelete); err != nil {
				return Receipt{}, err
			}
			if pinned.Slots[key] < lastDelete {
				return Receipt{}, ErrSourceSuperseded
			}
		}
		if !superseded {
			manifest, err = inputSnapshot(ctx, tx, in.Source.RunID, in.Source.InputID)
			if err != nil {
				return Receipt{}, err
			}
			proof, err = mutationWriteProof(manifest, in)
			if err != nil {
				return Receipt{}, err
			}
		}
	}
	now := time.Now().UTC()
	kind, target := "key", in.Key
	if byID {
		kind, target = "fact_id", in.FactID
	}
	receipt := Receipt{OperationID: in.OperationID, Key: key, Scope: scope.kind, FactID: f.ID, Revision: f.Revision, UpdatedAt: now,
		SchemaVersion: 2, TargetKind: kind, TargetID: target, Barrier: deleted}
	var restoreStatus, restoreReason string
	if !superseded && deleted && in.Action == "remember" && f.Revision == in.ExpectedRevision {
		restoreStatus, restoreReason, err = relearnDecisionTx(ctx, tx, f, in, manifest, proof)
		if err != nil {
			return Receipt{}, err
		}
	}
	switch {
	case superseded || in.Automatic && f.Revision != in.ExpectedRevision:
		receipt.Status = "superseded"
	case restoreStatus != "":
		receipt.Status, receipt.Reason = restoreStatus, restoreReason
	case byID && (f.ID == "" || (in.Action == "edit" && deleted)):
		receipt.Status = "not_found"
	case f.Revision != in.ExpectedRevision:
		return Receipt{}, &StateConflict{State: KeyState{Scope: scope.kind, Key: key, Revision: f.Revision, Deleted: deleted, Exists: f.ID != ""}}
	case in.Action == "forget" && deleted:
		receipt.Status = "already_forgotten"
	case in.Action == "forget" && f.ID != "":
		f, err = deleteFactTx(ctx, tx, scope, f, version, Source{ID: "operation/" + in.OperationID, Kind: in.Source.Kind, ActorID: in.Source.ActorID,
			RunID: in.Source.RunID, InputID: in.Source.InputID, EventSequence: in.Source.EventSequence, PolicyVersion: in.Source.PolicyVersion}, true)
		if err != nil {
			return Receipt{}, err
		}
		receipt.Status, receipt.Revision, receipt.Barrier = "forgotten", f.Revision, true
	case !deleted && f.ID != "" && f.Content == in.Content && (in.Action == "edit" || in.Action == "remember" && f.Category == in.Category):
		receipt.Status = "unchanged"
	default:
		version, err = bumpScope(ctx, tx, scope, version)
		if err != nil {
			return Receipt{}, err
		}
		isNew := f.ID == ""
		if isNew {
			hash := sha256.Sum256([]byte(scope.key + "\x00" + key))
			f = Fact{ID: "mem-" + hex.EncodeToString(hash[:16]), ScopeKey: scope.key, CreatedAt: now}
			if _, err = tx.ExecContext(ctx, `INSERT INTO memory_facts(id,scope_key,head_revision,created_at) VALUES(?,?,0,?)`, f.ID, scope.key, now.Format(time.RFC3339Nano)); err != nil {
				return Receipt{}, err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO memory_slots(scope_key,slot_key,fact_id) VALUES(?,?,?)`, scope.key, key, f.ID); err != nil {
				return Receipt{}, err
			}
		}
		// A manual correction has no new model-input evidence. Keep external
		// context marked on subsequent revisions without reusing old evidence.
		if in.Action == "edit" && Provenance(f) == ExternalContextProvenance {
			in.Source.Provenance = ExternalContextProvenance
		}
		f.Revision++
		f.Source = in.Source
		f.Source.ID = "operation/" + in.OperationID
		f.UpdatedAt = now
		deleted = in.Action == "forget"
		if in.Action == "edit" {
			f.Content = in.Content
		} else if !deleted {
			f.Content, f.Category, f.Confidence = in.Content, in.Category, 1
		}
		if f.Category == "" {
			f.Category = "profile"
		}
		if _, err = tx.ExecContext(ctx, `UPDATE memory_facts SET head_revision=?,deleted=? WHERE id=?`, f.Revision, deleted, f.ID); err != nil {
			return Receipt{}, err
		}
		if err = insertRevision(ctx, tx, f, version, deleted); err != nil {
			return Receipt{}, err
		}
		if !deleted {
			if err = recordValueIdentityTx(ctx, tx, f, manifest, proof); err != nil {
				return Receipt{}, err
			}
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM memory_vectors WHERE fact_id=?`, f.ID); err != nil {
			return Receipt{}, err
		}
		receipt.FactID, receipt.Revision, receipt.Barrier = f.ID, f.Revision, deleted
		receipt.Reason = restoreReason
		receipt.Status = "saved"
		if !isNew {
			receipt.Status = "updated"
		}
		if deleted {
			receipt.Status = "not_found"
		} // Newly created empty tombstone.
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return Receipt{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO memory_operations(operation_id,scope_key,request_sha,receipt,created_at) VALUES(?,?,?,?,?)`, in.OperationID, scope.key, requestSHA, encoded, now.Format(time.RFC3339Nano)); err != nil {
		return Receipt{}, err
	}
	if !in.Automatic && in.Source.Kind == "operator" && in.Source.RunID != "" && in.Source.InputID != "" {
		_, err = tx.ExecContext(ctx, `INSERT INTO memory_operation_sources(operation_id,run_id,input_id,action,target_kind,target_id) VALUES(?,?,?,?,?,?)`,
			in.OperationID, in.Source.RunID, in.Source.InputID, in.Action, kind, target)
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE memory_inputs SET decision='explicit_action' WHERE run_id=? AND input_id=?`, in.Source.RunID, in.Source.InputID)
		}
	}
	return receipt, err
}

func factHeadTx(ctx context.Context, tx *sql.Tx, scope Scope, id string) (Fact, bool, error) {
	var deleted bool
	err := tx.QueryRowContext(ctx, `SELECT deleted FROM memory_facts WHERE scope_key=? AND id=?`, scope.key, id).Scan(&deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return Fact{}, false, nil
	}
	if err != nil {
		return Fact{}, false, err
	}
	f, err := scanFact(tx.QueryRowContext(ctx, `SELECT `+factColumns+` FROM memory_facts f JOIN memory_fact_revisions r ON r.fact_id=f.id AND r.revision=f.head_revision WHERE f.scope_key=? AND f.id=?`, scope.key, id))
	return f, deleted, err
}

type KeyState struct {
	Scope    ScopeKind `json:"scope"`
	Key      string    `json:"key"`
	Revision int64     `json:"current_revision"`
	Deleted  bool      `json:"deleted"`
	Exists   bool      `json:"exists"`
}

type StateConflict struct{ State KeyState }

func (e *StateConflict) Error() string {
	raw, _ := json.Marshal(e.State)
	return fmt.Sprintf("%v: %s", ErrVersionConflict, raw)
}
func (e *StateConflict) Unwrap() error { return ErrVersionConflict }
func (s *Store) KeyState(ctx context.Context, scope Scope, key string) (KeyState, error) {
	key = CanonicalKey(key)
	if !slotPattern.MatchString(key) {
		return KeyState{}, harness.ErrInvalidInput
	}
	var state KeyState
	err := transact(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := checkScope(ctx, tx, scope, false); err != nil {
			return err
		}
		f, deleted, err := slotHead(ctx, tx, scope, key)
		state = KeyState{Scope: scope.kind, Key: key, Revision: f.Revision, Deleted: deleted, Exists: f.ID != ""}
		return err
	})
	return state, err
}

// MutationOperationID separates targets, profiles, actions and source inputs.
func MutationOperationID(scope Scope, in Mutation) string {
	kind, target := "key", CanonicalKey(in.Key)
	if in.FactID != "" {
		kind, target = "fact_id", in.FactID
	}
	raw, _ := json.Marshal([]any{in.Source.RunID, in.Source.InputID, scope.key, in.Action, kind, target,
		in.ExpectedRevision, in.Category, in.Content})
	return OperationID("memory-op-v2", string(raw))
}

func (s *Store) SlotVersions(ctx context.Context, scope Scope) (map[string]int64, error) {
	return slotVersions(ctx, s.db, scope)
}

func slotVersions(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, scope Scope) (map[string]int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT k.slot_key,f.head_revision FROM memory_slots k JOIN memory_facts f ON f.id=k.fact_id WHERE k.scope_key=?`, scope.key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]int64{}
	for rows.Next() {
		var key string
		var v int64
		if err = rows.Scan(&key, &v); err != nil {
			return nil, err
		}
		result[key] = v
	}
	return result, rows.Err()
}

func (s *Store) FactKey(ctx context.Context, scope Scope, id string) (string, error) {
	var key string
	err := s.db.QueryRowContext(ctx, `SELECT slot_key FROM memory_slots WHERE scope_key=? AND fact_id=?`, scope.key, id).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return key, err
}

// SourceText ties in-run mutations to the immutable accepted human input. Child
// and notification runs have no write authority over personal memory.
func (s *Store) SourceText(ctx context.Context, runID, inputID string) (string, int64, error) {
	sequence, err := s.AcceptedInputSequence(ctx, runID, inputID)
	if err != nil {
		return "", 0, err
	}
	var raw []byte
	if err = s.db.QueryRowContext(ctx, `SELECT content FROM harness_inputs WHERE id=? AND run_id=?`, inputID, runID).Scan(&raw); err != nil {
		return "", 0, err
	}
	var input []harness.Content
	if err = json.Unmarshal(raw, &input); err != nil {
		return "", 0, err
	}
	var text strings.Builder
	for _, p := range input {
		if p.Type == "text" {
			text.WriteString(p.Text)
			text.WriteByte('\n')
		}
	}
	return text.String(), sequence, nil
}

func OperationID(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return fmt.Sprintf("memory-%x", h[:20])
}
