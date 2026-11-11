package wechat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/channelproto"
)

func feedbackInput() (channelproto.Auth, channelproto.Message, channelproto.ToolProgress) {
	return channelproto.Auth{Account: "bot", Owner: "alice", Secret: json.RawMessage(`{"bot_token":"secret"}`)},
		channelproto.Message{Sender: "alice", Context: "context", Text: "PRIVATE-PROMPT"},
		channelproto.ToolProgress{RunID: "run", ToolCallID: "call", ToolName: "读取文件", Phase: "start"}
}

func TestPluginNativeToolProgressAndTypingLifecycle(t *testing.T) {
	p := NewPlugin()
	ctx := context.Background()
	d, err := p.Handle(ctx, "initialize", json.RawMessage(`{"protocol":1}`))
	if err != nil || !d.(channelproto.Descriptor).IndependentFeedback {
		t.Fatal("feedback capability missing", d, err)
	}
	var kinds, typingStates []int
	configs := 0
	p.client = fakeClient(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "PRIVATE-PROMPT") || r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatal("invalid feedback payload or authentication")
		}
		var body struct {
			User    string `json:"ilink_user_id"`
			Context string `json:"context_token"`
			Ticket  string `json:"typing_ticket"`
			Status  int    `json:"status"`
			Msg     struct {
				To, From, Context, Run, Key string
				Items                       []map[string]any `json:"item_list"`
			} `json:"msg"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		switch r.URL.Path {
		case "/ilink/bot/sendmessage":
			var envelope map[string]json.RawMessage
			_ = json.Unmarshal(raw, &envelope)
			var msg map[string]any
			_ = json.Unmarshal(envelope["msg"], &msg)
			if msg["to_user_id"] != "alice" || msg["from_user_id"] != "" || msg["run_id"] != "run" || msg["context_token"] != "context" || msg["message_state"] != float64(2) {
				t.Fatal("lost progress envelope", msg)
			}
			if len(body.Msg.Items) != 1 {
				t.Fatal("expected one structured item", body.Msg.Items)
			}
			item := body.Msg.Items[0]
			kind := int(item["type"].(float64))
			kinds = append(kinds, kind)
			field := "tool_call_start_item"
			if kind == 12 {
				field = "tool_call_result_item"
			}
			tool := item[field].(map[string]any)
			if tool["tool_name"] != "读取文件" || tool["tool_call_id"] != "call" || item["is_completed"] != (kind == 12) || item["create_time_ms"].(float64) <= 0 {
				t.Fatal("invalid structured progress", item)
			}
			if kind == 12 && tool["status"] != "blocked" {
				t.Fatal("waiting state lost", tool)
			}
		case "/ilink/bot/getconfig":
			configs++
			if body.User != "alice" || body.Context != "context" {
				t.Fatal("typing ticket scoped incorrectly")
			}
			return response(`{"ret":0,"typing_ticket":"ticket"}`), nil
		case "/ilink/bot/sendtyping":
			if body.User != "alice" || body.Ticket != "ticket" {
				t.Fatal("typing ticket lost")
			}
			typingStates = append(typingStates, body.Status)
		default:
			t.Fatal("unexpected endpoint", r.URL.Path)
		}
		return response(`{"ret":0}`), nil
	})
	auth, msg, tool := feedbackInput()
	call := func(method string, input any) any {
		t.Helper()
		raw, _ := json.Marshal(input)
		result, err := p.Handle(ctx, method, raw)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	prepared := call("typing.prepare", channelproto.Typing{Auth: auth, Message: msg}).(channelproto.TypingResult)
	for range 2 {
		call("typing", channelproto.Typing{Auth: auth, Message: msg, Ticket: prepared.Ticket, Active: true})
	}
	call("progress", channelproto.Progress{Auth: auth, Message: msg, Key: "start-key", Tool: tool})
	tool.Phase, tool.Status = "end", "blocked"
	call("progress", channelproto.Progress{Auth: auth, Message: msg, Key: "end-key", Tool: tool})
	call("typing", channelproto.Typing{Auth: auth, Message: msg, Ticket: prepared.Ticket})
	if configs != 1 || !reflect.DeepEqual(kinds, []int{11, 12}) || !reflect.DeepEqual(typingStates, []int{1, 1, 2}) {
		t.Fatal(configs, kinds, typingStates)
	}
}

func TestFeedbackChecksBusinessErrorsAndAuthExpiry(t *testing.T) {
	auth, msg, tool := feedbackInput()
	for _, method := range []string{"progress", "typing.prepare", "typing"} {
		for _, payload := range []string{`{"ret":-2,"errmsg":"PRIVATE-ERROR"}`, `{"errcode":-14}`, `[]`, `broken`} {
			t.Run(method+payload, func(t *testing.T) {
				p := NewPlugin()
				p.client = fakeClient(func(*http.Request) (*http.Response, error) { return response(payload), nil })
				var input any = channelproto.Typing{Auth: auth, Message: msg, Ticket: "ticket", Active: true}
				if method == "progress" {
					input = channelproto.Progress{Auth: auth, Message: msg, Key: "key", Tool: tool}
				}
				raw, _ := json.Marshal(input)
				_, err := p.Handle(context.Background(), method, raw)
				if err == nil || strings.Contains(err.Error(), "PRIVATE-ERROR") {
					t.Fatal("provider failure ignored or leaked", err)
				}
				if strings.Contains(payload, "-14") && !errors.Is(err, channelproto.ErrAuthExpired) {
					t.Fatal("lost auth expiry", err)
				}
			})
		}
	}
}

func TestFeedbackRejectsInvalidFieldsAndHonorsCancellation(t *testing.T) {
	_, _, tool := feedbackInput()
	c := fakeClient(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid feedback reached provider")
		return nil, nil
	})
	for _, change := range []func(*channelproto.ToolProgress){
		func(v *channelproto.ToolProgress) { v.RunID = "" },
		func(v *channelproto.ToolProgress) { v.ToolCallID = "" },
		func(v *channelproto.ToolProgress) { v.ToolName = strings.Repeat("x", 257) },
		func(v *channelproto.ToolProgress) { v.ToolName = "name\ncommand" },
		func(v *channelproto.ToolProgress) { v.Phase = "update" },
		func(v *channelproto.ToolProgress) { v.Phase, v.Status = "end", "private error" },
	} {
		v := tool
		change(&v)
		if err := c.SendProgress(context.Background(), Credentials{}, "alice", "context", "key", v); err == nil {
			t.Fatal("invalid progress accepted", v)
		}
	}
	if err := c.Typing(context.Background(), Credentials{}, "alice", "", true); err == nil {
		t.Fatal("missing typing ticket accepted")
	}
	c = fakeClient(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := c.SendProgress(ctx, Credentials{}, "alice", "context", "key", tool); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("feedback ignored deadline", err)
	}
}

func TestMissingTypingTicketDoesNotAttemptTyping(t *testing.T) {
	calls := 0
	c := fakeClient(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != "/ilink/bot/getconfig" {
			t.Fatal(r.URL.Path)
		}
		return response(`{}`), nil
	})
	ticket, err := c.TypingTicket(context.Background(), Credentials{}, "alice", "context")
	if err != nil || ticket != "" || calls != 1 {
		t.Fatal(ticket, err, calls)
	}
}
