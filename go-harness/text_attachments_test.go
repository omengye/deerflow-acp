package deerflow

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestTextAttachmentNativeToolRoundTripAndReopen(t *testing.T) {
	var reads atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role, Content string
				ToolCallID    string `json:"tool_call_id"`
			}
			Tools []struct{ Function struct{ Name string } }
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		available := false
		for _, item := range body.Tools {
			available = available || item.Function.Name == "read_attachment"
		}
		if !available {
			t.Error("read-only attachment tool not offered")
		}
		var id string
		for _, m := range body.Messages {
			if m.Role == "user" {
				if strings.Contains(m.Content, "attachment-body-42") {
					t.Error("attachment body entered prompt before tool read")
				}
				if _, tail, ok := strings.Cut(m.Content, "read_attachment with id="); ok {
					id, _, _ = strings.Cut(tail, " ")
				}
			}
		}
		if id == "" {
			t.Error("no opaque reference")
		}
		delta := map[string]any{"role": "assistant", "content": "verified"}
		finish := "stop"
		last := body.Messages[len(body.Messages)-1]
		if last.Role != "tool" {
			delta = map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "attachment-read", "type": "function", "function": map[string]any{"name": "read_attachment", "arguments": fmt.Sprintf(`{"id":%q}`, id)}}}}
			finish = "tool_calls"
		} else {
			if !strings.Contains(last.Content, "attachment-body-42") {
				t.Errorf("tool did not return attachment: %s", last.Content)
			}
			reads.Add(1)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunk, _ := json.Marshal(map[string]any{"id": "fixture", "object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
	}))
	defer provider.Close()
	cfg := Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", APIKey: "fixture-only", BaseURL: provider.URL + "/v1", DisableSubagents: true, PermissionMode: harness.PermissionModeDangerous}
	ctx := context.Background()
	client, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	x, err := client.NewSession(ctx, t.TempDir())
	if err != nil {
		_ = client.Close()
		t.Fatal(err)
	}
	if _, err = client.SetConfigOption(ctx, x.ID, "approval", harness.ApprovalReadOnly); err != nil {
		_ = client.Close()
		t.Fatal(err)
	}
	_, err = client.Run(ctx, x.ID, []harness.Content{{Type: "attachment", Name: "notes.txt", Text: "attachment-body-42"}}, nil, nil)
	closeErr := client.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	client, err = Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err = client.LoadSession(ctx, x.ID, x.CWD, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = client.Run(ctx, x.ID, []harness.Content{{Type: "text", Text: "Read the earlier attachment again"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if reads.Load() != 2 {
		t.Fatalf("read rounds=%d", reads.Load())
	}
}
