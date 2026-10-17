package wechat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(body string) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}
func fakeClient(fn transportFunc) *Client { c := NewClient(); c.HTTP.Transport = fn; return c }

func TestWeChatProtocolAndCredentialsNeverRedirect(t *testing.T) {
	calls := 0
	c := fakeClient(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("AuthorizationType") != "ilink_bot_token" || r.Header.Get("X-WECHAT-UIN") == "" {
			t.Fatal("missing headers")
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		switch r.URL.Path {
		case "/ilink/bot/getupdates":
			if string(body["get_updates_buf"]) != `"cursor"` {
				t.Fatal("cursor lost")
			}
			return response(`{"ret":0,"msgs":[],"get_updates_buf":"next"}`), nil
		case "/ilink/bot/sendmessage":
			var msg map[string]any
			_ = json.Unmarshal(body["msg"], &msg)
			if msg["client_id"] != "stable-id" || msg["context_token"] != "context" || msg["to_user_id"] != "user" {
				t.Fatal(msg)
			}
			return response(`{"ret":0}`), nil
		}
		t.Fatal("unexpected endpoint")
		return nil, nil
	})
	cred := Credentials{Token: "secret", BotID: "bot", Origin: DefaultOrigin}
	u, err := c.Poll(context.Background(), cred, "cursor")
	if err != nil || u.Cursor != "next" {
		t.Fatal(u, err)
	}
	if err = c.Send(context.Background(), cred, "user", "context", "stable-id", "reply"); err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{"https://evil.test", "https://weixin.qq.com.evil.test", "http://ilinkai.weixin.qq.com", "https://user:pw@ilinkai.weixin.qq.com", "https://ilinkai.weixin.qq.com:8443", "https://ilinkai.weixin.qq.com/redirect"} {
		cred.Origin = origin
		if _, err = c.Poll(context.Background(), cred, ""); err == nil {
			t.Fatal("unsafe origin", origin)
		}
	}
	if calls != 2 {
		t.Fatal("unsafe request sent")
	}
	c = fakeClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://evil.test"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	cred.Origin = DefaultOrigin
	if _, err = c.Poll(context.Background(), cred, ""); err == nil {
		t.Fatal("accepted redirect")
	}
}
func TestWeChatLoginRejectsMaliciousOriginAndRedactsErrors(t *testing.T) {
	c := fakeClient(func(*http.Request) (*http.Response, error) {
		return response(`{"status":"confirmed","bot_token":"secret","ilink_bot_id":"bot","base_url":"https://evil.test"}`), nil
	})
	if _, err := c.Login(context.Background(), "qr", "123"); err == nil {
		t.Fatal("accepted credential redirection")
	}
	c = fakeClient(func(*http.Request) (*http.Response, error) {
		return response(`{"errcode":-14,"errmsg":"secret token"}`), nil
	})
	if _, err := c.Poll(context.Background(), Credentials{}, ""); !errors.Is(err, ErrExpired) || strings.Contains(err.Error(), "secret") {
		t.Fatal(err)
	}
	if _, err := c.Login(context.Background(), "qr", "x"); err == nil {
		t.Fatal("accepted non-digit verification")
	}
}
func TestWeChatCredentialEnvelopeRoundTrip(t *testing.T) {
	raw := []byte(`{"bot_token":"private"}`)
	sealed, err := sealCredentials(raw)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := openCredentials(sealed)
	if err != nil || string(decoded) != string(raw) {
		t.Fatal("credential round trip", err)
	}
}

func TestWeChatProviderBaseurlAndLoginRedirect(t *testing.T) {
	c := fakeClient(func(r *http.Request) (*http.Response, error) {
		return response(`{"status":"confirmed","bot_token":"private","ilink_bot_id":"bot","baseurl":"https://other.weixin.qq.com"}`), nil
	})
	v, err := c.Login(context.Background(), "qr", "")
	if err != nil || v.Origin != "https://other.weixin.qq.com" {
		t.Fatal(v.Status, err)
	}
	c = fakeClient(func(*http.Request) (*http.Response, error) {
		return response(`{"status":"scaned_but_redirect","redirect_host":"evil.test"}`), nil
	})
	if _, err = c.Login(context.Background(), "qr", ""); err == nil {
		t.Fatal("unsafe provider redirect accepted")
	}
}

func TestWeChatSendRequiresAnObjectAcknowledgement(t *testing.T) {
	for _, body := range []string{`null`, `[]`, `{"ret":"0"}`, `{"errcode":1,"errmsg":"private token"}`} {
		c := fakeClient(func(*http.Request) (*http.Response, error) { return response(body), nil })
		err := c.Send(context.Background(), Credentials{}, "alice", "context", "stable-id", "reply")
		if err == nil || strings.Contains(err.Error(), "private token") {
			t.Fatalf("invalid acknowledgement accepted or leaked: %s %v", body, err)
		}
	}
}
