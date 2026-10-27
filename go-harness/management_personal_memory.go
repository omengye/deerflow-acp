package deerflow

import (
	"context"
	"errors"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
	"github.com/omengye/deerflow-acp/go-harness/internal/memory"
)

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
			Operation string `json:"operation"`
			Cursor    string `json:"cursor"`
		}
		if err = decodeManagement(r, &in, "cursor"); err != nil {
			return nil, err
		}
		page, err := c.service.Memory.List(ctx, scope, in.Cursor, 100)
		if err != nil {
			return nil, err
		}
		facts := []map[string]any{}
		for _, f := range page.Facts {
			key, err := c.service.Memory.FactKey(ctx, scope, f.ID)
			if err != nil {
				return nil, err
			}
			facts = append(facts, map[string]any{"id": f.ID, "key": key, "scope": "global", "revision": f.Revision, "content": f.Content, "category": f.Category, "created_at": f.CreatedAt, "updated_at": f.UpdatedAt, "source": map[string]any{"kind": f.Source.Kind, "run_id": f.Source.RunID, "input_id": f.Source.InputID, "policy": f.Source.PolicyVersion}})
		}
		versions, err := c.service.Memory.SlotVersions(ctx, scope)
		if err != nil {
			return nil, err
		}
		jobs, err := c.service.Memory.JobStatus(ctx)
		if err != nil {
			return nil, err
		}
		return map[string]any{"profile_id": c.service.MemoryGlobalID, "facts": facts, "versions": versions, "next_cursor": page.Next, "jobs": jobs, "scope_revision": page.ScopeRevision}, nil
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
		if err = decodeManagement(r, &in, "profile_id", "operation_id", "action", "key", "content", "category", "expected_revision"); err != nil {
			return nil, err
		}
		if in.ProfileID != c.service.MemoryGlobalID {
			return nil, managementInvalid("profile changed; reload personal memory")
		}
		in.Source = memory.Source{ID: in.OperationID, Kind: "operator", ActorID: "single-user-management", PolicyVersion: "personal-explicit-v1"}
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
