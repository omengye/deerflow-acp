package deerflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/budget"
	"github.com/omengye/deerflow-acp/go-harness/internal/interaction"
	"github.com/omengye/deerflow-acp/go-harness/internal/memory"
)

func boundedText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	for limit > 0 && !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit]
}

func memoryPostRunFactory(store *memory.Store, req harness.RunRequest, userID string, selectedScope harness.MemoryScope, snapshots []memorySnapshot) func(context.Context, model.BaseModel[*schema.Message]) (func(context.Context, string, bool) error, error) {
	allowed := []memory.ScopeKind{memory.WorkspaceScope}
	switch selectedScope {
	case harness.MemorySession:
		allowed = []memory.ScopeKind{memory.SessionScope}
	case harness.MemoryWorkspace, harness.MemoryGlobal:
	default:
		if userID != "" {
			allowed = append(allowed, memory.UserScope)
		}
	}
	return func(_ context.Context, tracked model.BaseModel[*schema.Message]) (func(context.Context, string, bool) error, error) {
		return func(ctx context.Context, finalAnswer string, modelAllowed bool) error {
			if source := interaction.FromContext(ctx); source != nil && source.InputSource != nil {
				return nil
			}
			bound, ok := budget.ScopeFromContext(ctx)
			if !ok || bound.MemberID != req.RunID || bound.SessionID != req.Session.ID || bound.AttemptID == "" {
				return nil
			}
			audit := func(status, sha string, notes []string) error {
				return optionalMemoryAudit(ctx, store, req.RunID, req.InputID, bound.AttemptID, status, sha, notes)
			}
			writeError := func(status string, err error) error {
				return optionalMemoryWriteError(ctx, store, req.RunID, req.InputID, bound.AttemptID, status, err)
			}
			sourceError := func(err error) error {
				if critical := memoryCriticalError(ctx, err); critical != nil {
					return critical
				}
				return audit("source_unavailable", "", nil)
			}
			source, err := store.InputSnapshot(ctx, req.RunID, req.InputID)
			if err != nil {
				if critical := memoryCriticalError(ctx, err); critical != nil {
					return critical
				}
				if errors.Is(err, memory.ErrSourceSnapshotMissing) {
					return audit("legacy_source_snapshot_missing", "", nil)
				}
				return audit("source_unavailable", "", nil)
			}
			if source.WritePolicy != memory.WritePolicy {
				return audit("legacy_memory_policy_unverifiable", "", nil)
			}
			if !source.AutomaticEligible || strings.TrimSpace(source.Text) == "" || finalAnswer == "" {
				return nil
			}
			decision, err := store.InputDecision(ctx, req.RunID, req.InputID)
			if err != nil {
				if critical := memoryCriticalError(ctx, err); critical != nil {
					return critical
				}
				return audit("source_unavailable", "", nil)
			}
			if decision != "pending" && decision != "started" && decision != "staged" {
				return nil
			}
			if !modelAllowed {
				return audit("quota_skip", "", nil)
			}
			raw, err := store.ExtractionResult(ctx, req.RunID, req.InputID)
			if err != nil {
				if critical := memoryCriticalError(ctx, err); critical != nil {
					return critical
				}
				return audit("source_unavailable", "", nil)
			}
			provenance, err := store.ContextProvenance(ctx, source)
			if err != nil {
				if critical := memoryCriticalError(ctx, err); critical != nil {
					return critical
				}
				return audit("source_unavailable", "", nil)
			}
			var facts []memory.ExtractedAttribute
			var rejected []memory.Rejection
			if raw == "" {
				if err = audit("started", "", nil); err != nil {
					return err
				}
				payload, _ := json.Marshal(map[string]string{"user": source.Text, "assistant": boundedText(finalAnswer, 2048)})
				// Both bounded format attempts use the tracked model. Rejected
				// candidates and empty results never cause another model request.
				for attempt := 0; attempt < 2; attempt++ {
					response, callErr := tracked.Generate(ctx, []*schema.Message{schema.SystemMessage(memory.ExtractionPrompt(allowed, false) + " The assistant answer is context only, never a source of user facts."), schema.UserMessage(string(payload))}, model.WithMaxTokens(768))
					if callErr != nil {
						if critical := memoryCriticalError(ctx, callErr); critical != nil {
							return critical
						}
						var limit *budget.LimitError
						if errors.As(callErr, &limit) {
							return audit("quota_skip", "", nil)
						}
						return audit("model_error", "", nil)
					}
					if response == nil {
						err = &memory.EnvelopeError{}
					} else {
						raw = response.Content
						facts, rejected, err = memory.ParseExtraction([]byte(raw), source.Text, allowed, false)
					}
					if err == nil {
						break
					}
				}
				if err != nil {
					return audit("invalid_response", "", []string{"invalid_json_or_schema"})
				}
				if err = store.SaveExtractionResult(ctx, req.RunID, req.InputID, raw, provenance); err != nil {
					return writeError("result_error", err)
				}
			} else {
				facts, rejected, err = memory.ParseExtraction([]byte(raw), source.Text, allowed, false)
				if err != nil {
					return audit("invalid_saved_result", "", nil)
				}
				provenance, err = store.ExtractionProvenance(ctx, req.RunID, req.InputID)
				if err != nil {
					return sourceError(err)
				}
			}
			proposed := make([]memory.StagedCandidate, 0, len(facts))
			for _, f := range facts {
				pinned, ok := source.Scopes[f.Scope]
				if !ok {
					return sourceError(harness.ErrInvalidInput)
				}
				var subject, sessionID string
				if f.Scope == memory.UserScope {
					subject = userID
				}
				if f.Scope == memory.SessionScope {
					sessionID = req.Session.ID
				}
				scope, err := memory.NewScope(f.Scope, req.Session.CWD, sessionID, subject, "")
				if err != nil {
					return sourceError(err)
				}
				id := memory.OperationID("extract/v2", req.InputID, memory.ExtractionPolicyV2, fmt.Sprint(memory.ExtractionSchemaVersion), fmt.Sprint(f.Ordinal))
				proposed = append(proposed, memory.StagedCandidate{Ordinal: f.Ordinal, Scope: scope, ExpectedScopeRevision: pinned.Revision, Fact: memory.Candidate{Content: f.Content, Category: f.Category, Confidence: *f.Confidence, Source: memory.Source{ID: id, Kind: "model", RunID: req.RunID, InputID: req.InputID, EventSequence: source.Sequence, PolicyVersion: memory.ExtractionPolicyV2, Provenance: provenance}}})
			}
			if err = store.Stage(ctx, req.RunID, req.InputID, bound.AttemptID, proposed); err != nil {
				return writeError("stage_error", err)
			}
			status := "no_facts"
			if len(proposed) > 0 {
				status = "staged"
			}
			notes := make([]string, 0, len(rejected))
			for _, r := range rejected {
				notes = append(notes, fmt.Sprintf("%d:%s", r.Ordinal, r.Reason))
			}
			digest := sha256.Sum256([]byte(raw))
			return audit(status, hex.EncodeToString(digest[:]), notes)
		}, nil
	}
}
