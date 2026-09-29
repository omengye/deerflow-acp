package eino

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	einosession "github.com/cloudwego/eino/adk/session"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

const testImageBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aZ1sAAAAASUVORK5CYII="

func testImageAsset(t *testing.T) (harness.AssetRef, []byte) {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(testImageBase64)
	if err != nil {
		t.Fatal(err)
	}
	return harness.AssetRef{ID: "asset-image", SessionID: "test-session", SHA256: fmt.Sprintf("%x", sha256.Sum256(data)), Size: int64(len(data)), MimeType: "image/png", Name: "sample.png", Kind: harness.AssetImage}, data
}

type testAssetResolver struct {
	ref   harness.AssetRef
	data  []byte
	calls atomic.Int32
}

type testToolImageImporter struct {
	ref   harness.AssetRef
	data  string
	calls atomic.Int32
}

type testModelImageImporter struct {
	ref   harness.AssetRef
	calls atomic.Int32
}

type imageCommitOrderStore struct {
	adk.SessionEventStore[*schema.Message]
	published *atomic.Bool
	checked   atomic.Int32
}

func (s *imageCommitOrderStore) AppendEvents(ctx context.Context, sessionID string, events []*adk.SessionEvent[*schema.Message]) error {
	for _, event := range events {
		if event == nil || event.Message == nil {
			continue
		}
		for _, part := range event.Message.AssistantGenMultiContent {
			if part.Type == schema.ChatMessagePartTypeImageURL {
				s.checked.Add(1)
				if !s.published.Load() {
					return errors.New("native image history preceded durable image event")
				}
			}
		}
	}
	return s.SessionEventStore.AppendEvents(ctx, sessionID, events)
}

func (i *testModelImageImporter) StageModelImage(_ context.Context, session harness.Session, runID string, image harness.Content) (harness.Content, error) {
	i.calls.Add(1)
	if session.ID != i.ref.SessionID || runID == "" || image.Type != "image" || image.Data != testImageBase64 || image.MimeType != i.ref.MimeType {
		return harness.Content{}, harness.ErrInvalidInput
	}
	return imageContent(i.ref), nil
}

func TestGeneratedModelImageIsReferencedEmittedAndRehydrated(t *testing.T) {
	ref, data := testImageAsset(t)
	importer := &testModelImageImporter{ref: ref}
	resolver := &testAssetResolver{ref: ref, data: data}
	var published atomic.Bool
	store := &imageCommitOrderStore{SessionEventStore: einosession.NewInMemoryStore[*schema.Message](nil), published: &published}
	var rehydrated atomic.Bool
	fake := &scriptedModel{stream: func(_ context.Context, call int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call == 0 {
			inline := testImageBase64
			return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, AssistantGenMultiContent: []schema.MessageOutputPart{{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageOutputImage{MessagePartCommon: schema.MessagePartCommon{Base64Data: &inline, MIMEType: "image/png"}}}}}}), nil
		}
		for _, message := range input {
			for _, part := range message.AssistantGenMultiContent {
				if part.Image != nil && part.Image.Base64Data != nil && *part.Image.Base64Data == testImageBase64 {
					rehydrated.Store(true)
				}
			}
		}
		return textStream("done"), nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake, Model: "vision", Media: harness.MediaConfig{VisionModels: []string{"vision"}}, AssetResolver: resolver, ModelImageImporter: importer, SessionStore: store})
	var events []harness.RunEvent
	for _, id := range []string{"generated-first", "generated-second"} {
		if _, err := e.Run(context.Background(), request(id), func(_ context.Context, event harness.RunEvent) error {
			if event.Kind == "image_delta" {
				published.Store(true)
			}
			events = append(events, event)
			return nil
		}, allowTool); err != nil {
			t.Fatal(err)
		}
	}
	if importer.calls.Load() != 1 || !rehydrated.Load() || store.checked.Load() == 0 {
		t.Fatalf("generated image was not staged, committed and replayed: importer=%d replay=%v nativeChecks=%d", importer.calls.Load(), rehydrated.Load(), store.checked.Load())
	}
	var images int
	for _, event := range events {
		if event.Kind == "image_delta" {
			images++
			if len(event.Content) != 1 || event.Content[0].Asset == nil || *event.Content[0].Asset != ref {
				t.Fatalf("invalid generated image event: %+v", event)
			}
		}
	}
	if images != 1 {
		t.Fatalf("image events=%d", images)
	}
	assertNoImageBytes(t, events)
	stored, err := e.config.SessionStore.LoadEvents(context.Background(), ref.SessionID, &adk.LoadSessionEventsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	assertNoImageBytes(t, stored)
}

func (i *testToolImageImporter) StageToolImages(_ context.Context, session harness.Session, runID, callID string, images []harness.Content) ([]harness.Content, error) {
	i.calls.Add(1)
	if session.ID != i.ref.SessionID || runID == "" || callID == "" || len(images) != 1 || images[0].Data != i.data {
		return nil, harness.ErrInvalidInput
	}
	return []harness.Content{imageContent(i.ref)}, nil
}

func TestNormalizeToolImagesUsesStagedReference(t *testing.T) {
	ref, _ := testImageAsset(t)
	importer := &testToolImageImporter{ref: ref, data: testImageBase64}
	data := testImageBase64
	result := &schema.ToolResult{Parts: []schema.ToolOutputPart{{Type: schema.ToolPartTypeText, Text: "caption"}, {Type: schema.ToolPartTypeImage, Image: &schema.ToolOutputImage{MessagePartCommon: schema.MessagePartCommon{Base64Data: &data, MIMEType: "image/png"}}}}}
	safe, err := normalizeToolImages(context.Background(), result, request("tool-image"), "image-call", importer)
	if err != nil || importer.calls.Load() != 1 {
		t.Fatalf("normalize: %v", err)
	}
	if len(safe.Parts) != 2 || safe.Parts[0].Text != "caption" || safe.Parts[1].Image == nil || safe.Parts[1].Image.URL == nil || *safe.Parts[1].Image.URL != harness.AssetURI(ref) {
		t.Fatalf("lost tool content: %+v", safe)
	}
	assertNoImageBytes(t, safe)
	if _, err := normalizeToolImages(context.Background(), result, request("tool-image"), "image-call", nil); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatalf("missing importer accepted: %v", err)
	}
}

func (r *testAssetResolver) Resolve(_ context.Context, sessionID string, ref harness.AssetRef) ([]byte, error) {
	r.calls.Add(1)
	if sessionID != r.ref.SessionID || ref != r.ref {
		return nil, harness.ErrPermissionDenied
	}
	return append([]byte(nil), r.data...), nil
}

func imageContent(ref harness.AssetRef) harness.Content {
	return harness.Content{Type: "image", URI: harness.AssetURI(ref), Asset: &ref, MimeType: ref.MimeType, Name: ref.Name, Size: &ref.Size}
}

func assertNoImageBytes(t *testing.T, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(testImageBase64)) || bytes.Contains(data, []byte("provider-mutated")) {
		t.Fatal("model hydration mutated durable history or leaked image bytes")
	}
}

func TestImageHydrationPreservesDurableHistoryAndCheckpoint(t *testing.T) {
	ref, data := testImageAsset(t)
	resolver := &testAssetResolver{ref: ref, data: data}
	started := make(chan struct{})
	fake := &scriptedModel{stream: func(ctx context.Context, call int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		found := false
		for _, message := range input {
			if len(message.UserInputMultiContent) == 0 {
				continue
			}
			parts := message.UserInputMultiContent
			if len(parts) != 3 || parts[0].Text != "before" || parts[2].Text != "after" {
				t.Errorf("text/image order changed: %+v", parts)
			}
			image := parts[1].Image
			if image == nil || image.URL != nil || image.Base64Data == nil || *image.Base64Data != testImageBase64 {
				t.Error("provider did not receive hydrated image")
			}
			parts[0].Text = "provider-mutated"
			if image != nil {
				corrupted := "provider-mutated"
				image.Base64Data = &corrupted
			}
			found = true
		}
		if !found {
			t.Error("image disappeared from restored model history")
		}
		if call > 0 {
			return textStream("done"), nil
		}
		r, w := schema.Pipe[*schema.Message](1)
		go func() { defer w.Close(); close(started); <-ctx.Done(); w.Send(nil, ctx.Err()) }()
		return r, nil
	}}
	e := newTestEngine(t, Config{ChatModel: fake, Model: "vision", Media: harness.MediaConfig{VisionModels: []string{"vision"}}, AssetResolver: resolver})
	req := request("image-cancel")
	req.Input = []harness.Content{{Type: "text", Text: "before"}, imageContent(ref), {Type: "text", Text: "after"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := e.Run(ctx, req, nil, nil); done <- err }()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("ended before model: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("model not started")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("model did not stop")
	}
	saved, err := loadCheckpointEnvelope(context.Background(), e.config.CheckpointStore, CheckpointID(req.RunID))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(saved.Native, []byte(testImageBase64)) || bytes.Contains(saved.Native, []byte("provider-mutated")) {
		t.Fatal("checkpoint contains hydrated or mutated data")
	}
	e = newTestEngine(t, e.config)
	if _, err := e.Resume(context.Background(), request("image-resume"), CheckpointID(req.RunID), nil, nil); err != nil {
		t.Fatal(err)
	}
	e = newTestEngine(t, e.config)
	if _, err := e.Run(context.Background(), request("next-text"), nil, nil); err != nil {
		t.Fatal(err)
	}
	events, err := e.config.SessionStore.LoadEvents(context.Background(), req.Session.ID, &adk.LoadSessionEventsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	assertNoImageBytes(t, events)
	if resolver.calls.Load() != 3 {
		t.Fatalf("resolver calls=%d", resolver.calls.Load())
	}
	// Switching to a text-only selected model must reject historical images too.
	next := request("text-only")
	next.Session.Model = "text-only"
	if _, err := e.Run(context.Background(), next, nil, nil); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatalf("text-only model accepted image history: %v", err)
	}
	if fake.calls != 3 {
		t.Fatalf("text-only provider was called: %d", fake.calls)
	}
}

func TestMediaHydrationRejectsUnauthorizedAndUnnormalizedInputs(t *testing.T) {
	ref, data := testImageAsset(t)
	resolver := &testAssetResolver{ref: ref, data: data}
	p := &mediaProjection{sessionID: ref.SessionID, model: "vision", policy: harness.MediaConfig{VisionModels: []string{"vision"}}, resolver: resolver}
	message, err := convertContent(schema.User, []harness.Content{imageContent(ref)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.hydrate(context.Background(), []*schema.Message{message}, []model.Option{model.WithModel("text-only")}); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatalf("per-call model override bypassed vision policy: %v", err)
	}
	foreign := *p
	foreign.sessionID = "other-session"
	if _, err := foreign.hydrate(context.Background(), []*schema.Message{message}, nil); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatalf("foreign session accepted: %v", err)
	}
	if resolver.calls.Load() != 0 {
		t.Fatal("read bytes before capability or session check")
	}
	resolver.data = []byte("wrong data")
	if _, err := p.hydrate(context.Background(), []*schema.Message{message}, nil); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatalf("corrupt resolver content accepted: %v", err)
	}
	for _, content := range []harness.Content{
		{Type: "image", Data: testImageBase64, MimeType: "image/png"},
		{Type: "image", URI: "https://example.invalid/image.png", MimeType: "image/png"},
		{Type: "resource_link", URI: "https://example.invalid/image.png"},
		{Type: "resource_link", URI: "file:///private/file.txt"},
	} {
		if _, err := convertContent(schema.User, []harness.Content{content}); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatalf("unsupported reference accepted: %v", err)
		}
	}
}

func TestAttachmentReferencesPreserveOrderWithoutReadingContent(t *testing.T) {
	message, err := convertContent(schema.User, []harness.Content{{Type: "text", Text: "before"}, {Type: "resource_link", URI: "https://example.invalid/report.pdf", MimeType: "application/pdf", Name: "report.pdf"}, {Type: "text", Text: "after"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(message.Content, "before") || !strings.HasSuffix(message.Content, "after") || !strings.Contains(message.Content, "contents have not been read") {
		t.Fatalf("attachment projection=%q", message.Content)
	}
	projected, err := ProjectToolContent([]harness.Content{{Type: "resource_link", URI: "https://example.invalid/report.pdf", MimeType: "application/pdf"}})
	if err != nil {
		t.Fatal(err)
	}
	parts, err := projected.ToMessageInputParts()
	if err != nil {
		t.Fatal(err)
	}
	p := &mediaProjection{sessionID: "test-session"}
	input, err := p.hydrate(context.Background(), []*schema.Message{{Role: schema.Tool, UserInputMultiContent: parts}}, nil)
	if err != nil || input[0].UserInputMultiContent[0].Type != schema.ChatMessagePartTypeText {
		t.Fatalf("provider received fetchable file: %+v err=%v", input, err)
	}
}

func TestImageBudgetIgnoresBase64LengthAndSettlesUsage(t *testing.T) {
	var estimates []int64
	for _, data := range []string{"tiny", strings.Repeat("x", 1024*1024)} {
		input := []*schema.Message{{Role: schema.User, UserInputMultiContent: []schema.MessageInputPart{{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageInputImage{MessagePartCommon: schema.MessagePartCommon{Base64Data: &data, MIMEType: "image/png"}}}}}}
		b := &runBudget{limits: harness.BudgetLimits{MaxTokens: 10000, MaxOutputTokens: 100}}
		r, _, err := b.reserve(input, nil)
		if err != nil {
			t.Fatal(err)
		}
		estimates = append(estimates, r.input)
		if r.input < estimatedImageTokens {
			t.Fatalf("missing image estimate: %d", r.input)
		}
		if err := r.observe(&schema.Message{ResponseMeta: &schema.ResponseMeta{Usage: &schema.TokenUsage{PromptTokens: 30, CompletionTokens: 7, TotalTokens: 37}}}); err != nil {
			t.Fatal(err)
		}
		u := r.settle()
		if b.spent != 37 || u.Estimated || *input[0].UserInputMultiContent[0].Image.Base64Data != data {
			t.Fatalf("settlement or mutation: usage=%+v", u)
		}
	}
	if estimates[0] != estimates[1] {
		t.Fatalf("base64 counted as text tokens: %v", estimates)
	}
}

func TestGeneratedImageHasBoundedOutputTokenEstimate(t *testing.T) {
	for _, data := range []string{"tiny", strings.Repeat("x", 1024*1024)} {
		b := &runBudget{limits: harness.BudgetLimits{MaxTokens: 10000, MaxOutputTokens: 10000}}
		r, _, err := b.reserve([]*schema.Message{{Role: schema.User, Content: "draw"}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		msg := &schema.Message{Role: schema.Assistant, AssistantGenMultiContent: []schema.MessageOutputPart{{Type: schema.ChatMessagePartTypeImageURL, Image: &schema.MessageOutputImage{MessagePartCommon: schema.MessagePartCommon{Base64Data: &data, MIMEType: "image/png"}}}}}
		if err = r.observe(msg); err != nil {
			t.Fatal(err)
		}
		if got := r.currentUsage().OutputTokens; got != estimatedImageTokens {
			t.Fatalf("estimated output tokens=%d", got)
		}
	}
}

type mediaResultTool struct{ result *schema.ToolResult }

func (*mediaResultTool) Info(ctx context.Context) (*schema.ToolInfo, error) {
	return (&recordingTool{}).Info(ctx)
}
func (t *mediaResultTool) InvokableRun(context.Context, *schema.ToolArgument, ...tool.Option) (*schema.ToolResult, error) {
	return t.result, nil
}

type mediaStreamTool struct{ mediaResultTool }

func (t *mediaStreamTool) StreamableRun(context.Context, *schema.ToolArgument, ...tool.Option) (*schema.StreamReader[*schema.ToolResult], error) {
	return schema.StreamReaderFromArray([]*schema.ToolResult{t.result}), nil
}

func TestRawToolMediaNeverEntersNativeHistoryOrReceipts(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			data := testImageBase64
			result := &schema.ToolResult{Parts: []schema.ToolOutputPart{{Type: schema.ToolPartTypeImage, Image: &schema.ToolOutputImage{MessagePartCommon: schema.MessagePartCommon{Base64Data: &data, MIMEType: "image/png"}}}}}
			var mediaTool tool.BaseTool = &mediaResultTool{result: result}
			if stream {
				mediaTool = &mediaStreamTool{mediaResultTool{result: result}}
			}
			e := newTestEngine(t, Config{ChatModel: toolScript(), Tools: []tool.BaseTool{mediaTool}})
			var events []harness.RunEvent
			_, err := e.Run(context.Background(), request("raw-tool"), func(_ context.Context, event harness.RunEvent) error { events = append(events, event); return nil }, allowTool)
			if !errors.Is(err, harness.ErrInvalidInput) {
				t.Fatalf("raw tool media accepted: %v", err)
			}
			assertNoImageBytes(t, events)
			stored, err := e.config.SessionStore.LoadEvents(context.Background(), "test-session", &adk.LoadSessionEventsRequest{})
			if err != nil {
				t.Fatal(err)
			}
			assertNoImageBytes(t, stored)
			failed := false
			for _, event := range events {
				if event.Kind == "tool_end" && event.Status == "failed" && event.Receipt != nil {
					failed = true
				}
			}
			if !failed {
				t.Fatal("rejected tool media lost failure receipt")
			}
		})
	}
}

func TestStreamedEnhancedToolImageUsesStagedReference(t *testing.T) {
	ref, bytes := testImageAsset(t)
	importer := &testToolImageImporter{ref: ref, data: testImageBase64}
	resolver := &testAssetResolver{ref: ref, data: bytes}
	data := testImageBase64
	result := &schema.ToolResult{Parts: []schema.ToolOutputPart{{Type: schema.ToolPartTypeText, Text: "caption"}, {Type: schema.ToolPartTypeImage, Image: &schema.ToolOutputImage{MessagePartCommon: schema.MessagePartCommon{Base64Data: &data, MIMEType: "image/png"}}}}}
	engine := newTestEngine(t, Config{ChatModel: toolScript(), Model: "vision", Media: harness.MediaConfig{VisionModels: []string{"vision"}}, Tools: []tool.BaseTool{&mediaStreamTool{mediaResultTool{result: result}}}, ToolImageImporter: importer, AssetResolver: resolver})
	var events []harness.RunEvent
	req := request("stream-image")
	req.Session.Model = "vision"
	if _, err := engine.Run(context.Background(), req, func(_ context.Context, event harness.RunEvent) error { events = append(events, event); return nil }, allowTool); err != nil {
		t.Fatal(err)
	}
	if importer.calls.Load() != 1 {
		t.Fatal("stream image was not staged")
	}
	seenPending, seenEnd := false, false
	for _, event := range events {
		if event.Kind != "tool_update" && event.Kind != "tool_end" {
			continue
		}
		for _, c := range event.Content {
			if c.Type == "image" && c.Asset != nil && *c.Asset == ref {
				if event.Kind == "tool_update" {
					t.Fatal("uncommitted image reference entered update")
				}
				seenEnd = true
			}
			if event.Kind == "tool_update" && c.Type == "text" && strings.Contains(c.Text, "Image staged") {
				seenPending = true
			}
		}
	}
	if !seenPending || !seenEnd {
		t.Fatalf("stream image missing in update or terminal event: %+v", events)
	}
	assertNoImageBytes(t, events)
	stored, err := engine.config.SessionStore.LoadEvents(context.Background(), "test-session", &adk.LoadSessionEventsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	assertNoImageBytes(t, stored)
}

func TestToolMediaProjectionRejectsConflictingPartsAndRemovesOpaquePayload(t *testing.T) {
	data := testImageBase64
	if _, err := validateToolMedia(&schema.ToolResult{Parts: []schema.ToolOutputPart{{Type: schema.ToolPartTypeText, Text: "caption", Image: &schema.ToolOutputImage{MessagePartCommon: schema.MessagePartCommon{Base64Data: &data}}}}}, "test-session"); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatalf("conflicting raw media fields accepted: %v", err)
	}
	ref, _ := testImageAsset(t)
	result, err := ProjectToolContent([]harness.Content{imageContent(ref)})
	if err != nil {
		t.Fatal(err)
	}
	result.Parts[0].Image.Extra = map[string]any{"untrusted_bytes": data}
	result.Parts[0].Extra["untrusted_bytes"] = data
	safe, err := validateToolMedia(result, ref.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	assertNoImageBytes(t, safe)
}

func TestSubagentHydratesToolAssetsInSameSession(t *testing.T) {
	ref, data := testImageAsset(t)
	result, err := ProjectToolContent([]harness.Content{imageContent(ref)})
	if err != nil {
		t.Fatal(err)
	}
	resolver := &testAssetResolver{ref: ref, data: data}
	var childSawImage atomic.Bool
	fake := &scriptedModel{stream: func(_ context.Context, call int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		switch call {
		case 0:
			return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "delegate-image", Type: "function", Function: schema.FunctionCall{Name: "task", Arguments: `{"subagent_type":"general-purpose","prompt":"inspect image using read_workspace","description":"inspect"}`}}}}}), nil
		case 1:
			return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: "child-image", Type: "function", Function: schema.FunctionCall{Name: "read_workspace", Arguments: `{}`}}}}}), nil
		case 2:
			for _, message := range input {
				for _, part := range message.UserInputMultiContent {
					if part.Image != nil && part.Image.Base64Data != nil && *part.Image.Base64Data == testImageBase64 {
						childSawImage.Store(true)
					}
				}
			}
		}
		return textStream("image inspected"), nil
	}}
	e, err := New(context.Background(), Config{ChatModel: fake, Model: "vision", Media: harness.MediaConfig{VisionModels: []string{"vision"}}, AssetResolver: resolver, Tools: []tool.BaseTool{&mediaResultTool{result: result}}})
	if err != nil {
		t.Fatal(err)
	}
	var events []harness.RunEvent
	if _, err := e.Run(context.Background(), request("child-media"), func(_ context.Context, event harness.RunEvent) error { events = append(events, event); return nil }, allowTool); err != nil {
		t.Fatal(err)
	}
	if !childSawImage.Load() || resolver.calls.Load() == 0 {
		t.Fatal("child did not resolve the same-session tool image")
	}
	assertNoImageBytes(t, events)
	stored, err := e.config.SessionStore.LoadEvents(context.Background(), ref.SessionID, &adk.LoadSessionEventsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	assertNoImageBytes(t, stored)
}
