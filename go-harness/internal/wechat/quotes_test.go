package wechat

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/channelproto"
)

func pollQuotedFixture(t *testing.T, ref string) channelproto.Message {
	t.Helper()
	p := NewPlugin()
	p.client = fakeClient(func(*http.Request) (*http.Response, error) {
		return response(`{"ret":0,"get_updates_buf":"next","msgs":[{"message_type":1,"message_id":"a","from_user_id":"alice","context_token":"token","item_list":[{"type":1,"text_item":{"text":"照这个处理"},"ref_msg":` + ref + `}]}]}`), nil
	})
	raw, err := json.Marshal(channelproto.Poll{Auth: channelproto.Auth{Secret: json.RawMessage(`{}`)}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.Handle(context.Background(), "poll", raw)
	if err != nil {
		t.Fatal(err)
	}
	batch := result.(channelproto.Batch)
	if len(batch.Messages) != 1 || batch.Cursor != "next" {
		t.Fatal("unexpected poll result", batch)
	}
	return batch.Messages[0]
}

func TestInboundQuotedTextPreservesOriginalAndMessageID(t *testing.T) {
	msg := pollQuotedFixture(t, `{"title":"用户","message_item":{"type":1,"text_item":{"text":"只检查测试环境，不修改生产环境\n先报告结果"}},"svr_id":"previous"}`)
	if msg.Unsupported || msg.Text != "照这个处理" || msg.ID != "a" || msg.Context != "token" {
		t.Fatal("quote changed message identity or current text", msg)
	}
	for _, want := range []string{"【引用消息】", "> 标题：用户", "> 消息 ID：previous", "> 只检查测试环境，不修改生产环境\n> 先报告结果"} {
		if !strings.Contains(msg.QuotedText, want) {
			t.Fatalf("missing quoted context %q: %s", want, msg.QuotedText)
		}
	}
}

func TestNestedQuotedTextPreservesConstraints(t *testing.T) {
	msg := pollQuotedFixture(t, `{"message_item":{"type":1,"text_item":{"text":"执行这份计划"},"ref_msg":{"message_item":{"type":1,"text_item":{"text":"不要修改生产环境"}},"svr_id":"original"}},"svr_id":"parent"}`)
	if msg.Unsupported || !strings.Contains(msg.QuotedText, "执行这份计划") || !strings.Contains(msg.QuotedText, "不要修改生产环境") || !strings.Contains(msg.QuotedText, "original") {
		t.Fatal("nested quote silently lost", msg)
	}
}

func TestUnavailableQuotedContentIsExplicitlyUnsupported(t *testing.T) {
	for name, ref := range map[string]string{
		"id only":      `{"svr_id":"missing"}`,
		"title only":   `{"title":"unresolved summary"}`,
		"empty":        `{}`,
		"empty text":   `{"message_item":{"type":1,"text_item":{"text":" "}}}`,
		"media":        `{"message_item":{"type":2,"image_item":{}}}`,
		"nested media": `{"message_item":{"type":1,"text_item":{"text":"follow this"},"ref_msg":{"message_item":{"type":2}}}}`,
		"partial":      `{"message_item":{"type":1,"text_item":{"text":"first; second"}},"partial_text":{"startindex":0,"endindex":5}}`,
	} {
		t.Run(name, func(t *testing.T) {
			msg := pollQuotedFixture(t, ref)
			if !msg.Unsupported || !strings.Contains(msg.QuotedText, "粘贴原文") {
				t.Fatal("incomplete quoted instruction would execute", msg)
			}
		})
	}
}

func TestQuoteDepthIsBoundedAndUnquotedTextIsUnchanged(t *testing.T) {
	ref := `{"message_item":{"type":1,"text_item":{"text":"original constraint"}}}`
	for range 10 {
		ref = `{"message_item":{"type":1,"text_item":{"text":"reply"},"ref_msg":` + ref + `}}`
	}
	if msg := pollQuotedFixture(t, ref); !msg.Unsupported {
		t.Fatal("overly nested quote accepted", msg)
	}
	if msg := pollQuotedFixture(t, `null`); msg.Unsupported || msg.QuotedText != "" || msg.Text != "照这个处理" {
		t.Fatal("unquoted message changed", msg)
	}
}

func TestWeChatAdvertisesIndependentPolling(t *testing.T) {
	result, err := NewPlugin().Handle(context.Background(), "initialize", json.RawMessage(`{"protocol":1}`))
	if err != nil {
		t.Fatal(err)
	}
	descriptor := result.(channelproto.Descriptor)
	if !descriptor.IndependentPoll || !descriptor.RequiresReplyContext {
		t.Fatal("host cannot select safe WeChat delivery mode", descriptor)
	}
}
