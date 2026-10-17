package deerflow

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/channelproto"
	"github.com/omengye/deerflow-acp/go-harness/internal/channels"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
	qrcode "github.com/skip2/go-qrcode"
)

func channelManagementError(err error) error {
	if errors.Is(err, channelproto.ErrAuthExpired) {
		return managementInvalid("Login expired; sign in again")
	}
	var public *channels.UserError
	if errors.As(err, &public) {
		return managementInvalid(public.Message)
	}
	return err
}

func (c *Client) managementChannel(ctx context.Context, r localhost.ManagementRequest) (any, error) {
	var in struct {
		ID        string            `json:"id"`
		Revision  int64             `json:"revision"`
		Workspace string            `json:"workspace"`
		Options   map[string]string `json:"options"`
		Fields    map[string]string `json:"fields"`
		Enabled   bool              `json:"enabled"`
		Login     string            `json:"login_id"`
		Verify    string            `json:"verify_code"`
		Message   string            `json:"message_id"`
		Manifest  channels.Manifest `json:"manifest"`
		Approval  string            `json:"approval"`
	}
	keys := []string{"id"}
	switch r.Operation {
	case "channel.list":
		keys = nil
	case "channel.install":
		keys = append(keys, "manifest")
	case "channel.update":
		keys = append(keys, "manifest", "revision")
	case "channel.rollback":
		keys = append(keys, "revision")
	case "channel.configure":
		keys = append(keys, "revision", "workspace", "options", "enabled")
	case "channel.permissions":
		keys = append(keys, "revision", "approval")
	case "channel.login.poll":
		keys = append(keys, "login_id", "verify_code")
	case "channel.login.start":
		keys = append(keys, "fields")
	case "channel.review":
		keys = append(keys, "message_id")
	}
	if err := decodeManagement(r, &in, keys...); err != nil {
		return nil, err
	}
	switch r.Operation {
	case "channel.list":
		return c.channels.List(ctx)
	case "channel.status":
		return c.channels.Status(ctx, in.ID)
	case "channel.install":
		return c.channels.Install(ctx, in.ID, in.Manifest)
	case "channel.update":
		return c.channels.Update(ctx, in.ID, in.Revision, in.Manifest, false)
	case "channel.rollback":
		return c.channels.Update(ctx, in.ID, in.Revision, channels.Manifest{}, true)
	case "channel.configure":
		return c.channels.Configure(ctx, in.ID, in.Revision, in.Workspace, in.Options, in.Enabled)
	case "channel.permissions":
		v, err := c.channels.SetPermissions(ctx, in.ID, in.Revision, in.Approval)
		return v, channelManagementError(err)
	case "channel.history":
		return c.channels.History(ctx, in.ID)
	case "channel.review":
		return map[string]bool{"reviewed_without_replay": true}, c.channels.Review(ctx, in.ID, in.Message)
	case "channel.login.start", "channel.login.poll", "channel.login.cancel", "channel.logout":
		value, err := c.channels.LoginWithFields(ctx, in.ID, strings.TrimPrefix(r.Operation, "channel."), in.Login, in.Verify, in.Fields)
		if err != nil {
			return nil, err
		}
		data, ok := value.(map[string]any)
		if !ok {
			return value, nil
		}
		if content, _ := data["qrcode_content"].(string); content != "" {
			png, e := qrcode.Encode(content, qrcode.Medium, 320)
			if e != nil {
				return nil, e
			}
			data["qr_png"] = base64.StdEncoding.EncodeToString(png)
		}
		return data, nil
	default:
		return nil, managementInvalid("unsupported channel operation")
	}
}
