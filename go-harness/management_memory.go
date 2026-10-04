package deerflow

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
	"github.com/omengye/deerflow-acp/go-harness/internal/memory"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
)

func (c *Client) managementMemory(ctx context.Context, request localhost.ManagementRequest) (any, error) {
	if c.service.Memory == nil {
		return nil, managementInvalid("memory is unavailable")
	}
	mutation := request.Operation != "memory.get"
	for key := range request.Fields {
		if key == "operation" || key == "session_id" || key == "scope" {
			continue
		}
		if mutation && (key == "fact_id" || key == "expected_revision") {
			continue
		}
		return nil, managementInvalid("unexpected memory management fields")
	}
	var selected harness.MemoryScope
	if raw, ok := request.Fields["scope"]; ok {
		if json.Unmarshal(raw, &selected) != nil || selected == "" {
			return nil, managementInvalid("invalid memory scope")
		}
	}
	var x harness.Session
	if _, ok := request.Fields["session_id"]; ok || selected != harness.MemoryGlobal {
		id, err := managementID(request.Fields, "session_id")
		if err != nil {
			return nil, err
		}
		x, err = c.service.Store.ManagementSession(ctx, id)
		if errors.Is(err, harness.ErrNotFound) {
			return nil, managementInvalid("session does not exist")
		}
		if err != nil {
			return nil, err
		}
	}
	var scopes []managementScope
	var err error
	if selected == harness.MemoryGlobal {
		if c.service.MemoryGlobalID == "" {
			return nil, managementInvalid("global memory is unavailable")
		}
		global, scopeErr := memory.NewScope(memory.GlobalScope, "", "", c.service.MemoryGlobalID, "")
		if scopeErr != nil {
			return nil, scopeErr
		}
		scopes = []managementScope{{kind: harness.MemoryGlobal, scope: global}}
	} else {
		all, scopeErr := c.managementMemoryScopes(x)
		if scopeErr != nil {
			return nil, scopeErr
		}
		for _, item := range all {
			if selected == "" || selected == item.kind {
				scopes = append(scopes, item)
			}
		}
		if len(scopes) == 0 {
			return nil, managementInvalid("invalid memory scope")
		}
	}
	if !mutation {
		return c.managementMemorySnapshot(ctx, x, scopes)
	}
	factID, err := managementID(request.Fields, "fact_id")
	if err != nil {
		return nil, err
	}
	var expected int64
	if raw, ok := request.Fields["expected_revision"]; ok {
		if json.Unmarshal(raw, &expected) != nil || expected < 1 {
			return nil, managementInvalid("invalid memory revision")
		}
	} else if request.Operation == "memory.promote" {
		return nil, managementInvalid("promotion requires the reviewed memory revision")
	}
	var found *managementScope
	var revision int64
	for i := range scopes {
		fact, getErr := c.service.Memory.Get(ctx, scopes[i].scope, factID)
		if getErr == nil {
			found, revision = &scopes[i], fact.Revision
			break
		}
		if !errors.Is(getErr, harness.ErrNotFound) {
			return nil, getErr
		}
	}
	if found == nil {
		return nil, managementInvalid("fact does not exist in the selected scopes")
	}
	if expected != 0 {
		revision = expected
	}
	prior := c.service.Coordinator.Activity().Draining
	priorBackground := false
	c.service.Coordinator.SetDraining(true)
	backgroundOperations := 0
	if c.background != nil {
		priorBackground = c.background.service.Activity().Draining
		backgroundOperations = c.background.service.SetDraining(true).ActiveOperations
	}
	defer func() {
		if c.background != nil {
			c.background.service.SetDraining(priorBackground)
		}
		c.service.Coordinator.SetDraining(prior)
	}()
	if c.service.Coordinator.Activity().ActiveOperations+backgroundOperations != 0 {
		return nil, &localhost.ManagementError{Code: "busy", Message: "wait for active operations before changing memory"}
	}
	source := memory.Source{ID: hr.NewID(), Kind: "operator", ActorID: "local-management", PolicyVersion: "local-management-v1"}
	var promoted memory.Fact
	if request.Operation == "memory.promote" {
		global, scopeErr := memory.NewScope(memory.GlobalScope, "", "", c.service.MemoryGlobalID, "")
		if scopeErr != nil {
			return nil, scopeErr
		}
		promoted, err = c.service.Memory.Promote(ctx, found.scope, global, factID, revision, source)
	} else {
		err = c.service.Memory.Delete(ctx, found.scope, factID, revision, source)
	}
	if errors.Is(err, memory.ErrVersionConflict) {
		return nil, &localhost.ManagementError{Code: "memory_conflict", Message: "memory changed; reload the scope and review its current revision"}
	}
	if err != nil {
		return nil, err
	}
	snapshot, err := c.managementMemorySnapshot(ctx, x, scopes)
	if err == nil && request.Operation == "memory.promote" {
		snapshot.(map[string]any)["promoted"] = map[string]any{"id": promoted.ID, "scope": harness.MemoryGlobal, "revision": promoted.Revision}
	}
	return snapshot, err
}
