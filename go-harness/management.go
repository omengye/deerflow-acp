package deerflow

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/internal/background"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
)

func managementInvalid(message string) error {
	return &localhost.ManagementError{Code: "invalid_request", Message: message}
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
	default:
		return nil, &localhost.ManagementError{Code: "unsupported_operation", Message: fmt.Sprintf("unsupported management operation: %s", request.Operation)}
	}
}
