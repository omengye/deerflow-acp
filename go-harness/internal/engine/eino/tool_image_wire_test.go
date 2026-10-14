package eino

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstreamtest"
)

func TestOpenAIToolImagesUseUserContentAfterAllToolResults(t *testing.T) {
	for _, provider := range []string{"", "openai", "openai-compatible"} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", provider, streaming), func(t *testing.T) {
				data := testImageBase64
				image := schema.MessageInputPart{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{MessagePartCommon: schema.MessagePartCommon{Base64Data: &data, MIMEType: "image/png"}}}
				input := []*schema.Message{
					schema.UserMessage("Inspect the images"),
					{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
						{ID: "image-a", Type: "function", Function: schema.FunctionCall{Name: "view_image", Arguments: `{}`}},
						{ID: "text-b", Type: "function", Function: schema.FunctionCall{Name: "read_file", Arguments: `{}`}},
						{ID: "image-c", Type: "function", Function: schema.FunctionCall{Name: "view_image", Arguments: `{}`}},
					}},
					{Role: schema.Tool, ToolCallID: "image-a", ToolName: "view_image", UserInputMultiContent: []schema.MessageInputPart{{Type: schema.ChatMessagePartTypeText, Text: "first caption"}, image}},
					schema.ToolMessage("file text", "text-b"),
					{Role: schema.Tool, ToolCallID: "image-c", ToolName: "view_image", UserInputMultiContent: []schema.MessageInputPart{image, {Type: schema.ChatMessagePartTypeText, Text: "last caption"}}},
					schema.AssistantMessage("Earlier inspection", nil),
					schema.UserMessage("Continue"),
				}
				before, _ := json.Marshal(input)
				var calls atomic.Int32
				server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					var body struct {
						Messages []struct {
							Role       string          `json:"role"`
							ToolCallID string          `json:"tool_call_id"`
							Content    json.RawMessage `json:"content"`
						}
						Tools []json.RawMessage `json:"tools"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						return
					}
					var roles, ids []string
					images := 0
					for _, message := range body.Messages {
						roles = append(roles, message.Role)
						if message.Role == "tool" {
							ids = append(ids, message.ToolCallID)
							var text string
							if json.Unmarshal(message.Content, &text) != nil || strings.Contains(text, "base64") {
								t.Error("tool result must contain text, never image bytes or image parts")
							}
							if message.ToolCallID == "image-a" && !strings.Contains(text, "first caption") || message.ToolCallID == "image-c" && !strings.Contains(text, "last caption") || message.ToolCallID == "text-b" && text != "file text" {
								t.Error("tool text was lost")
							}
						}
						if strings.Contains(string(message.Content), "data:image/png;base64,"+data) {
							images++
							if message.Role != "user" || !strings.Contains(string(message.Content), "image-") {
								t.Error("image must be a user content part linked to its tool result")
							}
						}
					}
					if !reflect.DeepEqual(roles, []string{"user", "assistant", "tool", "tool", "tool", "user", "user", "assistant", "user"}) || !reflect.DeepEqual(ids, []string{"image-a", "text-b", "image-c"}) || images != 2 || len(body.Tools) != 1 {
						t.Errorf("roles=%v ids=%v images=%d tools=%d", roles, ids, images, len(body.Tools))
					}
					_ = modelstreamtest.WriteJSON(w, `{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
				}))
				defer server.Close()
				chat, err := newModel(context.Background(), Config{Provider: provider, Model: "fixture", BaseURL: server.URL, APIKey: "fixture"}, "fixture")
				if err != nil {
					t.Fatal(err)
				}
				chat, err = chat.WithTools([]*schema.ToolInfo{{Name: "view_image", Desc: "Read an image"}})
				if err != nil {
					t.Fatal(err)
				}
				if streaming {
					stream, err := chat.Stream(context.Background(), input, model.WithMaxTokens(64))
					if err != nil {
						t.Fatal(err)
					}
					defer stream.Close()
					for {
						_, err := stream.Recv()
						if err == io.EOF {
							break
						}
						if err != nil {
							t.Fatal(err)
						}
					}
				} else if _, err = chat.Generate(context.Background(), input, model.WithMaxTokens(64)); err != nil {
					t.Fatal(err)
				}
				after, _ := json.Marshal(input)
				if string(before) != string(after) || calls.Load() != 1 {
					t.Fatalf("native messages changed or request retried: calls=%d", calls.Load())
				}
			})
		}
	}
}

func TestToolImageHTTPRoundTripKeepsDurableHistoryUnchanged(t *testing.T) {
	ref, data := testImageAsset(t)
	result, err := ProjectToolContent([]harness.Content{imageContent(ref)})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		var body struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if call == 1 {
			_ = modelstreamtest.WriteJSON(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"inspect-image","type":"function","function":{"name":"read_workspace","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		images := 0
		for _, message := range body.Messages {
			if message.Role == "tool" && strings.Contains(string(message.Content), "base64") {
				t.Error("hydrated image became tool text")
			}
			if strings.Contains(string(message.Content), "data:image/png;base64,"+testImageBase64) {
				images++
				if message.Role != "user" {
					t.Error("hydrated tool image was not projected at the provider boundary")
				}
			}
		}
		if images != 1 {
			t.Errorf("request %d contains %d images", call, images)
		}
		_ = modelstreamtest.WriteJSON(w, `{"choices":[{"message":{"role":"assistant","content":"inspected"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	e := newTestEngine(t, Config{Provider: "openai", Model: "vision", BaseURL: server.URL, APIKey: "fixture", Media: harness.MediaConfig{VisionModels: []string{"vision"}}, AssetResolver: &testAssetResolver{ref: ref, data: data}, Tools: []tool.BaseTool{&mediaResultTool{result: result}}})
	for _, id := range []string{"image-turn", "image-followup"} {
		if _, err := e.Run(context.Background(), request(id), nil, allowTool); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("unexpected calls=%d", calls.Load())
	}
	stored, err := e.config.SessionStore.LoadEvents(context.Background(), ref.SessionID, &adk.LoadSessionEventsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	assertNoImageBytes(t, stored)
	images := 0
	for _, event := range stored.Events {
		if event.Message == nil {
			continue
		}
		for _, part := range event.Message.UserInputMultiContent {
			if part.Image != nil {
				images++
				if event.Message.Role != schema.Tool || event.Message.ToolCallID != "inspect-image" || part.Image.URL == nil || *part.Image.URL != harness.AssetURI(ref) {
					t.Fatal("provider projection leaked into native session history")
				}
			}
		}
	}
	if images != 1 {
		t.Fatalf("native image references=%d", images)
	}
}

func TestToolImageProjectionLeavesOtherProvidersAndTextUntouched(t *testing.T) {
	data := testImageBase64
	image := &schema.Message{Role: schema.Tool, ToolCallID: "image", UserInputMultiContent: []schema.MessageInputPart{{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{MessagePartCommon: schema.MessagePartCommon{Base64Data: &data, MIMEType: "image/png"}}}}}
	for _, provider := range []string{"claude", "anthropic", "injected"} {
		t.Run(provider, func(t *testing.T) {
			fake := &scriptedModel{stream: func(_ context.Context, _ int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				if len(input) != 1 || input[0] != image {
					t.Fatal("non-OpenAI image format changed")
				}
				return textStream("OK"), nil
			}}
			chat := &outputTokenModel{inner: fake, provider: provider}
			if _, err := chat.Generate(context.Background(), []*schema.Message{image}); err != nil {
				t.Fatal(err)
			}
		})
	}
	text := []*schema.Message{schema.UserMessage("request"), schema.ToolMessage("result", "text")}
	if got := openAIToolImageMessages(text); !reflect.DeepEqual(got, text) || &got[0] != &text[0] {
		t.Fatal("text-only requests should be a no-op")
	}
	input := []*schema.Message{image, schema.AssistantMessage("next", nil), image}
	got := openAIToolImageMessages(input)
	if len(got) != 5 || got[0].Role != schema.Tool || got[1].Role != schema.User || got[2] != input[1] || got[3].Role != schema.Tool || got[4].Role != schema.User {
		t.Fatal("image projection crossed tool exchange boundaries")
	}
}
