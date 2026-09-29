package deerflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/background"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
	"github.com/omengye/deerflow-acp/go-harness/internal/memory"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
)

func managementInvalid(message string) error {
	return &localhost.ManagementError{Code: "invalid_request", Message: message}
}

func managementID(fields map[string]json.RawMessage, key string) (string, error) {
	var value string
	if json.Unmarshal(fields[key], &value) != nil || value == "" || len(value) > 256 || strings.TrimSpace(value) != value {
		return "", managementInvalid("invalid " + key)
	}
	return value, nil
}

type managementScope struct {
	kind  harness.MemoryScope
	scope memory.Scope
}

func (c *Client) managementMemoryScopes(x harness.Session) ([]managementScope, error) {
	options := []harness.MemoryScope{harness.MemorySession, harness.MemoryWorkspace}
	if c.service.MemoryUserID != "" {
		options = append(options, harness.MemoryUser)
	}
	result := make([]managementScope, 0, len(options))
	for _, kind := range options {
		var sessionID, userID string
		if kind == harness.MemorySession {
			sessionID = x.ID
		} else if kind == harness.MemoryUser {
			userID = c.service.MemoryUserID
		}
		scope, err := memory.NewScope(memory.ScopeKind(kind), x.CWD, sessionID, userID, "")
		if err != nil {
			return nil, err
		}
		result = append(result, managementScope{kind: kind, scope: scope})
	}
	return result, nil
}

func (c *Client) managementMemorySnapshot(ctx context.Context, x harness.Session, scopes []managementScope) (any, error) {
	facts := make([]map[string]any, 0)
	bytesUsed := 0
	truncated := false
	labels := make([]string, 0, len(scopes))
	for _, item := range scopes {
		labels = append(labels, string(item.kind))
	}
	for _, item := range scopes {
		cursor := ""
		for {
			page, err := c.service.Memory.List(ctx, item.scope, cursor, 100)
			if err != nil {
				return nil, err
			}
			for _, fact := range page.Facts {
				entry := map[string]any{"id": fact.ID, "scope": item.kind, "revision": fact.Revision, "content": fact.Content, "category": fact.Category, "confidence": fact.Confidence}
				encoded, err := json.Marshal(entry)
				if err != nil {
					return nil, err
				}
				if len(facts) == 1000 || bytesUsed+len(encoded) > 3<<20 {
					truncated = true
					return map[string]any{"workspace": x.CWD, "scope": strings.Join(labels, "/"), "memory": map[string]any{"facts": facts}, "truncated": truncated}, nil
				}
				bytesUsed += len(encoded)
				facts = append(facts, entry)
			}
			if page.Next == "" {
				break
			}
			cursor = page.Next
		}
	}
	return map[string]any{"workspace": x.CWD, "scope": strings.Join(labels, "/"), "memory": map[string]any{"facts": facts}, "truncated": truncated}, nil
}

func (c *Client) managementMemory(ctx context.Context, request localhost.ManagementRequest) (any, error) {
	want := 2
	if request.Operation == "memory.delete" {
		want = 3
	}
	if len(request.Fields) != want {
		return nil, managementInvalid("unexpected memory management fields")
	}
	sessionID, err := managementID(request.Fields, "session_id")
	if err != nil {
		return nil, err
	}
	x, err := c.service.Store.ManagementSession(ctx, sessionID)
	if errors.Is(err, harness.ErrNotFound) {
		return nil, managementInvalid("session does not exist")
	}
	if err != nil {
		return nil, err
	}
	scopes, err := c.managementMemoryScopes(x)
	if err != nil {
		return nil, err
	}
	if request.Operation == "memory.get" {
		return c.managementMemorySnapshot(ctx, x, scopes)
	}
	factID, err := managementID(request.Fields, "fact_id")
	if err != nil {
		return nil, err
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
		return nil, managementInvalid("fact does not exist in this session's scopes")
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
		return nil, &localhost.ManagementError{Code: "busy", Message: "wait for active operations before deleting memory"}
	}
	err = c.service.Memory.Delete(ctx, found.scope, factID, revision, memory.Source{ID: hr.NewID(), Kind: "operator", ActorID: "local-management", PolicyVersion: "local-management-v1"})
	if err != nil {
		return nil, err
	}
	return c.managementMemorySnapshot(ctx, x, scopes)
}

func managementStatus(activity session.Activity, tasks background.Activity, connections int) map[string]any {
	return map[string]any{
		"draining":          activity.Draining,
		"active_operations": activity.ActiveOperations + tasks.ActiveOperations,
		"active_runs":       activity.ActiveOperations + tasks.ActiveRuns,
		"queued_runs":       tasks.QueuedRuns, // The foreground coordinator does not queue runs.
		"connections":       connections,
	}
}

// ManageLocal serves only the authenticated DFACP/1 local control channel.
// It is deliberately separate from ACP methods and does not accept a session
// owner supplied by a client connection.
func (c *Client) ManageLocal(ctx context.Context, request localhost.ManagementRequest) (any, error) {
	done, err := c.operation()
	if err != nil {
		return nil, err
	}
	defer done()
	c.manageMu.Lock()
	defer c.manageMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch request.Operation {
	case "daemon.status", "daemon.drain", "daemon.resume":
		if len(request.Fields) != 1 {
			return nil, managementInvalid("daemon status accepts only operation")
		}
		var activity session.Activity
		if request.Operation == "daemon.drain" {
			activity = c.service.Coordinator.SetDraining(true)
			if c.background != nil {
				c.background.service.SetDraining(true)
			}
		} else if request.Operation == "daemon.resume" {
			if c.background != nil {
				c.background.service.SetDraining(false)
			}
			activity = c.service.Coordinator.SetDraining(false)
		} else {
			activity = c.service.Coordinator.Activity()
		}
		var tasks background.Activity
		if c.background != nil {
			tasks = c.background.service.Activity()
		}
		return managementStatus(activity, tasks, request.ActiveConnections), nil
	case "session.list":
		if len(request.Fields) != 1 {
			return nil, managementInvalid("session list accepts only operation")
		}
		listed, err := c.service.Store.ListForManagement(ctx, 1001)
		if err != nil {
			return nil, err
		}
		truncated := len(listed) > 1000
		if truncated {
			listed = listed[:1000]
		}
		activity := c.service.Coordinator.Activity()
		sessions := make([]map[string]any, 0, len(listed))
		inventoryBytes := 0
		for _, x := range listed {
			var phase any
			if current, exists := activity.Phases[x.ID]; exists {
				phase = current
			}
			entry := map[string]any{
				"session_id":       x.ID,
				"cwd":              x.CWD,
				"title":            x.Title,
				"updated_at":       x.UpdatedAt.Format(time.RFC3339Nano),
				"model_name":       x.Model,
				"subagent_enabled": x.Subagents,
				"plan_mode":        x.Mode == "plan",
				"approval_mode":    x.ApprovalMode,
				"phase":            phase,
				"cleanup_eligible": false, // Retention and purge are not implemented.
			}
			encoded, err := json.Marshal(entry)
			if err != nil {
				return nil, err
			}
			if inventoryBytes+len(encoded) > 3<<20 {
				truncated = true
				break
			}
			inventoryBytes += len(encoded)
			sessions = append(sessions, entry)
		}
		return map[string]any{"sessions": sessions, "truncated": truncated}, nil
	case "memory.get", "memory.delete":
		return c.managementMemory(ctx, request)
	default:
		return nil, &localhost.ManagementError{Code: "unsupported_operation", Message: fmt.Sprintf("unsupported management operation: %s", request.Operation)}
	}
}
