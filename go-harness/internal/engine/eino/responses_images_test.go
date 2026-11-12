package eino

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	chatapi "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
	"github.com/openai/openai-go/v3/responses"
)

func TestResponsesImageGenerationToolSelection(t *testing.T) {
	m := &responsesModel{name: "fixture", tools: []map[string]any{{"type": "function", "name": "lookup"}}}
	for _, test := range []struct {
		name string
		opts []model.Option
		want bool
	}{
		{"default_off", nil, false},
		{"enabled", []model.Option{withResponsesImageGeneration(true)}, true},
		{"forbidden", []model.Option{withResponsesImageGeneration(true), model.WithToolChoice(schema.ToolChoiceForbidden)}, false},
		{"function_only", []model.Option{withResponsesImageGeneration(true), model.WithToolChoice(schema.ToolChoiceAllowed, "lookup")}, false},
		{"image_only", []model.Option{withResponsesImageGeneration(true), model.WithToolChoice(schema.ToolChoiceAllowed, "image_generation")}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			params, err := m.request([]*schema.Message{schema.UserMessage("draw a cat")}, test.opts...)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(params)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), `"type":"image_generation"`) != test.want {
				t.Fatalf("image generation tool selection: %s", encoded)
			}
		})
	}
}

func TestResponsesImageGenerationSeparatesOfficialImageComplexityFromReasoning(t *testing.T) {
	for _, effort := range []string{"", "low", "medium", "high", "xhigh", "max"} {
		for _, endpoint := range []string{"", "https://api.openai.com/v1", "https://API.OPENAI.COM:443/v1/"} {
			chat, err := newResponsesModel(context.Background(), Config{BaseURL: endpoint}, "gpt-6.1-sol", nil)
			if err != nil {
				t.Fatal(err)
			}
			// WithTools must preserve image configuration used by real agents.
			chat, err = chat.WithTools([]*schema.ToolInfo{{Name: "lookup"}})
			if err != nil {
				t.Fatal(err)
			}
			params, err := chat.(*responsesModel).request([]*schema.Message{schema.UserMessage("draw")}, withResponsesImageGeneration(true), withResponsesTuning(effort, nil))
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(params)
			var body map[string]any
			if err := json.Unmarshal(encoded, &body); err != nil {
				t.Fatal(err)
			}
			tool := body["tools"].([]any)[1].(map[string]any)
			if tool["model"] != "gpt-image-2.6-sunburst" || tool["complexity"] != "high" || body["model"] != "gpt-6.1-sol" {
				t.Fatalf("image parameters are not independent: %s", encoded)
			}
			if effort != "" && body["reasoning"].(map[string]any)["effort"] != effort {
				t.Fatal("conversation reasoning was changed")
			}
		}
	}
	for _, endpoint := range []string{"https://gateway.example/v1", "https://api.openai.com.evil.test/v1", "https://api.openai.com/tenant/v1", "https://api.openai.com/v1?tenant=other", "http://api.openai.com/v1", "https://api.openai.com:8443/v1", "https://user@api.openai.com/v1"} {
		chat, err := newResponsesModel(context.Background(), Config{BaseURL: endpoint}, "custom-model", nil)
		if err != nil {
			t.Fatal(err)
		}
		params, err := chat.(*responsesModel).request([]*schema.Message{schema.UserMessage("draw")}, withResponsesImageGeneration(true), withResponsesTuning("high", nil))
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(params)
		if strings.Contains(string(encoded), "gpt-image-2.6-sunburst") || strings.Contains(string(encoded), `"complexity"`) {
			t.Fatalf("official parameters leaked to gateway %s", endpoint)
		}
	}
}

func TestResponsesImageGenerationSurvivesRestartAndContinuesEditing(t *testing.T) {
	ref, data := testImageAsset(t)
	importer := &testModelImageImporter{ref: ref}
	resolver := &testAssetResolver{ref: ref, data: data}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		encoded, _ := json.Marshal(body)
		if body["model"] != "image-model" || body["store"] != false || !strings.Contains(string(encoded), `"type":"image_generation"`) || !strings.Contains(string(encoded), "image_generation tool is enabled") {
			t.Error("selected model lost the image generation tool or instruction")
		}
		var output []any
		if calls.Add(1) == 1 {
			output = []any{map[string]any{"type": "image_generation_call", "id": "ig_fixture", "status": "completed", "result": testImageBase64, "output_format": "png"}}
		} else {
			found := false
			for _, raw := range body["input"].([]any) {
				item := raw.(map[string]any)
				if item["type"] == "image_generation_call" {
					// Actual Responses rejects unpersisted image call IDs even
					// when their base64 result is included alongside the ID.
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"error":{"message":"Item not found; store is false"}}`))
					return
				}
				itemJSON, _ := json.Marshal(item)
				if strings.Contains(string(itemJSON), `"type":"input_image"`) && strings.Contains(string(itemJSON), "data:image/png;base64,"+testImageBase64) {
					found = true
				}
			}
			if !found {
				t.Error("previous image did not reach the edit request after restart")
			}
			output = []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "image received"}}}}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Load() == 1 {
			writeResponseAdapterEvent(w, map[string]any{"type": "response.image_generation_call.partial_image", "partial_image_b64": testImageBase64})
			writeResponseAdapterEvent(w, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": output[0]})
		}
		writeResponseAdapterEvent(w, map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": output, "usage": map[string]any{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}}})
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "history.db")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	images := 0
	for turn := 0; turn < 2; turn++ {
		store, err := sqlite.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		e := newTestEngine(t, Config{
			Provider: "openai", Model: "root", BaseURL: server.URL + "/v1",
			ModelRoutes:  map[string]harness.ModelRoute{"selected": {Provider: "openai", API: harness.ModelAPIResponses, Model: "image-model", BaseURL: server.URL + "/v1"}},
			Media:        harness.MediaConfig{ImageGenerationModels: []string{"selected"}},
			SessionStore: store, AssetResolver: resolver, ModelImageImporter: importer,
		})
		req := request("image-first")
		if turn == 1 {
			req = request("image-edit")
		}
		req.Session.Model = "selected"
		result, runErr := e.Run(ctx, req, func(_ context.Context, event harness.RunEvent) error {
			assertNoImageBytes(t, event)
			if event.Kind == "image_delta" {
				images++
				if len(event.Content) != 1 || event.Content[0].Asset == nil || *event.Content[0].Asset != ref {
					t.Error("generated image did not use a session asset")
				}
			}
			return nil
		}, allowTool)
		history, loadErr := store.LoadEvents(ctx, ref.SessionID, &adk.LoadSessionEventsRequest{})
		assertNoImageBytes(t, history)
		closeErr := store.Close()
		if runErr != nil || loadErr != nil || closeErr != nil || result.StopReason != "end_turn" {
			t.Fatalf("image turn %d: result=%+v errors=%v", turn, result, errors.Join(runErr, loadErr, closeErr))
		}
	}
	if images != 1 || importer.calls.Load() != 1 || calls.Load() != 2 || resolver.calls.Load() == 0 {
		t.Fatalf("image duplicate/loss: images=%d imported=%d requests=%d hydrated=%d", images, importer.calls.Load(), calls.Load(), resolver.calls.Load())
	}
}

func TestResponsesGeneratedImageDoesNotLeakProviderIdentityAcrossOrigins(t *testing.T) {
	var response responses.Response
	encoded, _ := json.Marshal(map[string]any{"output": []any{map[string]any{"type": "image_generation_call", "id": "ig_private", "status": "completed", "result": testImageBase64}}})
	if err := json.Unmarshal(encoded, &response); err != nil {
		t.Fatal(err)
	}
	message, err := responsesOutput(response, false)
	if err != nil {
		t.Fatal(err)
	}
	assertNoImageBytes(t, message.Extra)
	message.Extra[responsesOriginKey] = "old-endpoint"
	items, err := responsesInputForOrigin([]*schema.Message{message}, "new-endpoint")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(items)
	if strings.Contains(string(encoded), "ig_private") || !strings.Contains(string(encoded), "data:image/png;base64,"+testImageBase64) || !strings.Contains(string(encoded), "not new user instructions") {
		t.Fatal("image context or origin isolation was lost")
	}
}

func TestResponsesGeneratedImagePreservesStreamedCaptionAndHistoryOrder(t *testing.T) {
	m := responseAdapterFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeResponseAdapterEvent(w, map[string]any{"type": "response.output_text.delta", "delta": "Before. "})
		writeResponseAdapterEvent(w, map[string]any{"type": "response.output_text.delta", "delta": "After."})
		writeResponseAdapterEvent(w, map[string]any{"type": "response.completed", "response": map[string]any{"output": []any{
			map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Before. "}}},
			map[string]any{"type": "image_generation_call", "id": "ig_caption", "status": "completed", "result": testImageBase64},
			map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "After."}}},
		}}})
	})
	message, err := m.Generate(context.Background(), []*schema.Message{schema.UserMessage("draw")}, withResponsesImageGeneration(true))
	if err != nil {
		t.Fatal(err)
	}
	if message.Content != "Before. After." || len(message.AssistantGenMultiContent) != 1 {
		t.Fatal("streamed caption or image was duplicated/lost")
	}
	assertNoImageBytes(t, message.Extra)
	items, err := responsesInput([]*schema.Message{message})
	if err != nil || len(items) != 3 {
		t.Fatalf("image history order: count=%d err=%v", len(items), err)
	}
	for i, want := range []string{"Before. ", "data:image/png;base64," + testImageBase64, "After."} {
		encoded, _ := json.Marshal(items[i])
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("image history item %d lost its position", i)
		}
	}
}

func TestResponsesRejectsIncompleteAndOversizedImages(t *testing.T) {
	for _, test := range []struct {
		name, status, format, result string
		count                        int
	}{
		{"unfinished", "generating", "png", testImageBase64, 1},
		{"missing", "completed", "png", "", 1},
		{"unsupported", "completed", "svg", testImageBase64, 1},
		{"too_many", "completed", "png", testImageBase64, harness.MaxInputImagesPerTurn + 1},
		{"too_large", "completed", "png", strings.Repeat("A", int((harness.MaxInputImageBytes/3+1)*4)), 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output []any
			for i := 0; i < test.count; i++ {
				output = append(output, map[string]any{"type": "image_generation_call", "id": strings.Repeat("i", i+1), "status": test.status, "result": test.result, "output_format": test.format})
			}
			encoded, _ := json.Marshal(map[string]any{"output": output})
			var response responses.Response
			if err := json.Unmarshal(encoded, &response); err != nil {
				t.Fatal(err)
			}
			if _, err := responsesOutput(response, false); !errors.Is(err, harness.ErrModelResponseInvalid) {
				t.Fatalf("invalid generated output was accepted: %v", err)
			}
		})
	}
}

func TestResponsesPrivateModelCallsDoNotGenerateImages(t *testing.T) {
	m := &trackedModel{media: &mediaProjection{model: "image", allowImageGeneration: true, policy: harness.MediaConfig{ImageGenerationModels: []string{"image"}}, importer: &testModelImageImporter{}, publish: func(context.Context, harness.Content) error { return nil }}}
	for _, native := range []bool{false, true} {
		m.nativeEndpoint = native
		opts, err := m.providerOptions(nil, []model.Option{withResponsesImageGeneration(true)})
		if err != nil {
			t.Fatal(err)
		}
		params, err := (&responsesModel{name: "fixture"}).request([]*schema.Message{schema.UserMessage("summarize")}, opts...)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(params)
		if strings.Contains(string(encoded), `"type":"image_generation"`) != native {
			t.Fatal("private model call inherited image generation")
		}
	}
}

func TestResponsesImageFailureRetainsCauseWithoutAuthorizingReplay(t *testing.T) {
	for _, test := range []struct{ code, detail, kind string }{
		{"permission_denied", "image_generation access denied", "access_denied"},
		{"insufficient_quota", "image generation quota exhausted", "quota_exceeded"},
		{"invalid_request_error", "image_generation is not supported", "unsupported"},
		{"image_generation_failed", "generation could not finish", "failed"},
		{"", "Complexity 'high' is not supported for this image model.", "unsupported_complexity"},
	} {
		t.Run(test.kind, func(t *testing.T) {
			encoded, _ := json.Marshal(map[string]any{"output": []any{map[string]any{"type": "image_generation_call", "id": "ig_failed", "status": "failed", "error": map[string]any{"code": test.code, "message": test.detail}}}})
			var response responses.Response
			if err := json.Unmarshal(encoded, &response); err != nil {
				t.Fatal(err)
			}
			_, err := responsesOutput(response, false)
			var failure *harness.ImageGenerationError
			var api *chatapi.APIError
			if !errors.As(err, &failure) || failure.Kind != test.kind || !errors.As(err, &api) || api.Message != test.detail || errors.Is(err, harness.ErrModelResponseInvalid) || retryableModelRateLimit(err) {
				t.Fatalf("image failure lost its classification/cause: %v", err)
			}
		})
	}
	ordinary := &chatapi.APIError{HTTPStatusCode: 403, Code: "permission_denied", Message: "model access denied"}
	if classifyResponsesImageError(ordinary) != ordinary {
		t.Fatal("unrelated model failure was attributed to images")
	}
	image := &chatapi.APIError{HTTPStatusCode: 403, Code: "permission_denied", Message: "image_generation access denied"}
	var failure *harness.ImageGenerationError
	if err := classifyResponsesImageError(image); !errors.As(err, &failure) || failure.Kind != "access_denied" || !errors.Is(err, image) {
		t.Fatal("image rejection lost provider cause")
	}
	complexity := &chatapi.APIError{HTTPStatusCode: 500, Message: "Complexity 'high' is not supported for this image model."}
	if err := classifyResponsesImageError(complexity); !errors.As(err, &failure) || failure.Kind != "unsupported_complexity" || !errors.Is(err, complexity) || retryableModelRateLimit(err) {
		t.Fatal("complexity rejection lost classification or allowed replay")
	}
}

func TestImageGenerationInstructionDescribesEffectiveConfiguration(t *testing.T) {
	p := &mediaProjection{model: "selected", allowImageGeneration: true, importer: &testModelImageImporter{}, publish: func(context.Context, harness.Content) error { return nil }}
	if p.imageGenerationEnabled() || !strings.Contains(p.imageGenerationInstruction(), "not enabled in the selected model configuration") {
		t.Fatal("disabled setting was described as unsupported model")
	}
	p.policy.ImageGenerationModels = []string{"other"}
	if p.imageGenerationEnabled() {
		t.Fatal("another model's enablement leaked")
	}
	p.policy.ImageGenerationModels = []string{"selected"}
	if !p.imageGenerationEnabled() || !strings.Contains(p.imageGenerationInstruction(), "image_generation tool is enabled") {
		t.Fatal("configured tool and instruction disagree")
	}
	p.allowImageGeneration = false
	if p.imageGenerationEnabled() || !strings.Contains(p.imageGenerationInstruction(), "session mode or tool policy") {
		t.Fatal("session restriction was not explained")
	}
	p.allowImageGeneration = true
	p.importer = nil
	if p.imageGenerationEnabled() || !strings.Contains(p.imageGenerationInstruction(), "save and display") {
		t.Fatal("host limitation was not explained")
	}
}
