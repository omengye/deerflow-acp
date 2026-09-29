package tests

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestACPExecutableImageArtifactAndRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	bin := filepath.Join(t.TempDir(), "deerflow-acp-go")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.CommandContext(ctx, "go", "build", "-p=2", "-o", bin, "./cmd/deerflow-acp-go")
	build.Dir = ".."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	var imageData bytes.Buffer
	if err := png.Encode(&imageData, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	encodedImage := base64.StdEncoding.EncodeToString(imageData.Bytes())
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte("data:image/png;base64,"+encodedImage)) {
			t.Error("ACP image did not reach actual model adapter")
		}
		if bytes.Contains(body, []byte("deerflow-asset://")) && calls.Load() == 0 {
			t.Error("internal input reference reached provider")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			chunk := map[string]any{"id": "fixture", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "publish-output", "type": "function", "function": map[string]any{"name": "present_files", "arguments": `{"paths":["report.txt"]}`}}}}, "finish_reason": "tool_calls"}}}
			encoded, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", encoded)
		} else {
			fmt.Fprint(w, "data: {\"id\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Report ready\"},\"finish_reason\":\"stop\"}]}\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer provider.Close()
	workspace, dataDir := t.TempDir(), t.TempDir()
	output := filepath.Join(workspace, ".deerflow", "outputs", "report.txt")
	if err := os.MkdirAll(filepath.Dir(output), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, []byte("report snapshot"), 0600); err != nil {
		t.Fatal(err)
	}
	start := func() *wireProcess {
		p := launch(t, bin, dataDir, provider.URL+"/v1", "--vision-model", "fixture-model", "--disable-subagents")
		result := p.request(t, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}})
		var initialized struct {
			Capabilities struct {
				Prompt struct {
					Image bool `json:"image"`
				} `json:"promptCapabilities"`
			} `json:"agentCapabilities"`
		}
		if err := json.Unmarshal(result, &initialized); err != nil || !initialized.Capabilities.Prompt.Image {
			t.Fatalf("image capability missing: %s err=%v", result, err)
		}
		return p
	}
	p := start()
	created := p.request(t, "session/new", map[string]any{"cwd": workspace, "mcpServers": []any{}})
	var session struct {
		ID string `json:"sessionId"`
	}
	if err := json.Unmarshal(created, &session); err != nil || session.ID == "" {
		t.Fatalf("session=%s err=%v", created, err)
	}
	p.request(t, "session/prompt", map[string]any{"sessionId": session.ID, "prompt": []any{map[string]any{"type": "image", "data": encodedImage, "mimeType": "image/png"}, map[string]any{"type": "text", "text": "Inspect image and present report.txt"}}})
	list := p.request(t, "_deerflow/artifacts/list", map[string]any{"sessionId": session.ID})
	if !bytes.Contains(list, []byte(`"type":"resource_link"`)) || !bytes.Contains(list, []byte("file:")) || bytes.Contains(list, []byte(`"asset"`)) {
		t.Fatalf("artifact projection=%s", list)
	}
	p.stop(t)
	if err := os.WriteFile(output, []byte("changed source"), 0600); err != nil {
		t.Fatal(err)
	}
	p = start()
	p.request(t, "session/load", map[string]any{"sessionId": session.ID, "cwd": workspace, "mcpServers": []any{}})
	var imageReplayed, artifactReplayed bool
	for _, event := range p.updates {
		value := string(event)
		imageReplayed = imageReplayed || strings.Contains(value, encodedImage)
		artifactReplayed = artifactReplayed || (strings.Contains(value, `"resource_link"`) && strings.Contains(value, "file:"))
		if strings.Contains(value, "deerflow-asset://") || strings.Contains(value, `"asset"`) {
			t.Fatal("internal asset reference leaked into ACP replay")
		}
	}
	if !imageReplayed || !artifactReplayed {
		t.Fatalf("image replay=%v artifact replay=%v", imageReplayed, artifactReplayed)
	}
	if replay := p.request(t, "_deerflow/artifacts/list", map[string]any{"sessionId": session.ID}); !bytes.Equal(replay, list) {
		t.Fatalf("artifact URI changed after restart: before=%s after=%s", list, replay)
	}
	if calls.Load() != 2 {
		t.Fatalf("restart caused model/tool replay: calls=%d", calls.Load())
	}
	p.stop(t)
}
