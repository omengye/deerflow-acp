package wechat

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/channelproto"
)

func validFeedbackText(value string, limit int) bool {
	return strings.TrimSpace(value) != "" && len(value) <= limit && utf8.ValidString(value) && strings.IndexFunc(value, unicode.IsControl) < 0
}

func (c *Client) SendProgress(ctx context.Context, cred Credentials, to, token, key string, tool channelproto.ToolProgress) error {
	if !validFeedbackText(to, 1024) || !validFeedbackText(token, 8192) || !validFeedbackText(key, 256) ||
		!validFeedbackText(tool.RunID, 256) || !validFeedbackText(tool.ToolCallID, 256) || !validFeedbackText(tool.ToolName, 256) {
		return errors.New("invalid WeChat progress")
	}
	item := map[string]any{"create_time_ms": time.Now().UnixMilli()}
	switch tool.Phase {
	case "start":
		item["type"], item["is_completed"] = 11, false
		item["tool_call_start_item"] = map[string]string{"tool_name": tool.ToolName, "tool_call_id": tool.ToolCallID}
	case "end":
		switch tool.Status {
		case "completed", "failed", "blocked", "unknown":
		default:
			return errors.New("invalid WeChat tool status")
		}
		item["type"], item["is_completed"] = 12, true
		item["tool_call_result_item"] = map[string]string{"tool_name": tool.ToolName, "tool_call_id": tool.ToolCallID, "status": tool.Status}
	default:
		return errors.New("invalid WeChat progress phase")
	}
	body := map[string]any{
		"base_info": map[string]string{"channel_version": "1.1.0"},
		"msg": map[string]any{
			"from_user_id": "", "to_user_id": to, "context_token": token,
			"client_id": key, "run_id": tool.RunID, "message_type": 2, "message_state": 2,
			"item_list": []any{item},
		},
	}
	var out map[string]any
	return c.request(ctx, cred, "/ilink/bot/sendmessage", nil, body, &out)
}

func (c *Client) TypingTicket(ctx context.Context, cred Credentials, to, token string) (string, error) {
	if !validFeedbackText(to, 1024) || !validFeedbackText(token, 8192) {
		return "", errors.New("invalid WeChat typing request")
	}
	var config struct {
		Ticket string `json:"typing_ticket"`
	}
	err := c.request(ctx, cred, "/ilink/bot/getconfig", nil, map[string]any{
		"ilink_user_id": to, "context_token": token,
		"base_info": map[string]string{"channel_version": "1.1.0"},
	}, &config)
	if err != nil {
		return "", err
	}
	if config.Ticket != "" && !validFeedbackText(config.Ticket, 8192) {
		return "", errors.New("invalid WeChat typing ticket")
	}
	return config.Ticket, nil
}

func (c *Client) Typing(ctx context.Context, cred Credentials, to, ticket string, active bool) error {
	if !validFeedbackText(to, 1024) || !validFeedbackText(ticket, 8192) {
		return errors.New("invalid WeChat typing request")
	}
	status := 2
	if active {
		status = 1
	}
	var out map[string]any
	return c.request(ctx, cred, "/ilink/bot/sendtyping", nil, map[string]any{
		"ilink_user_id": to, "typing_ticket": ticket, "status": status,
		"base_info": map[string]string{"channel_version": "1.1.0"},
	}, &out)
}
