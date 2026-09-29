package deerflow

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func imageFixture(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 31, G: 73, B: 137, A: 255})
	var output bytes.Buffer
	if err := png.Encode(&output, img); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(output.Bytes())
}

func TestSDKMediaHydratesForProviderButPersistsOnlyReferences(t *testing.T) {
	data := imageFixture(t)
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			http.Error(w, "read", 500)
			return
		}
		calls.Add(1)
		if !bytes.Contains(body, []byte("data:image/png;base64,"+data)) {
			t.Error("provider did not receive hydrated original image")
		}
		if bytes.Contains(body, []byte("deerflow-asset://")) {
			t.Error("internal asset URI leaked to model provider")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Image inspected\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer provider.Close()
	cfg := Config{DataDir: t.TempDir(), Provider: "openai", Model: "vision", Models: []harness.ConfigValue{{Value: "text-only"}}, APIKey: "fixture", BaseURL: provider.URL + "/v1", DisableSubagents: true, Media: harness.MediaConfig{VisionModels: []string{"vision"}}}
	c, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	workspace := t.TempDir()
	session, err := c.NewSession(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	input := []harness.Content{{Type: "text", Text: "Describe this image"}, {Type: "image", Data: data, MimeType: "image/png"}}
	if _, err = c.Run(context.Background(), session.ID, input, nil, nil); err != nil {
		t.Fatal(err)
	}
	if input[1].Data != data || input[1].Asset != nil {
		t.Fatal("caller input mutated")
	}
	assertNoStoredBase64(t, c, data)
	history, err := c.service.Store.History(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	var ref *harness.AssetRef
	for _, event := range history {
		for _, part := range event.Content {
			if part.Type == "image" {
				ref = part.Asset
			}
		}
	}
	if ref == nil {
		t.Fatal("durable image reference missing")
	}
	if ref.SessionID != session.ID || ref.SHA256 == "" || ref.Size == 0 {
		t.Fatalf("asset metadata=%+v", ref)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.LoadSession(context.Background(), session.ID, workspace, true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Run(context.Background(), session.ID, []harness.Content{{Type: "text", Text: "Look at that image again"}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	assertNoStoredBase64(t, c, data)
	if calls.Load() != 2 {
		t.Fatalf("calls=%d", calls.Load())
	}
	if _, err = c.SetConfigOption(context.Background(), session.ID, "model", "text-only"); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Run(context.Background(), session.ID, input, nil, nil); err == nil {
		t.Fatal("image accepted for a non-vision model")
	}
	if calls.Load() != 2 {
		t.Fatal("non-vision request reached provider")
	}
}

func assertNoStoredBase64(t *testing.T, c *Client, data string) {
	t.Helper()
	for _, query := range []string{"SELECT content FROM harness_inputs", "SELECT event FROM harness_events", "SELECT payload FROM eino_session_events", "SELECT payload FROM eino_checkpoints"} {
		rows, err := c.store.DB().Query(query)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var stored []byte
			if err = rows.Scan(&stored); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			if bytes.Contains(stored, []byte(data)) {
				_ = rows.Close()
				t.Fatalf("image data embedded by %s", query)
			}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestSDKPresentFilesCommitsImmutableArtifactWithToolReceipt(t *testing.T) {
	workspace := t.TempDir()
	output := filepath.Join(workspace, ".deerflow", "outputs", "report.txt")
	if err := os.MkdirAll(filepath.Dir(output), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, []byte("ORIGINAL_REPORT"), 0600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			chunk := map[string]any{"id": "fixture", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "present-report", "type": "function", "function": map[string]any{"name": "present_files", "arguments": `{"paths":[".deerflow/outputs/report.txt"]}`}}}}, "finish_reason": "tool_calls"}}}
			encoded, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", encoded)
		} else {
			if !bytes.Contains(body, []byte("report.txt")) {
				t.Error("artifact reference did not reach model")
			}
			fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Report ready\"},\"finish_reason\":\"stop\"}]}\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer provider.Close()
	cfg := Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", APIKey: "fixture", BaseURL: provider.URL + "/v1", DisableSubagents: true}
	c, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	session, err := c.NewSession(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	var terminal harness.RunEvent
	_, err = c.Run(context.Background(), session.ID, []harness.Content{{Type: "text", Text: "Present report"}}, func(_ context.Context, event harness.RunEvent) error {
		if event.Kind == "tool_end" && event.ToolName == "present_files" {
			terminal = event
		}
		return nil
	}, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		return harness.AllowOnce, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var ref *harness.AssetRef
	for _, part := range terminal.Content {
		if part.Type == "resource_link" && part.Asset != nil {
			ref = part.Asset
		}
	}
	if ref == nil || terminal.Receipt == nil || terminal.Receipt.State != harness.ReceiptCompleted {
		t.Fatalf("artifact terminal=%+v", terminal)
	}
	if err = os.WriteFile(output, []byte("MUTATED_SOURCE"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := c.ResolveAsset(context.Background(), session.ID, *ref)
	if err != nil || string(data) != "ORIGINAL_REPORT" {
		t.Fatalf("snapshot=%q err=%v", data, err)
	}
	other, err := c.NewSession(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.ResolveAsset(context.Background(), other.ID, *ref); err == nil {
		t.Fatal("asset resolved through another session")
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.LoadSession(context.Background(), session.ID, workspace, true, nil); err != nil {
		t.Fatal(err)
	}
	artifacts, err := c.ListArtifacts(context.Background(), session.ID)
	if err != nil || len(artifacts) != 1 || artifacts[0].Asset == nil || artifacts[0].Asset.ID != ref.ID {
		t.Fatalf("artifacts=%+v err=%v", artifacts, err)
	}
	receipts, err := c.ListToolReceipts(context.Background(), session.ID)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("receipts=%+v err=%v", receipts, err)
	}
	encoded, _ := json.Marshal(receipts[0].Result)
	if !strings.Contains(string(encoded), ref.ID) {
		t.Fatal("artifact reference missing from durable receipt")
	}
	if calls.Load() != 2 {
		t.Fatal("reopening replayed artifact tool")
	}
}

func TestSDKViewImageUsesCommittedSnapshotInReadOnlyMode(t *testing.T) {
	data := imageFixture(t)
	workspace := t.TempDir()
	decoded, _ := base64.StdEncoding.DecodeString(data)
	if err := os.WriteFile(filepath.Join(workspace, "picture.png"), decoded, 0600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			if !bytes.Contains(body, []byte("view_image")) {
				t.Error("vision tool was not offered")
			}
			chunk := map[string]any{"id": "fixture", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "inspect-image", "type": "function", "function": map[string]any{"name": "view_image", "arguments": `{"path":"picture.png"}`}}}}, "finish_reason": "tool_calls"}}}
			encoded, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", encoded)
		} else {
			if !bytes.Contains(body, []byte("data:image/png;base64,"+data)) {
				t.Error("vision tool result was not hydrated for provider")
			}
			fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Image inspected\"},\"finish_reason\":\"stop\"}]}\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer provider.Close()
	c, err := Open(context.Background(), Config{DataDir: t.TempDir(), Provider: "openai", Model: "vision", APIKey: "fixture", BaseURL: provider.URL + "/v1", DisableSubagents: true, Media: harness.MediaConfig{VisionModels: []string{"vision"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	session, err := c.NewSession(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.SetConfigOption(context.Background(), session.ID, "approval", harness.ApprovalReadOnly); err != nil {
		t.Fatal(err)
	}
	_, err = c.Run(context.Background(), session.ID, []harness.Content{{Type: "text", Text: "Inspect picture.png"}}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		t.Error("read-only image requested approval")
		return harness.RejectOnce, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertNoStoredBase64(t, c, data)
	receipts, err := c.ListToolReceipts(context.Background(), session.ID)
	if err != nil || len(receipts) != 1 || receipts[0].State != harness.ReceiptCompleted {
		t.Fatalf("receipts=%+v err=%v", receipts, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("model calls=%d", calls.Load())
	}
	artifacts, err := c.ListArtifacts(context.Background(), session.ID)
	if err != nil || len(artifacts) != 0 {
		t.Fatalf("view_image published artifact: %+v err=%v", artifacts, err)
	}
}
