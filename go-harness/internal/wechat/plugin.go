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
	loginState         string
	qr                 QR
	expires            time.Time
}

func NewPlugin() *Plugin { return &Plugin{client: NewClient()} }

func Descriptor() channelproto.Descriptor {
	return channelproto.Descriptor{
		ID: "wechat", Name: "WeChat", Version: "1.1.0", Protocol: channelproto.Version,
		Login: []string{"qr"}, Messages: []string{"text"},
		RequiresReplyContext: true, IndependentPoll: true, IndependentFeedback: true,
		Fields: []channelproto.Field{
			{Key: "progress_mode", Label: "工具进度：native（默认）或 summary（文字摘要）"},
			{Key: "typing", Label: "正在输入：true（默认）或 false"},
		},
	}
}

func (p *Plugin) clearLogin() {
	p.id, p.origin, p.verify, p.loginState = "", "", "", ""
	p.qr = QR{}
	p.expires = time.Time{}
}

func (p *Plugin) expiredLogin() channelproto.Login {
	result := channelproto.Login{ID: p.id, State: "expired"}
	p.clearLogin()
	return result
}

func (p *Plugin) Handle(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		var in struct {
			Protocol int `json:"protocol"`
		}
		if channelproto.Decode(raw, &in) != nil || in.Protocol != channelproto.Version {
			return nil, errors.New("unsupported protocol")
		}
		return Descriptor(), nil
	case "login.start":
		p.clearLogin()
		ctx, cancel := context.WithTimeout(ctx, 7*time.Second)
		defer cancel()
		qr, err := p.client.QR(ctx)
		if err != nil {
			return nil, err
		}
		p.id, p.origin, p.qr, p.expires = rand.Text(), DefaultOrigin, qr, time.Now().Add(180*time.Second)
		p.loginState = "pending"
		return channelproto.Login{ID: p.id, State: "pending", QR: qr.Content, ExpiresIn: 180}, nil
	case "login.poll":
		var in struct {
			ID     string `json:"id"`
			Verify string `json:"verify_code"`
		}
		if channelproto.Decode(raw, &in) != nil || in.ID == "" || in.ID != p.id {
			return nil, channelproto.ErrAuthExpired
		}
		if !time.Now().Before(p.expires) {
			return p.expiredLogin(), nil
		}
		if err := validateVerificationCode(in.Verify); err != nil {
			return nil, err
		}
		if in.Verify != "" {
			p.verify = in.Verify
			if p.loginState == "verification_required" {
				p.loginState = "pending"
			}
		}
		deadline := time.Now().Add(7 * time.Second)
		if p.expires.Before(deadline) {
			deadline = p.expires
		}
		pollCtx, cancel := context.WithDeadline(ctx, deadline)
		defer cancel()
		v, err := p.client.loginAt(pollCtx, p.origin, p.qr.Code, p.verify)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !time.Now().Before(p.expires) || errors.Is(err, ErrExpired) {
			return p.expiredLogin(), nil
		}
		if err != nil {
			// iLink holds an unchanged QR status for about 30 seconds. Keep each
			// management call bounded without invalidating the login on that wait.
			// Only our poll deadline is recoverable; caller cancellation and
			// provider/network failures must still reach the host.
			if errors.Is(pollCtx.Err(), context.DeadlineExceeded) {
				return channelproto.Login{ID: p.id, State: p.loginState}, nil
			}
			return nil, err
		}
		result := channelproto.Login{ID: p.id, State: p.loginState}
		switch v.Status {
		case "wait", "pending":
			// A wait does not undo a scan or acknowledge a verification code.
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
		case "expired":
			result.State = "expired"
		default:
			result.State = "failed"
		}
		if result.State == "confirmed" || result.State == "expired" || result.State == "failed" {
			p.clearLogin()
		} else {
			p.loginState = result.State
		}
		return result, nil
	case "login.cancel", "logout":
		p.clearLogin()
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
			var quotes strings.Builder
			for _, item := range m.Items {
				if item.Type != 1 {
					msg.Unsupported = true
				} else {
					text.WriteString(item.Text.Text)
					text.WriteByte('\n')
				}
				if item.Ref != nil {
					quoted, complete := quotedText(item.Ref, 0)
					if !complete {
						msg.Unsupported = true
					}
					quotes.WriteString(quoted)
					quotes.WriteByte('\n')
				}
			}
			msg.Text = strings.TrimSpace(text.String())
			msg.QuotedText = strings.TrimSpace(quotes.String())
			result.Messages = append(result.Messages, msg)
		}
		return result, nil
	case "progress":
		var in channelproto.Progress
		if err := channelproto.Decode(raw, &in); err != nil {
			return nil, err
		}
		var cred Credentials
		if err := json.Unmarshal(in.Auth.Secret, &cred); err != nil {
			return nil, err
		}
		err := p.client.SendProgress(ctx, cred, in.Message.Sender, in.Message.Context, in.Key, in.Tool)
		if errors.Is(err, ErrExpired) {
			err = channelproto.ErrAuthExpired
		}
		return map[string]bool{"sent": err == nil}, err
	case "typing.prepare", "typing":
		var in channelproto.Typing
		if err := channelproto.Decode(raw, &in); err != nil {
			return nil, err
		}
		var cred Credentials
		if err := json.Unmarshal(in.Auth.Secret, &cred); err != nil {
			return nil, err
		}
		var result channelproto.TypingResult
		var err error
		if method == "typing.prepare" {
			result.Ticket, err = p.client.TypingTicket(ctx, cred, in.Message.Sender, in.Message.Context)
		} else {
			err = p.client.Typing(ctx, cred, in.Message.Sender, in.Ticket, in.Active)
			result.Sent = err == nil
		}
		if errors.Is(err, ErrExpired) {
			err = channelproto.ErrAuthExpired
		}
		return result, err
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

func quotedText(ref *RefMessage, depth int) (string, bool) {
	// Bound nested quotes and reject incomplete/media/partial quotes instead of
	// silently dropping constraints. Partial selections need provider-specific
	// offset/hash validation before they can be represented faithfully.
	if depth >= 8 || ref.Item == nil || ref.Item.Type != 1 || strings.TrimSpace(ref.Item.Text.Text) == "" || (len(ref.PartialText) > 0 && string(ref.PartialText) != "null") {
		return "【引用内容不可用，请粘贴原文】", false
	}
	var text strings.Builder
	text.WriteString("【引用消息】\n")
	line := func(value string) {
		text.WriteString("> ")
		text.WriteString(strings.ReplaceAll(value, "\n", "\n> "))
		text.WriteByte('\n')
	}
	if ref.Title != "" {
		line("标题：" + ref.Title)
	}
	if ref.ID != "" {
		line("消息 ID：" + ref.ID)
	}
	line(ref.Item.Text.Text)
	if ref.Item.Ref != nil {
		nested, complete := quotedText(ref.Item.Ref, depth+1)
		if !complete {
			return "【引用内容不可用，请粘贴原文】", false
		}
		line(nested)
	}
	return strings.TrimSpace(text.String()), true
}
