package deerflow

import (
	"context"
	"errors"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
	"github.com/omengye/deerflow-acp/go-harness/internal/memory"
)

func (c *Client) personalMemoryRow(ctx context.Context, scope memory.Scope, f memory.Fact) (map[string]any, error) {
	key, err := c.service.Memory.FactKey(ctx, scope, f.ID)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"id": f.ID, "key": key, "scope": "global", "revision": f.Revision,
		"content": f.Content, "category": f.Category, "created_at": f.CreatedAt, "updated_at": f.UpdatedAt,
		"source": map[string]any{"kind": f.Source.Kind, "run_id": f.Source.RunID, "input_id": f.Source.InputID, "policy": f.Source.PolicyVersion},
	}, nil
}

// Personal management uses per-fact CAS and durable receipts. It can safely
// coexist with foreground runs and never acquires that run's foreground slot.
func (c *Client) managementPersonalMemory(ctx context.Context, r localhost.ManagementRequest) (any, error) {
	if c.service.Memory == nil || c.service.MemoryGlobalID == "" {
		return nil, managementInvalid("personal memory unavailable")
	}
	scope, err := memory.NewScope(memory.GlobalScope, "", "", c.service.MemoryGlobalID, "")
	if err != nil {
		return nil, err
	}
	switch r.Operation {
	case "memory.personal.get":
		var in struct {
			Operation    string `json:"operation"`
			Cursor       string `json:"cursor"`
			Limit        int    `json:"limit"`
			FactsOnly    bool   `json:"facts_only"`
			ReviewFactID string `json:"review_fact_id"`
		}
		in.Limit = 100
		if err = decodeManagement(r, &in, "cursor", "limit", "facts_only", "review_fact_id"); err != nil {
			return nil, err
		}
		if in.Limit < 1 || in.Limit > 100 {
			return nil, managementInvalid("personal memory limit must be between 1 and 100")
		}
		_, review := r.Fields["review_fact_id"]
		if review && (in.ReviewFactID == "" || len(in.ReviewFactID) > 256 || strings.TrimSpace(in.ReviewFactID) != in.ReviewFactID || strings.ContainsRune(in.ReviewFactID, 0)) {
			return nil, managementInvalid("invalid personal memory review ID")
		}
		page, err := c.service.Memory.List(ctx, scope, in.Cursor, in.Limit)
		if err != nil {
			return nil, err
		}
		facts := []map[string]any{}
		for _, f := range page.Facts {
			row, err := c.personalMemoryRow(ctx, scope, f)
			if err != nil {
				return nil, err
			}
			facts = append(facts, row)
		}
		result := map[string]any{"profile_id": c.service.MemoryGlobalID, "facts": facts, "next_cursor": page.Next, "scope_revision": page.ScopeRevision}
		if review {
			fact, err := c.service.Memory.Get(ctx, scope, in.ReviewFactID)
			switch {
			case errors.Is(err, harness.ErrNotFound):
				result["reviewed_fact"] = nil
			case err != nil:
				return nil, err
			default:
				row, err := c.personalMemoryRow(ctx, scope, fact)
				if err != nil {
					return nil, err
				}
				result["reviewed_fact"] = row
			}
		}
		if in.FactsOnly {
			return result, nil
		}
		versions, err := c.service.Memory.SlotVersions(ctx, scope)
		if err != nil {
			return nil, err
		}
		jobs, err := c.service.Memory.JobStatus(ctx)
		if err != nil {
			return nil, err
		}
		result["versions"] = versions
		result["jobs"] = jobs
		return result, nil
	case "memory.personal.receipt":
		var in struct {
			Operation string `json:"operation"`
			ID        string `json:"operation_id"`
		}
		if err = decodeManagement(r, &in, "operation_id"); err != nil {
			return nil, err
		}
		receipt, err := c.service.Memory.Receipt(ctx, scope, in.ID)
		if errors.Is(err, harness.ErrNotFound) {
			return map[string]any{"found": false}, nil
		}
		if err != nil {
			return nil, err
		}
		return map[string]any{"found": true, "receipt": receipt}, nil
	case "memory.personal.mutate":
		var in struct {
			Operation string `json:"operation"`
			ProfileID string `json:"profile_id"`
			memory.Mutation
		}
		if err = decodeManagement(r, &in, "profile_id", "operation_id", "action", "key", "fact_id", "content", "category", "expected_revision", "schema_version"); err != nil {
			return nil, err
		}
		if in.ProfileID != c.service.MemoryGlobalID {
			return nil, managementInvalid("profile changed; reload personal memory")
		}
		in.Source = memory.Source{ID: in.OperationID, Kind: "operator", ActorID: memory.PersonalManagementActor, PolicyVersion: memory.ExplicitPolicy}
		receipt, err := c.service.Memory.Mutate(ctx, scope, in.Mutation)
		if errors.Is(err, memory.ErrVersionConflict) {
			return nil, &localhost.ManagementError{Code: "memory_conflict", Message: "memory changed; refresh before editing"}
		}
		if errors.Is(err, harness.ErrInvalidInput) {
			return nil, managementInvalid("invalid personal memory change")
		}
		return receipt, err
	}
	return nil, managementInvalid("unknown personal memory operation")
}
