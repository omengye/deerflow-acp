package wechat

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/channelproto"
)

// Plugin is deliberately stateless except for a short-lived login attempt.
// Credentials, cursors, dedupe, execution and delivery receipts belong to the host.
type Plugin struct {
	client             *Client
	id, origin, verify string
	qr                 QR
	expires            time.Time
}

func NewPlugin() *Plugin { return &Plugin{client: NewClient()} }
func (p *Plugin) Handle(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		var in struct {
			Protocol int `json:"protocol"`
		}
		if channelproto.Decode(raw, &in) != nil || in.Protocol != channelproto.Version {
			return nil, errors.New("unsupported protocol")
		}
		return channelproto.Descriptor{ID: "wechat", Name: "WeChat", Version: "1.0.0", Protocol: 1, Login: []string{"qr"}, Messages: []string{"text"}, Fields: []channelproto.Field{}, RequiresReplyContext: true}, nil
	case "login.start":
		p.id, p.verify = "", ""
		ctx, cancel := context.WithTimeout(ctx, 7*time.Second)
		defer cancel()
		qr, err := p.client.QR(ctx)
		if err != nil {
			return nil, err
		}
		p.id, p.origin, p.qr, p.expires = rand.Text(), DefaultOrigin, qr, time.Now().Add(180*time.Second)
		return channelproto.Login{ID: p.id, State: "pending", QR: qr.Content, ExpiresIn: 180}, nil
	case "login.poll":
		var in struct {
			ID     string `json:"id"`
			Verify string `json:"verify_code"`
		}
		if channelproto.Decode(raw, &in) != nil || in.ID == "" || in.ID != p.id || time.Now().After(p.expires) {
			return nil, channelproto.ErrAuthExpired
		}
		if err := validateVerificationCode(in.Verify); err != nil {
			return nil, err
		}
		if in.Verify != "" {
			p.verify = in.Verify
		}
		ctx, cancel := context.WithTimeout(ctx, 7*time.Second)
		defer cancel()
		v, err := p.client.loginAt(ctx, p.origin, p.qr.Code, p.verify)
		if err != nil {
			return nil, err
		}
		result := channelproto.Login{ID: p.id}
		switch v.Status {
		case "wait", "pending":
			result.State = "pending"
		case "scaned_but_redirect":
			p.origin = "https://" + v.RedirectHost
			result.State = "scanned"
		case "scaned", "scanned":
			result.State = "scanned"
			p.verify = ""
		case "need_verifycode":
			result.State = "verification_required"
			p.verify = ""
		case "confirmed":
			secret, _ := json.Marshal(v.Credentials)
			result.State = "confirmed"
			result.Auth = &channelproto.Auth{Account: v.BotID, Owner: v.UserID, Secret: secret}
			p.id, p.verify = "", ""
		case "expired":
			result.State = "expired"
			p.id, p.verify = "", ""
		default:
			result.State = "failed"
			p.id, p.verify = "", ""
		}
		return result, nil
	case "login.cancel", "logout":
		p.id, p.verify = "", ""
		return map[string]bool{"ok": true}, nil
	case "poll":
		var in channelproto.Poll
		if err := channelproto.Decode(raw, &in); err != nil {
			return nil, err
		}
		var cred Credentials
		if err := json.Unmarshal(in.Auth.Secret, &cred); err != nil {
			return nil, err
		}
		v, err := p.client.Poll(ctx, cred, in.Cursor)
		if errors.Is(err, ErrExpired) {
			return nil, channelproto.ErrAuthExpired
		}
		if err != nil {
			return nil, err
		}
		result := channelproto.Batch{Cursor: v.Cursor}
		for _, m := range v.Messages {
			if m.Type != 1 {
				continue
			}
			if m.From == "" {
				m.From = m.UserID
			}
			msg := channelproto.Message{ID: m.key(), Conversation: m.From, Sender: m.From, Context: m.Context, Group: m.Group != ""}
			var text strings.Builder
			for _, item := range m.Items {
				if item.Type != 1 {
					msg.Unsupported = true
				} else {
					text.WriteString(item.Text.Text)
					text.WriteByte('\n')
				}
			}
			msg.Text = strings.TrimSpace(text.String())
			result.Messages = append(result.Messages, msg)
		}
		return result, nil
	case "send":
		var in channelproto.Send
		if err := channelproto.Decode(raw, &in); err != nil {
			return nil, err
		}
		var cred Credentials
		if err := json.Unmarshal(in.Auth.Secret, &cred); err != nil {
			return nil, err
		}
		err := p.client.Send(ctx, cred, in.Message.Sender, in.Message.Context, in.Key, in.Text)
		if errors.Is(err, ErrExpired) {
			err = channelproto.ErrAuthExpired
		}
		return map[string]bool{"sent": err == nil}, err
	default:
		return nil, errors.New("unsupported channel method")
	}
}
