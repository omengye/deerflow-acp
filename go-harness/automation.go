package deerflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/automation"
	"github.com/omengye/deerflow-acp/go-harness/internal/channels"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
	"github.com/omengye/deerflow-acp/go-harness/internal/wechat"
)

type WeChatConfig = wechat.Config

// Each background dispatch gets an owner lease. It never steals a desktop
// session, increases permission policy, or resumes an interrupted execution.
func (c *Client) unattended(ctx context.Context, existing, workspace, prompt, approval string, bind func(string) error) (out automation.Outcome, err error) {
	out, _, err = c.unattendedWithEvents(ctx, existing, workspace, prompt, approval, bind, nil)
	return out, err
}

func (c *Client) unattendedWithEvents(ctx context.Context, existing, workspace, prompt, approval string, bind func(string) error, observe harness.EventHandler) (out automation.Outcome, execution *harness.ExecutionState, err error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	owner := hr.NewID()
	defer func() { err = errors.Join(err, c.service.Disconnect(context.Background(), owner)) }()
	var x harness.Session
	if existing == "" {
		x, err = c.service.NewSession(ctx, owner, workspace)
	} else {
		x, err = c.service.Load(ctx, owner, existing, workspace, false, nil)
	}
	if err != nil {
		return out, nil, err
	}
	out.SessionID = x.ID
	if _, scheduled := ctx.Value(scheduledRunKey{}).(bool); scheduled {
		if err = c.bindScheduleSource(ctx, x.ID, automation.Source{Kind: "scheduled", Workspace: workspace}); err != nil {
			return out, nil, err
		}
	} else if d, ok := channels.DispatchFromContext(ctx); ok {
		if err = c.bindScheduleSource(ctx, x.ID, c.channels.ScheduleSource(d, x.ID)); err != nil {
			return out, nil, err
		}
	}
	if err = bind(x.ID); err != nil {
		return out, nil, err
	}
	if existing != "" {
		state, stateErr := c.service.Execution(ctx, owner, x.ID, "")
		if stateErr != nil && !errors.Is(stateErr, harness.ErrNotFound) {
			return out, nil, stateErr
		}
		if stateErr == nil && (state.Status == harness.ExecutionWaitingInput || state.Status == harness.ExecutionSuspending || state.Status == harness.ExecutionResuming) {
			out.Status = "needs_attention"
			return out, &state, nil
		}
	}
	if approval == "" {
		approval = harness.ApprovalReadOnly
	}
	if _, err = c.service.SetConfigOption(ctx, owner, x.ID, "approval", approval); err != nil {
		return out, nil, err
	}
	var mu sync.Mutex
	var answer strings.Builder
	emit := func(ctx context.Context, e harness.RunEvent) error {
		if e.Kind == "text_delta" && (e.AgentName == "" || e.AgentName == "deerflow") {
			mu.Lock()
			defer mu.Unlock()
			if answer.Len() < 16000 {
				answer.WriteString(e.Text[:min(len(e.Text), 16000-answer.Len())])
			}
		}
		if observe != nil {
			return observe(ctx, e)
		}
		return nil
	}
	var approve harness.PermissionHandler
	if approval == harness.ApprovalGlobal || observe != nil {
		// The runtime applies global policy first. Leave remaining native
		// permission intents pending for desktop review, never auto-reject them.
		approve = func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
			return harness.PermissionCancelled, nil
		}
	}
	result, err := c.service.Run(ctx, owner, x.ID, []harness.Content{{Type: "text", Text: prompt}}, emit, approve)
	out.Text = strings.ToValidUTF8(answer.String(), "")
	out.Status = "needs_attention"
	if err == nil && result.StopReason == "end_turn" && (result.Execution == nil || result.Execution.Status == harness.ExecutionCompleted) {
		out.Status = "completed"
	}
	return out, result.Execution, err
}

func (c *Client) startAutomation(ctx context.Context, cfg Config) error {
	var err error
	c.scheduler, err = automation.New(ctx, c.store.DB(), func(ctx context.Context, spec automation.Spec, bind func(string) error) (automation.Outcome, error) {
		ctx = context.WithValue(ctx, scheduledRunKey{}, true)
		if spec.Source != nil {
			if err := c.authorizeScheduleSource(ctx, *spec.Source, true); err != nil {
				return automation.Outcome{}, err
			}
		}
		return c.unattended(ctx, "", spec.Workspace, spec.Prompt, spec.Approval, bind)
	})
	if err != nil {
		return err
	}
	c.channels, err = channels.New(ctx, c.store.DB(), c.channelRun)
	if err != nil {
		return err
	}
	if err = c.channels.EnsureBundled(ctx); err != nil {
		return err
	}
	if err = c.channels.MigrateWeChat(ctx, channels.Config{Enabled: cfg.WeChat.Enabled, Workspace: cfg.WeChat.Workspace, LegacyAllowed: cfg.WeChat.AllowedUsers, LegacyApproval: cfg.WeChat.Approval}); err != nil {
		return err
	}
	c.scheduler.NotifyTx = c.enqueueScheduleNotification
	c.scheduler.Start(context.Background())
	c.channels.Start()
	return nil
}

func decodeManagement(request localhost.ManagementRequest, out any, keys ...string) error {
	allowed := map[string]bool{"operation": true}
	for _, key := range keys {
		allowed[key] = true
	}
	for key := range request.Fields {
		if !allowed[key] {
			return managementInvalid("unsupported field: " + key)
		}
	}
	fields := map[string]json.RawMessage{}
	for k, v := range request.Fields {
		if k != "operation" {
			fields[k] = v
		}
	}
	raw, _ := json.Marshal(fields)
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return managementInvalid("invalid management parameters")
	}
	return nil
}

func (c *Client) managementAutomation(ctx context.Context, r localhost.ManagementRequest) (any, error) {
	var in struct {
		ID       string            `json:"id"`
		Spec     automation.Spec   `json:"spec"`
		Paused   bool              `json:"paused"`
		Verify   string            `json:"verify_code"`
		Revision int64             `json:"expected_revision"`
		Timing   automation.Timing `json:"timing"`
		Rearm    bool              `json:"rearm"`
	}
	var keys []string
	switch r.Operation {
	case "schedule.create":
		keys = []string{"spec"}
	case "schedule.pause":
		keys = []string{"id", "paused", "expected_revision"}
	case "schedule.delete":
		keys = []string{"id", "expected_revision"}
	case "schedule.reschedule":
		keys = []string{"id", "expected_revision", "timing", "rearm"}
	case "schedule.preview":
		keys = []string{"timing"}
	case "schedule.snapshot", "schedule.history", "schedule.ack", "wechat.review":
		keys = []string{"id"}
	case "wechat.login.poll":
		keys = []string{"id", "verify_code"}
	}
	if err := decodeManagement(r, &in, keys...); err != nil {
		return nil, err
	}
	switch r.Operation {
	case "schedule.create":
		target := in.Spec.Notify
		if in.Spec.NotifyWeChat != "" {
			target = c.channels.LegacyTarget(in.Spec.NotifyWeChat)
		}
		if (in.Spec.NotifyWeChat != "" || in.Spec.Notify != nil) && !c.channels.CanNotify(target) {
			return nil, managementInvalid("notification requires a bound sender on an enabled channel")
		}
		workspace, err := session.NormalizeWorkspace(in.Spec.Workspace)
		if err != nil {
			return nil, managementInvalid("workspace does not exist or is invalid")
		}
		in.Spec.Workspace = workspace
		v, err := c.scheduler.Create(ctx, in.Spec)
		if err != nil {
			return nil, managementInvalid(err.Error())
		}
		return v, nil
	case "schedule.list":
		return c.scheduleList(ctx)
	case "schedule.snapshot":
		items, err := c.scheduleList(ctx)
		if err != nil {
			return nil, err
		}
		// Cards are bounded projections, not editable copies of task authority.
		// Keep even 200 maximum-sized prompts plus history below the IPC limit.
		for i := range items {
			items[i].Spec.Prompt = scheduleSummary(items[i].Spec.Prompt)
		}
		var history []automation.Occurrence
		if in.ID != "" {
			history, err = c.scheduleHistory(ctx, in.ID)
			if err != nil {
				return nil, err
			}
			for i := range history {
				history[i].Outcome.Text = scheduleSummary(history[i].Outcome.Text)
			}
		}
		return map[string]any{"items": items, "status": c.scheduler.Status(), "history": history}, nil
	case "schedule.preview":
		v, err := in.Timing.Preview(time.Now())
		return v, scheduleManagementError(err)
	case "schedule.reschedule":
		v, err := c.scheduler.Reschedule(ctx, in.ID, in.Revision, in.Timing, in.Rearm)
		return v, scheduleManagementError(err)
	case "schedule.status":
		return c.scheduler.Status(), nil
	case "schedule.pause":
		if in.ID == "" || (string(r.Fields["paused"]) != "true" && string(r.Fields["paused"]) != "false") {
			return nil, managementInvalid("id and paused are required")
		}
		return map[string]any{"id": in.ID, "paused": in.Paused}, scheduleManagementError(c.scheduler.SetPaused(ctx, in.ID, in.Paused, in.Revision))
	case "schedule.history":
		return c.scheduleHistory(ctx, in.ID)
	case "schedule.ack":
		return map[string]bool{"acknowledged": true}, c.scheduler.Acknowledge(ctx, in.ID)
	case "schedule.delete":
		return map[string]bool{"deleted": true}, scheduleManagementError(c.scheduler.Delete(ctx, in.ID, in.Revision))
	case "wechat.status":
		value, err := c.channels.Status(ctx, "wechat")
		if data, ok := value.(map[string]any); ok {
			data["bot_id"] = data["account"]
		}
		return value, channelManagementError(err)
	case "wechat.history":
		value, err := c.channels.History(ctx, "wechat")
		if rows, ok := value.([]any); ok {
			for _, row := range rows {
				if data, ok := row.(map[string]any); ok {
					data["bot_id"] = data["account"]
				}
			}
		}
		return value, err
	case "wechat.login.start":
		return c.channels.Login(ctx, "wechat", "login.start", "", "")
	case "wechat.login.poll":
		value, err := c.channels.Login(ctx, "wechat", "login.poll", in.ID, in.Verify)
		if data, ok := value.(map[string]any); ok {
			data["bot_id"], data["user_id"] = data["account"], data["owner"]
			data["status"] = data["state"]
			switch data["state"] {
			case "pending":
				data["status"] = "wait"
			case "scanned":
				data["status"] = "scaned"
			case "verification_required":
				data["status"] = "need_verifycode"
			case "binding_required":
				data["status"] = "confirmed"
			}
		}
		return value, channelManagementError(err)
	case "wechat.logout":
		return c.channels.Login(ctx, "wechat", "logout", "", "")
	case "wechat.review":
		return map[string]bool{"reviewed_without_replay": true}, c.channels.Review(ctx, "wechat", in.ID)
	default:
		return nil, managementInvalid("unsupported automation operation")
	}
}

func scheduleSummary(value string) string {
	text := []rune(value)
	if len(text) > 512 {
		return string(text[:512]) + "..."
	}
	return value
}

func scheduleManagementError(err error) error {
	if err == nil {
		return nil
	}
	code := "operation_failed"
	switch {
	case errors.Is(err, automation.ErrConflict):
		code = "conflict"
	case errors.Is(err, automation.ErrBusy):
		code = "busy"
	case errors.Is(err, automation.ErrNotFound):
		code = "not_found"
	case errors.Is(err, automation.ErrInvalid):
		code = "invalid_request"
	default:
		return err
	}
	return &localhost.ManagementError{Code: code, Message: err.Error()}
}
