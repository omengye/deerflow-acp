// Package wechat implements the iLink text channel, not WeCom or UI automation.
package wechat

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const DefaultOrigin = "https://ilinkai.weixin.qq.com"

var ErrExpired = errors.New("WeChat login expired; scan a new QR code")

type Credentials struct {
	Token  string `json:"bot_token"`
	BotID  string `json:"ilink_bot_id"`
	Origin string `json:"base_url"`
}
type Client struct{ HTTP *http.Client }

func NewClient() *Client {
	return &Client{HTTP: &http.Client{Timeout: 40 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func validateOrigin(origin string) error {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || !strings.HasSuffix(strings.ToLower(u.Hostname()), ".weixin.qq.com") {
		return errors.New("invalid WeChat HTTPS API origin")
	}
	return nil
}

func (c *Client) request(ctx context.Context, cred Credentials, endpoint string, query url.Values, body any, out any) error {
	origin := cred.Origin
	if origin == "" {
		origin = DefaultOrigin
	}
	if err := validateOrigin(origin); err != nil {
		return err
	}
	method := http.MethodGet
	var payload []byte
	var err error
	if body != nil {
		method = http.MethodPost
		payload, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	target := strings.TrimSuffix(origin, "/") + endpoint
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	r, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(payload))
	if err != nil {
		return errors.New("cannot construct WeChat request")
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("iLink-App-ClientVersion", "65536")
	var random [4]byte
	if _, err = rand.Read(random[:]); err != nil {
		return err
	}
	r.Header.Set("X-WECHAT-UIN", base64.StdEncoding.EncodeToString([]byte(fmt.Sprint(binary.BigEndian.Uint32(random[:])))))
	if cred.Token != "" {
		r.Header.Set("Authorization", "Bearer "+cred.Token)
		r.Header.Set("AuthorizationType", "ilink_bot_token")
	}
	resp, err := c.HTTP.Do(r)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("WeChat network request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("WeChat HTTP status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20+1))
	if err != nil {
		return errors.New("WeChat response read failed")
	}
	if len(data) > 4<<20 {
		return errors.New("WeChat response exceeds limit")
	}
	if object := bytes.TrimSpace(data); len(object) == 0 || object[0] != '{' {
		return errors.New("WeChat response must be a JSON object")
	}
	var status struct {
		Ret  int `json:"ret"`
		Code int `json:"errcode"`
	}
	if err = json.Unmarshal(data, &status); err != nil {
		return errors.New("invalid WeChat response")
	}
	if status.Code == -14 || status.Ret == -14 {
		return ErrExpired
	}
	if status.Code != 0 || status.Ret != 0 {
		return fmt.Errorf("WeChat API error (%d/%d)", status.Ret, status.Code)
	}
	if err = json.Unmarshal(data, out); err != nil {
		return errors.New("invalid WeChat response payload")
	}
	return nil
}

type QR struct {
	Code    string `json:"qrcode"`
	Content string `json:"qrcode_img_content"`
}
type LoginStatus struct {
	Status string `json:"status"`
	Credentials
	UserID       string `json:"ilink_user_id"`
	BaseURL      string `json:"baseurl"`
	RedirectHost string `json:"redirect_host"`
}

func (c *Client) QR(ctx context.Context) (QR, error) {
	var v QR
	err := c.request(ctx, Credentials{}, "/ilink/bot/get_bot_qrcode", url.Values{"bot_type": {"3"}}, nil, &v)
	if err == nil && (v.Code == "" || len(v.Code) > 2048 || v.Content == "" || len(v.Content) > 4096) {
		err = errors.New("invalid WeChat QR response")
	}
	return v, err
}
func (c *Client) Login(ctx context.Context, code, verify string) (LoginStatus, error) {
	return c.loginAt(ctx, DefaultOrigin, code, verify)
}

func validateVerificationCode(verify string) error {
	if len(verify) > 16 {
		return errors.New("verification code must be 1..16 digits")
	}
	for _, c := range verify {
		if c < '0' || c > '9' {
			return errors.New("verification code must contain digits only")
		}
	}
	return nil
}

func (c *Client) loginAt(ctx context.Context, origin, code, verify string) (LoginStatus, error) {
	if err := validateVerificationCode(verify); err != nil {
		return LoginStatus{}, err
	}
	query := url.Values{"qrcode": {code}}
	if verify != "" {
		query.Set("verify_code", verify)
	}
	var v LoginStatus
	err := c.request(ctx, Credentials{Origin: origin}, "/ilink/bot/get_qrcode_status", query, nil, &v)
	if err == nil && v.Status == "confirmed" {
		if v.BaseURL != "" {
			v.Origin = v.BaseURL
		}
		if v.Origin == "" {
			v.Origin = origin
		}
		if e := validateOrigin(v.Origin); e != nil {
			return LoginStatus{}, e
		}
		if v.Token == "" || v.BotID == "" || len(v.Token) > 8192 || len(v.BotID) > 512 {
			return LoginStatus{}, errors.New("invalid WeChat credentials")
		}
	}
	if err == nil && v.Status == "scaned_but_redirect" {
		if e := validateOrigin("https://" + v.RedirectHost); e != nil {
			return LoginStatus{}, e
		}
	}
	return v, err
}

type Message struct {
	ID       json.RawMessage `json:"message_id"`
	MsgID    json.RawMessage `json:"msg_id"`
	ClientID string          `json:"client_id"`
	Type     int             `json:"message_type"`
	From     string          `json:"from_user_id"`
	UserID   string          `json:"ilink_user_id"`
	Group    string          `json:"group_id"`
	Context  string          `json:"context_token"`
	Items    []struct {
		Type int `json:"type"`
		Text struct {
			Text string `json:"text"`
		} `json:"text_item"`
	} `json:"item_list"`
}

func (m Message) key() string {
	for _, raw := range []json.RawMessage{m.ID, m.MsgID} {
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var value string
		if json.Unmarshal(raw, &value) == nil && value != "" && len(value) <= 512 {
			return value
		}
		var n json.Number
		if len(raw) <= 128 && json.Unmarshal(raw, &n) == nil && n != "" {
			return n.String()
		}
	}
	if len(m.ClientID) <= 512 {
		return m.ClientID
	}
	return ""
}

type Updates struct {
	Messages []Message `json:"msgs"`
	Cursor   string    `json:"get_updates_buf"`
}

func (c *Client) Poll(ctx context.Context, cred Credentials, cursor string) (Updates, error) {
	var out Updates
	err := c.request(ctx, cred, "/ilink/bot/getupdates", nil, map[string]any{"get_updates_buf": cursor, "base_info": map[string]string{"channel_version": "1.0"}}, &out)
	if err == nil && (len(out.Messages) > 500 || len(out.Cursor) > 65536) {
		err = errors.New("WeChat update batch exceeds limit")
	}
	return out, err
}
func (c *Client) Send(ctx context.Context, cred Credentials, to, token, id, text string) error {
	if to == "" || token == "" || id == "" || len(text) > 12000 {
		return errors.New("invalid outbound WeChat message")
	}
	body := map[string]any{"base_info": map[string]string{"channel_version": "1.0"}, "msg": map[string]any{"from_user_id": "", "to_user_id": to, "context_token": token, "client_id": id, "message_type": 2, "message_state": 2, "item_list": []any{map[string]any{"type": 1, "text_item": map[string]string{"text": text}}}}}
	var out map[string]any
	return c.request(ctx, cred, "/ilink/bot/sendmessage", nil, body, &out)
}
