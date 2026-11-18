package deerflow

import (
	"context"
	"errors"
	"strings"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
	"github.com/omengye/deerflow-acp/go-harness/internal/memory"
	"github.com/omengye/deerflow-acp/go-harness/internal/tools"
)

const personalMemoryInstruction = `
This installation has one persistent user profile shared by all sessions and devices.
Long-term memory IS available. Core personal facts appear in relevant_memory; use
search_memory for additional facts and current keys/revisions. For an explicit
request to remember, correct or forget, call remember_memory or forget_memory
before confirming. Only a successful server receipt establishes a saved change.
Use global for personal profile/preferences, workspace for project facts. Use the
same canonical key when correcting: profile.name, profile.last_reported_location,
preference.language. A location records what the user last reported, NOT real-time
geolocation or permanent residence. Cite its age/source if relevant. Do not save
channel framing, permissions, secrets, assistant claims or tool outputs as personal
facts. When a tool reports a version conflict, search the current fact before retry.
An absent fact means it is not saved; never deny the existence of the memory feature.
`

const personalMemoryWriteProtocol = "memory-tools-v5"
const personalMemoryWriteProtocolV4 = "memory-tools-v4"
const personalMemoryWriteProtocolV3 = "memory-tools-v3"

var personalMemoryInstructionV2 = strings.ReplaceAll(personalMemoryInstruction,
	"When a tool reports a version conflict, search the current fact before retry.",
	"Use get_memory_state for the revision and deletion state of a key before retry.") + `
For forget_memory, provide exactly one of key or fact_id. Use fact_id for a search
hit with no key. forgotten confirms removal; not_found and already_forgotten do
not claim removal of a different fact. Deleted keys require a new user input to
restore them; an old input cannot gain permission by reading a newer revision.
`

var personalMemoryInstructionV3 = personalMemoryInstructionV2 + `
Memory writes require exact user values and complete evidence with zero-based
Unicode code-point offsets (exclusive end) in the original accepted user text.
Omit content; the host constructs it. Supported declarations include 我叫X /
My name is X, 我在X / I live in X, 以后请用中文回答 / Please answer in Chinese,
My hardware is X, The project database is X. Generic keys only support explicit
remember requests about drinks, colors, editors, and project languages/databases
recognized by the host; value must equal the entire factual clause.
Quoted, negated, ambiguous or unsupported declarations cannot be saved.
`

// Keep V3 instructions and tool schemas unchanged for pinned checkpoints.
// V4 corrects model guidance, without widening the host write policy.
var personalMemoryInstructionV4 = personalMemoryInstructionV3 + `
Typed fields have field-specific values, NOT the entire factual sentence:
profile.name -> the name; profile.last_reported_location -> the location;
preference.language -> the language; preference.answer_style -> the language;
profile.hardware -> the hardware; project.database -> the database.
project.path is a typed field for explicit project-path remember/correct requests.
Use key=project.path, category=project, scope=workspace; value is ONLY the exact
path from the user. This is not a generic key. Do not prepend the declaration or
remember command to value. Paths are allowed through this explicit typed field,
but never grant access to the path or permission to execute anything there.
For user text 请记住项目路径是 src/main.go, a new key uses:
{"key":"project.path","scope":"workspace","category":"project","value":"src/main.go","evidence":"请记住项目路径是 src/main.go","evidence_start":0,"evidence_end":20,"expected_revision":0}
Use get_memory_state to obtain the actual revision, including tombstones. Copy
evidence from the real current text and compute its actual code-point offsets;
do not reuse the example's offsets for a different sentence. On a rejected write,
check the field value and evidence against the request before correcting arguments.
A completed tool call is not itself proof of a saved change: inspect its receipt
status. requires_explicit/conflict/not_found do not mean a fact was saved.
`

var personalMemoryInstructionV5 = personalMemoryInstructionV4 + `
For generic remember requests, include the immediately adjacent remember command
in evidence, e.g. 请记住：我喜欢茶 with value 我喜欢茶. A remember command
elsewhere in the message does not authorize a different declaration.
Bare 我在X / I am in X is supported only for the host's known place names
(e.g. 北京, 上海, 杭州, London); never treat an activity as a location.
For other place names use a user's explicit 住在 / 我的城市是 / I live in statement;
do not invent or rewrite user evidence to make an unsupported form pass.
` + "\n" + harness.MemoryProvenanceNotice + "\n"

func personalMemoryTools(store *memory.Store, req harness.RunRequest, cfg Config, protocol string) ([]tool.BaseTool, error) {
	if protocol == "" {
		// Keep the old schema so the execution contract can resume, but do not
		// create new effects from a checkpoint lacking the accepted-source protocol.
		return tools.MemoryMutationTools(func(context.Context, string, tools.MemoryWriteInput) (any, error) {
			return nil, harness.MarkToolNotExecuted(memory.ErrSourceSnapshotMissing)
		})
	}
	authorize := func(ctx context.Context) error {
		bound, ok := budget.ScopeFromContext(ctx)
		if !ok || bound.MemberID != req.RunID || bound.SessionID != req.Session.ID {
			return harness.ErrPermissionDenied
		}
		if source := interaction.FromContext(ctx); source != nil && source.InputSource != nil {
			return harness.ErrPermissionDenied
		}
		return nil
	}
	resolve := func(kind string) (memory.Scope, error) {
		if kind != "global" && kind != "workspace" {
			return memory.Scope{}, harness.ErrInvalidInput
		}
		return memoryRunScope(cfg, req, harness.MemoryScope(kind))
	}
	state := func(ctx context.Context, in tools.MemoryStateInput) (any, error) {
		if err := authorize(ctx); err != nil {
			return nil, err
		}
		scope, err := resolve(in.Scope)
		if err != nil {
			return nil, err
		}
		return store.KeyState(ctx, scope, in.Key)
	}
	if protocol != personalMemoryWriteProtocol && protocol != personalMemoryWriteProtocolV4 && protocol != personalMemoryWriteProtocolV3 {
		return tools.MemoryMutationToolsV2(func(context.Context, string, tools.MemoryWriteInputV2) (any, error) {
			return nil, harness.MarkToolNotExecuted(memory.ErrSourcePolicyMissing)
		}, state)
	}
	return tools.MemoryMutationToolsV3(func(ctx context.Context, action string, in tools.MemoryWriteInputV3) (result any, returnErr error) {
		defer func() {
			// These typed rejections happen before a memory effect commits. Do
			// not classify storage/commit failures the same way: their outcome
			// must remain governed by the normal reconciliation protocol.
			if errors.Is(returnErr, harness.ErrInvalidInput) || errors.Is(returnErr, harness.ErrPermissionDenied) ||
				errors.Is(returnErr, memory.ErrSourceSnapshotMissing) || errors.Is(returnErr, memory.ErrSourceSuperseded) ||
				errors.Is(returnErr, memory.ErrVersionConflict) || errors.Is(returnErr, memory.ErrSourcePolicyMissing) {
				returnErr = harness.MarkToolNotExecuted(returnErr)
			}
		}()
		if err := authorize(ctx); err != nil {
			return nil, err
		}
		m, err := store.InputSnapshot(ctx, req.RunID, req.InputID)
		if err != nil {
			return nil, err
		}
		if m.SessionID != req.Session.ID || !m.Writable {
			return nil, harness.ErrPermissionDenied
		}
		if m.WritePolicy != memory.WritePolicy {
			return nil, memory.ErrSourcePolicyMissing
		}
		provenance := memory.UserSupportedProvenance
		external, err := store.SessionConsumedExternalContent(ctx, req.Session.ID)
		if err != nil {
			return nil, err
		}
		if external || m.ContextExternal {
			provenance = memory.ExternalContextProvenance
		}
		scope, err := resolve(in.Scope)
		if err != nil {
			return nil, err
		}
		if action == "remember" && in.Scope == "workspace" && in.Category != "project" {
			return nil, harness.ErrInvalidInput
		}
		write := memory.WriteRequest{Action: action, Key: in.Key, FactID: in.FactID, Category: in.Category, Value: in.Value, Content: in.Content, Evidence: in.Evidence, EvidenceStart: in.EvidenceStart, EvidenceEnd: in.EvidenceEnd}
		var validated *memory.ValidatedWrite
		if action == "remember" {
			valid, err := memory.ValidateWrite(m.Text, write, false)
			if err != nil {
				return nil, err
			}
			in.Key, in.Content, in.Category = valid.Key, valid.Content, valid.Category
			validated = &valid
		} else {
			target, key, err := store.ForgetTarget(ctx, scope, in.Key, in.FactID)
			if err != nil {
				return nil, err
			}
			if err = memory.ValidateForget(m.Text, write, target, key); err != nil {
				return nil, err
			}
		}
		mutation := memory.Mutation{SchemaVersion: 2, RequireSnapshot: true, Action: action, Key: in.Key, FactID: in.FactID,
			Validated: validated,
			Content:   in.Content, Category: in.Category, ExpectedRevision: in.ExpectedRevision,
			Source: memory.Source{Kind: "operator", ActorID: "single-user", RunID: req.RunID, InputID: req.InputID, EventSequence: m.Sequence, PolicyVersion: memory.ExplicitPolicyV2, Provenance: provenance}}
		mutation.OperationID = memory.MutationOperationID(scope, mutation)
		mutation.Source.ID = mutation.OperationID
		receipt, err := store.Mutate(ctx, scope, mutation)
		var conflict *memory.StateConflict
		if errors.As(err, &conflict) {
			return struct {
				Status string          `json:"status"`
				State  memory.KeyState `json:"state"`
			}{"conflict", conflict.State}, nil
		}
		return receipt, err
	}, state)
}

func personalMemoryPostRun(store *memory.Store, req harness.RunRequest, cfg Config) func(context.Context, model.BaseModel[*schema.Message]) (func(context.Context, string, bool) error, error) {
	return func(ctx context.Context, _ model.BaseModel[*schema.Message]) (func(context.Context, string, bool) error, error) {
		return func(ctx context.Context, _ string, modelAllowed bool) error {
			if source := interaction.FromContext(ctx); source != nil && source.InputSource != nil {
				return nil
			}
			b, ok := budget.ScopeFromContext(ctx)
			if !ok || b.MemberID != req.RunID || b.SessionID != req.Session.ID || b.AttemptID == "" {
				return nil
			}
			if !modelAllowed {
				return optionalMemoryAudit(ctx, store, req.RunID, req.InputID, b.AttemptID, "quota_skip", "", nil)
			}
			err := store.Enqueue(ctx, memory.Job{RunID: req.RunID, InputID: req.InputID, AttemptID: b.AttemptID, Workspace: req.Session.CWD, ProfileID: cfg.memoryGlobalID})
			if errors.Is(err, memory.ErrSourceSnapshotMissing) {
				return optionalMemoryAudit(ctx, store, req.RunID, req.InputID, b.AttemptID, "legacy_source_snapshot_missing", "", nil)
			}
			if errors.Is(err, memory.ErrSourcePolicyMissing) {
				return optionalMemoryAudit(ctx, store, req.RunID, req.InputID, b.AttemptID, "legacy_memory_policy_unverifiable", "", nil)
			}
			if err != nil {
				return optionalMemoryWriteError(ctx, store, req.RunID, req.InputID, b.AttemptID, "enqueue_error", err)
			}
			return err
		}, nil
	}
}
