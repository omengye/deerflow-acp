package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	bt "github.com/cloudwego/eino/adk/backgroundtask"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/agents"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
)

// BackgroundExecutionSpec is immutable host policy and input, without provider
// credentials. Parent stores the policy accepted by the submitting foreground
// run; child identity is separately derived by the native task service.
type BackgroundExecutionSpec struct {
	Version            int               `json:"version"`
	Parent             harness.Session   `json:"parent"`
	AgentVersion       string            `json:"agentVersion"`
	HostPolicy         string            `json:"hostPolicy"`
	Input              []harness.Content `json:"input"`
	Extension          json.RawMessage   `json:"extension"`
	OriginArguments    string            `json:"originArguments"`
	Role               *agents.Spec      `json:"role,omitempty"`
	Roles              *agents.Catalog   `json:"roles,omitempty"`
	Depth              int               `json:"depth,omitempty"`
	ParentTaskID       string            `json:"parentTaskId,omitempty"`
	ParentInvocationID string            `json:"parentInvocationId,omitempty"`
	// Legacy tasks stay explicit-only; later settings cannot opt them in.
	Automatic    *AutomaticContinuationPolicy `json:"automatic,omitempty"`
	Continuation *BackgroundContinuationRef   `json:"continuation,omitempty"`
}

type BackgroundContinuationRef struct {
	SourceSHA      string `json:"sourceSha"`
	RoleID         string `json:"roleId"`
	PreviousTaskID string `json:"previousTaskId"`
}

type AutomaticContinuationPolicy struct {
	Version int  `json:"version"`
	Enabled bool `json:"enabled"`
	Limit   int  `json:"limit"`
}

func ContinuationPolicy(catalog *agents.Catalog) *AutomaticContinuationPolicy {
	if catalog == nil {
		return nil
	}
	p := &AutomaticContinuationPolicy{Version: 1, Limit: 10}
	if catalog.Config.AutoContinue != nil {
		p.Enabled = *catalog.Config.AutoContinue
	}
	if catalog.Config.MaxAutoContinuations != nil {
		p.Limit = *catalog.Config.MaxAutoContinuations
	}
	return p
}

type BackgroundExecutionStore struct{ store *Store }

func NewBackgroundExecutionStore(ctx context.Context, store *Store) (*BackgroundExecutionStore, error) {
	if store == nil {
		return nil, errors.New("background execution store is required")
	}
	_, err := store.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS harness_background_specs (
task_id TEXT PRIMARY KEY REFERENCES eino_background_tasks(id), child_session_id TEXT NOT NULL REFERENCES harness_sessions(id),
input_id TEXT UNIQUE NOT NULL REFERENCES harness_inputs(id), contract TEXT NOT NULL, payload BLOB NOT NULL, binding BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS harness_background_engine_contracts (
task_id TEXT PRIMARY KEY REFERENCES harness_background_specs(task_id), contract TEXT NOT NULL);`)
	if err != nil {
		return nil, err
	}
	if err = migrateBackgroundContinuations(ctx, store.db); err != nil {
		return nil, err
	}
	return &BackgroundExecutionStore{store: store}, nil
}

func (s BackgroundExecutionSpec) Contract() (string, error) {
	if (s.Version < 1 || s.Version > 6) || s.Parent.ID == "" || s.Parent.CWD == "" || s.Parent.ConfigVersion < 1 || s.AgentVersion == "" || s.HostPolicy == "" || len(s.Input) != 1 || !json.Valid(s.Extension) {
		return "", fmt.Errorf("%w: incomplete background execution policy", harness.ErrInvalidInput)
	}
	if s.Version == 1 && s.Role != nil || s.Version >= 2 && s.Role == nil || s.Version < 3 && s.Roles != nil || s.Version >= 3 && s.Roles == nil {
		return "", harness.ErrInvalidInput
	}
	if s.Roles != nil {
		if err := s.Roles.Validate(); err != nil {
			return "", err
		}
	}
	if s.Version < 5 && s.ParentInvocationID != "" {
		return "", harness.ErrInvalidInput
	}
	if (s.Version == 6) != (s.Continuation != nil) {
		return "", harness.ErrInvalidInput
	}
	if c := s.Continuation; c != nil && (len(c.SourceSHA) != 64 || c.RoleID == "" || c.PreviousTaskID == "") {
		return "", harness.ErrInvalidInput
	}
	if s.Automatic != nil {
		want := ContinuationPolicy(s.Roles)
		if s.Version < 5 || want == nil || *s.Automatic != *want {
			return "", harness.ErrInvalidInput
		}
	}
	if s.Version < 4 {
		if s.Depth != 0 || s.ParentTaskID != "" {
			return "", harness.ErrInvalidInput
		}
	} else {
		maxDepth := 1
		if s.Roles.Config.MaxDepth != nil {
			maxDepth = *s.Roles.Config.MaxDepth
		}
		parentID := s.ParentTaskID
		if s.Version >= 5 {
			parentID = s.ParentInvocationID
		}
		if s.Depth < 1 || s.Depth > maxDepth || (s.Depth == 1) != (parentID == "") || s.Depth == 1 && s.ParentTaskID != "" {
			return "", harness.ErrInvalidInput
		}
	}
	if s.Role != nil {
		if err := s.Role.Validate(); err != nil {
			return "", err
		}
		if s.Role.Model == "" {
			return "", harness.ErrInvalidInput
		}
	}
	// The first host integration accepts explicit child instructions. Copying
	// parent asset capabilities requires a separate child-owned asset import.
	for _, part := range s.Input {
		if part.Type != "text" || part.Text == "" || part.Data != "" || part.URI != "" || part.Asset != nil || part.Size != nil || part.MimeType != "" || part.Name != "" || part.Description != "" {
			return "", fmt.Errorf("%w: background input must contain plain instructions", harness.ErrInvalidInput)
		}
	}
	var arguments struct {
		Instruction    string `json:"instruction"`
		Description    string `json:"description,omitempty"`
		ChildSessionID string `json:"childSessionId,omitempty"`
		Role           string `json:"role,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewBufferString(s.OriginArguments))
	decoder.DisallowUnknownFields()
	var trailing any
	if decoder.Decode(&arguments) != nil || decoder.Decode(&trailing) != io.EOF || arguments.Instruction != s.Input[0].Text {
		return "", fmt.Errorf("%w: background input differs from approved instruction", harness.ErrInvalidInput)
	}
	if s.Role == nil && arguments.Role != "" {
		return "", harness.ErrInvalidInput
	}
	if s.Role != nil {
		role := arguments.Role
		if role == "" {
			role = "general-purpose"
		}
		if role != s.Role.Name {
			return "", harness.ErrTaskOriginConflict
		}
	}
	encoded, err := json.Marshal(canonicalBackgroundSpec(s))
	if err != nil {
		return "", err
	}
	if len(encoded) > 128<<10 {
		return "", fmt.Errorf("%w: background execution policy is too large", harness.ErrInvalidInput)
	}
	// A continuation retains execution policy while receiving a new instruction.
	// Native RequestHash plus the complete persisted payload binds task intent.
	policy := canonicalBackgroundSpec(s)
	policy.Input, policy.OriginArguments = nil, ""
	encoded, err = json.Marshal(policy)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(encoded)), nil
}

func canonicalBackgroundSpec(s BackgroundExecutionSpec) BackgroundExecutionSpec {
	s.Parent.Title = ""
	s.Parent.CreatedAt, s.Parent.UpdatedAt = time.Time{}, time.Time{}
	return s
}

func validateBackgroundSpec(binding background.Binding, spec BackgroundExecutionSpec) (string, error) {
	contract, err := spec.Contract()
	if err != nil {
		return "", err
	}
	if contract != binding.ExecutionContract || spec.Parent.ID != binding.ParentSessionID || spec.Parent.CWD != binding.Workspace || spec.Parent.ConfigVersion != binding.ConfigVersion || spec.AgentVersion != binding.AgentVersion || binding.TaskID == "" || binding.ChildSessionID == "" || binding.ChildSessionID == binding.ParentSessionID || binding.OriginRunID == "" || binding.OriginToolCallID == "" {
		return "", harness.ErrTaskOriginConflict
	}
	if binding.ParentInvocationID != spec.ParentInvocationID || spec.Version >= 4 && (binding.TaskDepth() != spec.Depth || binding.ParentTaskID != spec.ParentTaskID) {
		return "", harness.ErrTaskOriginConflict
	}
	if c := spec.Continuation; c != nil {
		if binding.ContinuationOf != c.PreviousTaskID || binding.InvocationID != c.RoleID {
			return "", harness.ErrTaskOriginConflict
		}
	} else if binding.ContinuationOf != "" || binding.InvocationID != "" {
		return "", harness.ErrTaskOriginConflict
	}
	var arguments struct {
		ChildSessionID string `json:"childSessionId"`
	}
	if json.Unmarshal([]byte(spec.OriginArguments), &arguments) != nil || (arguments.ChildSessionID != "" && arguments.ChildSessionID != binding.ChildSessionID) {
		return "", harness.ErrTaskOriginConflict
	}
	return contract, nil
}

func backgroundInputID(binding background.Binding) string { return binding.TaskID + "/input" }

// BindTx runs inside the native task creation transaction, alongside the real
// budget binding. No model, resource creation or nested pool access occurs.
// Duplicate bindings are accepted only when every immutable value agrees.
func (s *BackgroundExecutionStore) BindTx(ctx context.Context, tx *sql.Tx, binding background.Binding, spec BackgroundExecutionSpec) error {
	contract, err := validateBackgroundSpec(binding, spec)
	if err != nil {
		return err
	}
	spec = canonicalBackgroundSpec(spec)
	encoded, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	bindingData, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	var previous, previousBinding []byte
	var previousChild, previousInput, previousContract string
	err = tx.QueryRowContext(ctx, `SELECT child_session_id,input_id,contract,payload,binding FROM harness_background_specs WHERE task_id=?`, binding.TaskID).Scan(&previousChild, &previousInput, &previousContract, &previous, &previousBinding)
	if err == nil {
		if previousChild != binding.ChildSessionID || previousInput != backgroundInputID(binding) || previousContract != contract || !bytes.Equal(previous, encoded) || !bytes.Equal(previousBinding, bindingData) {
			return harness.ErrTaskOriginConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if spec.Continuation != nil {
		admission, ok := ctx.Value(backgroundContinuationAdmissionKey{}).(*BackgroundContinuation)
		if !ok {
			return harness.ErrPermissionDenied
		}
		if err = admission.CheckTx(ctx, tx, binding); err != nil {
			return err
		}
	} else {
		var parentSession string
		var originStatus string
		if err = tx.QueryRowContext(ctx, `SELECT session_id,status FROM harness_runs WHERE id=?`, binding.OriginRunID).Scan(&parentSession, &originStatus); errors.Is(err, sql.ErrNoRows) {
			return harness.ErrNotFound
		} else if err != nil {
			return err
		}
		if parentSession != binding.ParentSessionID || originStatus != "running" {
			return harness.ErrTaskOriginConflict
		}
		var receiptData []byte
		if err = tx.QueryRowContext(ctx, `SELECT receipt FROM harness_tool_receipts WHERE run_id=? AND tool_call_id=? AND session_id=? AND state='started'`, binding.OriginRunID, binding.OriginToolCallID, binding.ParentSessionID).Scan(&receiptData); errors.Is(err, sql.ErrNoRows) {
			return harness.ErrReceiptConflict
		} else if err != nil {
			return err
		}
		var receipt harness.ToolReceipt
		if json.Unmarshal(receiptData, &receipt) != nil || receipt.ToolName != "background_agent" || receipt.SessionID != binding.ParentSessionID || receipt.RunID != binding.OriginRunID || receipt.ToolCallID != binding.OriginToolCallID || receipt.ConfigVersion != binding.ConfigVersion || receipt.State != harness.ReceiptStarted || receipt.ArgumentsDigest != fmt.Sprintf("%x", sha256.Sum256([]byte(spec.OriginArguments))) {
			return harness.ErrReceiptConflict
		}
		if err = s.CheckParentPolicyTx(ctx, tx, binding, spec); err != nil {
			return err
		}
	}

	child := spec.Parent
	child.ID, child.Subagents, child.Title = binding.ChildSessionID, spec.AllowsDelegation(), ""
	if spec.Role != nil {
		child.Model = spec.Role.Model
	}
	child.CreatedAt, child.UpdatedAt = time.Now().UTC(), time.Now().UTC()
	var childCWD, childModel, childMode, childApproval string
	var childVersion int64
	var childSubagents bool
	err = tx.QueryRowContext(ctx, `SELECT s.cwd,s.model,s.mode,c.approval_mode,c.subagents,c.version FROM harness_sessions s JOIN harness_session_configs c ON c.session_id=s.id WHERE s.id=?`, child.ID).Scan(&childCWD, &childModel, &childMode, &childApproval, &childSubagents, &childVersion)
	if err == nil {
		if childCWD != child.CWD || childModel != child.Model || childMode != child.Mode || childApproval != child.ApprovalMode || childSubagents != child.Subagents || childVersion != child.ConfigVersion {
			return harness.ErrTaskOriginConflict
		}
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM harness_background_specs b JOIN harness_background_bindings a ON a.task_id=b.task_id WHERE b.child_session_id=? AND a.parent_session_id=?`, child.ID, binding.ParentSessionID).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			return harness.ErrTaskOriginConflict
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		if _, err = tx.ExecContext(ctx, `INSERT INTO harness_sessions(id,cwd,title,mode,model,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, child.ID, child.CWD, child.Title, child.Mode, child.Model, child.CreatedAt.Format(time.RFC3339Nano), child.UpdatedAt.Format(time.RFC3339Nano)); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO harness_session_configs VALUES(?,?,?,?)`, child.ID, child.ApprovalMode, child.Subagents, child.ConfigVersion); err != nil {
			return err
		}
	} else {
		return err
	}
	input, err := json.Marshal(spec.Input)
	if err != nil {
		return err
	}
	now := timestamp()
	if _, err = tx.ExecContext(ctx, `INSERT INTO harness_runs(id,session_id,input_id,status,created_at,updated_at) VALUES(?,?,?,'pending',?,?)`, binding.TaskID, child.ID, backgroundInputID(binding), now, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO harness_inputs VALUES(?,?,?)`, backgroundInputID(binding), binding.TaskID, input); err != nil {
		return err
	}
	eventKind := "user_message"
	if spec.Continuation != nil {
		eventKind = "continuation_started"
	}
	user, err := json.Marshal(harness.RunEvent{SessionID: child.ID, RunID: binding.TaskID, Kind: eventKind, Content: spec.Input})
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO harness_events(session_id,run_id,event) VALUES(?,?,?)`, child.ID, binding.TaskID, user); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO harness_background_specs VALUES(?,?,?,?,?,?)`, binding.TaskID, child.ID, backgroundInputID(binding), contract, encoded, bindingData)
	return err
}

// CheckParentPolicyTx is also suitable for the host's budget effect check.
// Changing parent configuration revokes old task policy before its next effect.
func (s *BackgroundExecutionStore) CheckParentPolicyTx(ctx context.Context, tx *sql.Tx, binding background.Binding, spec BackgroundExecutionSpec) error {
	if _, err := validateBackgroundSpec(binding, spec); err != nil {
		return err
	}
	var cwd, model, mode, approval string
	var version int64
	var subagents bool
	err := tx.QueryRowContext(ctx, `SELECT s.cwd,s.model,s.mode,c.approval_mode,c.subagents,c.version FROM harness_sessions s JOIN harness_session_configs c ON c.session_id=s.id WHERE s.id=?`, binding.ParentSessionID).Scan(&cwd, &model, &mode, &approval, &subagents, &version)
	if err != nil {
		return err
	}
	parent := spec.Parent
	if cwd != parent.CWD || model != parent.Model || mode != parent.Mode || approval != parent.ApprovalMode || version != parent.ConfigVersion || subagents != parent.Subagents || !subagents {
		return harness.ErrPermissionDenied
	}
	if spec.Version >= 5 && spec.ParentInvocationID != "" {
		if _, err := s.ParentRoleTx(ctx, tx, binding, spec); err != nil {
			return err
		}
	}
	if spec.Continuation != nil {
		if _, err := readBackgroundContinuationSourceTx(ctx, tx, binding, spec); err != nil {
			return err
		}
	}
	if spec.ParentTaskID != "" {
		parentBinding, parentSpec, err := s.parentSpecTx(ctx, tx, binding, spec)
		if err != nil {
			return err
		}
		return s.CheckParentPolicyTx(ctx, tx, parentBinding, parentSpec)
	}
	return nil
}

func (s BackgroundExecutionSpec) AllowsDelegation() bool {
	if s.Version < 4 || s.Roles == nil || s.Role == nil || len(s.Role.DelegateTo) == 0 {
		return false
	}
	maxDepth := 1
	if s.Roles.Config.MaxDepth != nil {
		maxDepth = *s.Roles.Config.MaxDepth
	}
	return s.Depth < maxDepth && (s.Role.Tools.Allows("task") || s.Role.Tools.Allows("background_agent"))
}

// ParentRoleTx reads the immutable parent's recipe inside task creation.
// The caller resolves the selected child against it before binding the role.
func (s *BackgroundExecutionStore) ParentRoleTx(ctx context.Context, tx *sql.Tx, binding background.Binding, spec BackgroundExecutionSpec) (*agents.Spec, error) {
	if spec.Version >= 5 && spec.ParentInvocationID != "" {
		parent, err := agents.ReadInvocation(ctx, tx, spec.ParentInvocationID, binding.RootBudgetID)
		if err != nil {
			return nil, err
		}
		if parent.Depth+1 != spec.Depth {
			return nil, harness.ErrTaskOriginConflict
		}
		current, nearest := parent, ""
		for {
			if strings.HasPrefix(current.ID, "background/") && nearest == "" {
				nearest = strings.TrimPrefix(current.ID, "background/")
			}
			if current.Depth == 1 {
				if current.ParentID != "" {
					return nil, harness.ErrTaskOriginConflict
				}
				break
			}
			if current.Depth < 1 || current.Depth > 3 || current.ParentID == "" {
				return nil, harness.ErrTaskOriginConflict
			}
			ancestor, err := agents.ReadInvocation(ctx, tx, current.ParentID, binding.RootBudgetID)
			if err != nil {
				return nil, err
			}
			if ancestor.Depth+1 != current.Depth {
				return nil, harness.ErrTaskOriginConflict
			}
			current = ancestor
		}
		if nearest != spec.ParentTaskID {
			// A resumed role has a new physical host without a new logical
			// invocation. Validate that host's bound role as the nearest owner.
			host, hostSpec, err := s.parentSpecTx(ctx, tx, binding, spec)
			if err != nil || hostSpec.Continuation == nil {
				return nil, errors.Join(harness.ErrTaskOriginConflict, err)
			}
			owned, err := agents.IsDescendant(ctx, tx, binding.RootBudgetID, host.RoleID(), parent.ID)
			if err != nil || !owned {
				return nil, errors.Join(harness.ErrTaskOriginConflict, err)
			}
		}
		return &parent.Spec, nil
	}
	if spec.ParentTaskID == "" {
		return nil, nil
	}
	_, parent, err := s.parentSpecTx(ctx, tx, binding, spec)
	return parent.Role, err
}

func (s *BackgroundExecutionStore) parentSpecTx(ctx context.Context, tx *sql.Tx, binding background.Binding, spec BackgroundExecutionSpec) (background.Binding, BackgroundExecutionSpec, error) {
	var b background.Binding
	var parent BackgroundExecutionSpec
	var data, encoded []byte
	if err := tx.QueryRowContext(ctx, `SELECT binding,payload FROM harness_background_specs WHERE task_id=?`, spec.ParentTaskID).Scan(&data, &encoded); err != nil {
		return b, parent, err
	}
	if json.Unmarshal(data, &b) != nil || json.Unmarshal(encoded, &parent) != nil {
		return b, parent, harness.ErrTaskOriginConflict
	}
	validDepth := parent.Depth+1 == spec.Depth
	if spec.Version >= 5 {
		validDepth = parent.Depth < spec.Depth && parent.Depth > 0
	}
	if spec.Version < 4 || parent.Version < 4 || b.TaskID != spec.ParentTaskID || binding.ParentTaskID != b.TaskID || b.ChildSessionID != binding.ParentSessionID || b.RootSession() != binding.RootSession() || b.RootBudgetID != binding.RootBudgetID || !validDepth || parent.Roles == nil || spec.Roles == nil || parent.Roles.Digest != spec.Roles.Digest || !parent.AllowsDelegation() {
		return b, parent, harness.ErrTaskOriginConflict
	}
	if _, err := validateBackgroundSpec(b, parent); err != nil {
		return b, parent, err
	}
	return b, parent, nil
}

func (s *BackgroundExecutionStore) Load(ctx context.Context, binding background.Binding) (harness.RunRequest, BackgroundExecutionSpec, error) {
	var spec BackgroundExecutionSpec
	var data, bindingData []byte
	var childID, inputID, contract, runSession, runInput, rootID, memberSession string
	err := s.store.db.QueryRowContext(ctx, `SELECT b.child_session_id,b.input_id,b.contract,b.payload,b.binding,r.session_id,r.input_id,m.root_id,m.session_id FROM harness_background_specs b JOIN harness_runs r ON r.id=b.task_id JOIN budget_members m ON m.id=b.task_id WHERE b.task_id=?`, binding.TaskID).Scan(&childID, &inputID, &contract, &data, &bindingData, &runSession, &runInput, &rootID, &memberSession)
	if errors.Is(err, sql.ErrNoRows) {
		return harness.RunRequest{}, spec, harness.ErrNotFound
	}
	if err != nil {
		return harness.RunRequest{}, spec, err
	}
	if err = json.Unmarshal(data, &spec); err != nil {
		return harness.RunRequest{}, spec, err
	}
	var savedBinding background.Binding
	if json.Unmarshal(bindingData, &savedBinding) != nil || savedBinding != binding || childID != binding.ChildSessionID || inputID != backgroundInputID(binding) || contract != binding.ExecutionContract || runSession != childID || runInput != inputID || rootID != binding.RootBudgetID || memberSession != childID {
		return harness.RunRequest{}, spec, harness.ErrTaskOriginConflict
	}
	if _, err = validateBackgroundSpec(binding, spec); err != nil {
		return harness.RunRequest{}, spec, err
	}
	child, err := backgroundChildPolicy(ctx, s.store.db, binding, spec)
	if err != nil {
		return harness.RunRequest{}, spec, err
	}
	return harness.RunRequest{Session: child, RunID: binding.TaskID, InputID: backgroundInputID(binding), RootBudgetID: binding.RootBudgetID, Input: spec.Input}, spec, nil
}

func backgroundChildPolicy(ctx context.Context, q receiptQuery, binding background.Binding, spec BackgroundExecutionSpec) (harness.Session, error) {
	child, err := scanSession(q.QueryRowContext(ctx, `SELECT s.id,s.cwd,s.title,s.mode,s.model,s.created_at,s.updated_at,c.approval_mode,c.subagents,c.version FROM harness_sessions s JOIN harness_session_configs c ON c.session_id=s.id WHERE s.id=?`, binding.ChildSessionID))
	if err != nil {
		return child, err
	}
	wantModel := spec.Parent.Model
	if spec.Role != nil {
		wantModel = spec.Role.Model
	}
	if child.CWD != spec.Parent.CWD || child.Model != wantModel || child.Mode != spec.Parent.Mode || child.ApprovalMode != spec.Parent.ApprovalMode || child.ConfigVersion != spec.Parent.ConfigVersion || child.Subagents != spec.AllowsDelegation() {
		return child, harness.ErrTaskOriginConflict
	}
	return child, nil
}

// CheckAttemptPolicyTx checks the persisted host policy before a new effect.
// Compose it with Service.CheckEffectTx for native/child lease fencing. Origin
// receipt/run liveness is checked only by first Bind: a detached child can keep
// working after its parent turn completed, provided that policy is unchanged.
func (s *BackgroundExecutionStore) CheckAttemptPolicyTx(ctx context.Context, tx *sql.Tx, scope background.TaskScope) error {
	if scope.Attempt < 1 {
		return harness.ErrInvalidInput
	}
	if _, err := s.backgroundRunTx(ctx, tx, scope); err != nil {
		return err
	}
	var data []byte
	if err := tx.QueryRowContext(ctx, `SELECT payload FROM harness_background_specs WHERE task_id=?`, scope.Binding.TaskID).Scan(&data); err != nil {
		return err
	}
	var spec BackgroundExecutionSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return err
	}
	if _, err := validateBackgroundSpec(scope.Binding, spec); err != nil {
		return err
	}
	if _, err := backgroundChildPolicy(ctx, tx, scope.Binding, spec); err != nil {
		return err
	}
	return s.CheckParentPolicyTx(ctx, tx, scope.Binding, spec)
}

// BeforeAttemptTx projects the admitted native attempt into the child business
// run. Pass LedgerAdapter.BeforeAttemptTx as admit: it checks the current native
// attempt and child lease and begins the shared budget in this same transaction.
// The caller owns commit/rollback; an error must roll the entire transaction back.
func (s *BackgroundExecutionStore) BeforeAttemptTx(ctx context.Context, tx *sql.Tx, scope background.TaskScope, admit func(context.Context, *sql.Tx, background.TaskScope) error) error {
	if admit == nil || scope.Attempt < 1 {
		return harness.ErrInvalidInput
	}
	status, err := s.backgroundRunTx(ctx, tx, scope)
	if err != nil {
		return err
	}
	if status != "pending" && status != "waiting_input" && status != "running" {
		return harness.ErrExecutionConflict
	}
	if err = backgroundReceiptFrontier(ctx, tx, scope, false); err != nil {
		return err
	}
	if err = admit(ctx, tx, scope); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE harness_runs SET status='running',stop_reason='',error='',updated_at=? WHERE id=?`, timestamp(), scope.Binding.TaskID)
	return err
}

// CommitAttemptTx is called by the native transition hook only after executor
// and resource cleanup have joined. Pass LedgerAdapter.CommitAttemptTx as commit
// so native/child fencing and ledger End are atomic with receipts and this run
// projection. It never starts a transaction or rechecks revoked parent policy:
// cleanup evidence must remain publishable after that policy has changed.
func (s *BackgroundExecutionStore) CommitAttemptTx(ctx context.Context, tx *sql.Tx, scope background.TaskScope, nativeStatus string, commit func(context.Context, *sql.Tx, background.TaskScope, string) error) error {
	if commit == nil || scope.Attempt < 1 {
		return harness.ErrInvalidInput
	}
	previous, err := s.backgroundRunTx(ctx, tx, scope)
	if err != nil {
		return err
	}
	var status, reason, detail string
	switch bt.Status(nativeStatus) {
	case bt.StatusCompleted:
		status, reason = "completed", "end_turn"
		if err = backgroundReceiptFrontier(ctx, tx, scope, true); err != nil {
			return err
		}
	case bt.StatusWaitingInput, bt.StatusSuspended, bt.StatusPending:
		status = "waiting_input"
		// Pending receipts are approval continuations. Preserve their IDs,
		// versions and arguments; started/uncertain effects cannot safely pause.
		if err = backgroundReceiptFrontier(ctx, tx, scope, false); err != nil {
			return err
		}
	case bt.StatusFailed:
		status, reason, detail = "failed", "failed", "background attempt failed before a final tool receipt"
	case bt.StatusCanceled:
		status, reason = "cancelled", "cancelled"
	default:
		return harness.ErrInvalidInput
	}
	if previous != "running" && previous != "pending" && previous != "waiting_input" && previous != status {
		return harness.ErrExecutionConflict
	}
	if err = commit(ctx, tx, scope, nativeStatus); err != nil {
		return err
	}
	if status == "failed" || status == "cancelled" {
		if err = settleOpenReceipts(ctx, tx, scope.Binding.TaskID, "background attempt ended before a final tool receipt"); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE harness_runs SET status=?,stop_reason=?,error=?,updated_at=? WHERE id=?`, status, reason, detail, timestamp(), scope.Binding.TaskID)
	return err
}

func (s *BackgroundExecutionStore) backgroundRunTx(ctx context.Context, tx *sql.Tx, scope background.TaskScope) (string, error) {
	if tx == nil || scope.Attempt < 0 {
		return "", harness.ErrInvalidInput
	}
	var data []byte
	var child, input, contract, sessionID, runInput, rootID, memberSession, status string
	err := tx.QueryRowContext(ctx, `SELECT b.binding,b.child_session_id,b.input_id,b.contract,r.session_id,r.input_id,r.status,m.root_id,m.session_id FROM harness_background_specs b JOIN harness_runs r ON r.id=b.task_id JOIN budget_members m ON m.id=b.task_id WHERE b.task_id=?`, scope.Binding.TaskID).Scan(&data, &child, &input, &contract, &sessionID, &runInput, &status, &rootID, &memberSession)
	if errors.Is(err, sql.ErrNoRows) {
		return "", harness.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	var saved background.Binding
	if json.Unmarshal(data, &saved) != nil || saved != scope.Binding || child != saved.ChildSessionID || input != backgroundInputID(saved) || contract != saved.ExecutionContract || sessionID != child || runInput != input || rootID != saved.RootBudgetID || memberSession != child {
		return "", harness.ErrTaskOriginConflict
	}
	return status, nil
}

// TransitionTx handles native cancellation while no executor owns the task.
// Call it from the native transaction hook, after its actor/version checks and
// before saving the new task snapshot. Idle cancellation has no active attempt
// to end; its last waiting ledger outcome and already-spent budget stay intact.
func (s *BackgroundExecutionStore) TransitionTx(ctx context.Context, tx *sql.Tx, scope background.TaskScope, before, after *bt.Task) error {
	if before == nil || after == nil || tx == nil {
		return harness.ErrInvalidInput
	}
	if before.Status == bt.StatusRunning || after.Status != bt.StatusCanceled || before.Status == bt.StatusCanceled {
		return nil
	}
	if (before.Status != bt.StatusPending && before.Status != bt.StatusWaitingInput && before.Status != bt.StatusSuspended) || before.Spec.ID != scope.Binding.TaskID || after.Spec.ID != before.Spec.ID || before.Spec.SessionID != scope.Binding.ParentSessionID || after.Spec.SessionID != before.Spec.SessionID || scope.Attempt != before.Attempt || after.Attempt != before.Attempt || after.Version != before.Version+1 {
		return harness.ErrTaskOriginConflict
	}
	previous, err := s.backgroundRunTx(ctx, tx, scope)
	if err != nil {
		return err
	}
	if previous != "pending" && previous != "waiting_input" && previous != "running" && previous != "cancelled" {
		return harness.ErrExecutionConflict
	}
	var nativeStatus string
	var version int64
	if err = tx.QueryRowContext(ctx, `SELECT status,version FROM eino_background_tasks WHERE id=?`, before.Spec.ID).Scan(&nativeStatus, &version); err != nil {
		return err
	}
	if nativeStatus != string(before.Status) || version != before.Version {
		return bt.ErrVersionConflict
	}
	if err = settleOpenReceipts(ctx, tx, before.Spec.ID, "idle background task cancelled before a final tool receipt"); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE harness_runs SET status='cancelled',stop_reason='cancelled',error='',updated_at=? WHERE id=?`, timestamp(), before.Spec.ID)
	return err
}

func backgroundReceiptFrontier(ctx context.Context, tx *sql.Tx, scope background.TaskScope, completing bool) error {
	var open bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM harness_tool_receipts WHERE session_id=? AND (state='uncertain' OR (run_id=? AND (state='started' OR (? AND state='pending')))))`, scope.Binding.ChildSessionID, scope.Binding.TaskID, completing).Scan(&open)
	if err != nil {
		return err
	}
	if open {
		return harness.ErrReceiptConflict
	}
	return nil
}

// PinEngineContract prevents a raw native checkpoint from resuming with a
// changed set of model/tool handlers. First construction pins before execution.
func (s *BackgroundExecutionStore) PinEngineContract(ctx context.Context, scope background.TaskScope, contract string, check func(context.Context, *sql.Tx, background.TaskScope) error) error {
	if contract == "" || check == nil || scope.Attempt < 1 {
		return harness.ErrInvalidInput
	}
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = check(ctx, tx, scope); err != nil {
		return err
	}
	var data []byte
	if err = tx.QueryRowContext(ctx, `SELECT binding FROM harness_background_specs WHERE task_id=?`, scope.Binding.TaskID).Scan(&data); err != nil {
		return err
	}
	var saved background.Binding
	if json.Unmarshal(data, &saved) != nil || saved != scope.Binding {
		return harness.ErrTaskOriginConflict
	}
	taskID := scope.Binding.TaskID
	var old string
	err = tx.QueryRowContext(ctx, `SELECT contract FROM harness_background_engine_contracts WHERE task_id=?`, taskID).Scan(&old)
	if err == nil {
		if old != contract {
			return harness.ErrTaskOriginConflict
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		if _, err = tx.ExecContext(ctx, `INSERT INTO harness_background_engine_contracts VALUES(?,?)`, taskID, contract); err != nil {
			return err
		}
	} else {
		return err
	}
	return tx.Commit()
}
