package wechat

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/channelproto"
)

func TestPluginOwnsOnlyProviderProtocol(t *testing.T) {
	p := NewPlugin()
	ctx := context.Background()
	step := 0
	p.client = fakeClient(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/ilink/bot/get_bot_qrcode":
			return response(`{"qrcode":"opaque","qrcode_img_content":"https://example.test/qr"}`), nil
		case "/ilink/bot/get_qrcode_status":
			step++
			if step == 1 {
				return response(`{"status":"need_verifycode"}`), nil
			}
			if r.URL.Query().Get("verify_code") != "123456" {
				t.Fatal("missing verification")
			}
			return response(`{"status":"confirmed","bot_token":"private","ilink_bot_id":"bot","ilink_user_id":"owner"}`), nil
		case "/ilink/bot/getupdates":
			return response(`{"ret":0,"get_updates_buf":"next","msgs":[{"message_type":1,"message_id":"m","from_user_id":"owner","context_token":"context","item_list":[{"type":1,"text_item":{"text":"hello"}}]}]}`), nil
		default:
			t.Fatal(r.URL.Path)
			return nil, nil
		}
	})
	v, err := p.Handle(ctx, "login.start", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	login := v.(channelproto.Login)
	in, _ := json.Marshal(map[string]string{"id": login.ID})
	v, err = p.Handle(ctx, "login.poll", in)
	if err != nil || v.(channelproto.Login).State != "verification_required" {
		t.Fatal(v, err)
	}
	in, _ = json.Marshal(map[string]string{"id": login.ID, "verify_code": "123456"})
	v, err = p.Handle(ctx, "login.poll", in)
	if err != nil {
		t.Fatal(err)
	}
	auth := v.(channelproto.Login).Auth
	if auth.Owner != "owner" {
		t.Fatal("identity not forwarded")
	}
	in, _ = json.Marshal(channelproto.Poll{Auth: *auth})
	v, err = p.Handle(ctx, "poll", in)
	if err != nil {
		t.Fatal(err)
	}
	batch := v.(channelproto.Batch)
	if batch.Cursor != "next" || len(batch.Messages) != 1 || batch.Messages[0].Text != "hello" {
		t.Fatal(batch)
	}
	if _, err = p.Handle(ctx, "login.poll", json.RawMessage(`{"id":"old"}`)); err == nil {
		t.Fatal("stale login accepted")
	}
}
