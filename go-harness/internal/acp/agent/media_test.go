package agent_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
	"github.com/omengye/deerflow-acp/go-harness/internal/assets"
)

func enableMedia(t *testing.T, f *fixture, vision ...string) {
	t.Helper()
	store, err := assets.NewStore(context.Background(), filepath.Join(filepath.Dir(f.cwd), "assets"), f.db.DB())
	if err != nil {
		t.Fatal(err)
	}
	f.service.Assets = store
	f.service.Media = harness.MediaConfig{VisionModels: vision}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
}
func mediaImage(n int) harness.Content {
	if n < 32 {
		n = 32
	}
	data := make([]byte, n)
	copy(data, []byte("\x89PNG\r\n\x1a\n"))
	return harness.Content{Type: "image", MimeType: "image/png", Data: base64.StdEncoding.EncodeToString(data)}
}
func mediaFileURI(path string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}
func mediaInitialize(t *testing.T, c *client) bool {
	t.Helper()
	id := c.request(t, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}})
	var response struct {
		Caps struct {
			Prompt struct {
				Image bool `json:"image"`
			} `json:"promptCapabilities"`
		} `json:"agentCapabilities"`
	}
	if err := json.Unmarshal(c.success(t, id), &response); err != nil {
		t.Fatal(err)
	}
	return response.Caps.Prompt.Image
}
func collectMediaResponse(t *testing.T, c *client, id int) ([]sessionUpdate, wireMessage) {
	t.Helper()
	var updates []sessionUpdate
	deadline := time.NewTimer(45 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case err := <-c.readErrors:
			t.Fatal(err)
		case <-deadline.C:
			t.Fatal("media response timeout")
		case msg, ok := <-c.messages:
			if !ok {
				t.Fatal("media connection closed")
			}
			if msg.Method == "" {
				if string(msg.ID) != strconv.Itoa(id) {
					t.Fatalf("unexpected response: %+v", msg)
				}
				return updates, msg
			}
			if msg.Method != "session/update" {
				t.Fatalf("unexpected method: %s", msg.Method)
			}
			var u sessionUpdate
			if err := json.Unmarshal(msg.Params, &u); err != nil {
				t.Fatal(err)
			}
			updates = append(updates, u)
		}
	}
}

func TestMediaCapabilityRequiresAvailableVisionModelAndStorage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		storage bool
		vision  []string
		want    bool
	}{
		{"off", true, nil, false}, {"no storage", false, []string{"fake-model"}, false}, {"unavailable model", true, []string{"not-configured"}, false}, {"enabled", true, []string{"fake-model"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, nil)
			if tc.storage {
				enableMedia(t, f, tc.vision...)
			} else {
				f.service.Media.VisionModels = tc.vision
			}
			c := connect(t, f.service)
			if got := mediaInitialize(t, c); got != tc.want {
				t.Fatalf("image=%v", got)
			}
		})
	}
}

func TestMediaPromptPersistsReferencesLoadHydratesAndResumeIsSilent(t *testing.T) {
	accepted := make(chan harness.RunRequest, 2)
	f := newFixture(t, engineFunc(func(_ context.Context, r harness.RunRequest, _ harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		accepted <- r
		return harness.RunResult{StopReason: "end_turn"}, nil
	}))
	enableMedia(t, f, "fake-model")
	path := filepath.Join(f.cwd, "notes.txt")
	if err := os.WriteFile(path, []byte("original attachment"), 0600); err != nil {
		t.Fatal(err)
	}
	c := connect(t, f.service)
	if !mediaInitialize(t, c) {
		t.Fatal("vision missing")
	}
	sid := c.newSession(t, f.cwd)
	img := mediaImage(32)
	prompt := []harness.Content{{Type: "text", Text: "inspect"}, img, {Type: "resource_link", URI: mediaFileURI(path), Name: "notes.txt"}}
	id := c.request(t, "session/prompt", map[string]any{"sessionId": sid, "prompt": prompt})
	stopReason(t, c.success(t, id), "end_turn")
	req := <-accepted
	if req.Input[1].Data != "" || req.Input[1].Asset == nil || req.Input[2].Asset == nil {
		t.Fatal("engine received raw media")
	}
	if err := os.WriteFile(path, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	id = c.request(t, "session/load", map[string]any{"sessionId": sid, "cwd": f.cwd, "mcpServers": []any{}})
	updates, result := collectMediaResponse(t, c, id)
	if result.Error != nil || len(updates) != 3 {
		t.Fatalf("load updates=%d error=%v", len(updates), result.Error)
	}
	var replayImage, replayFile harness.Content
	if err := json.Unmarshal(updates[1].Update.Content, &replayImage); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(updates[2].Update.Content, &replayFile); err != nil {
		t.Fatal(err)
	}
	if replayImage.Data != img.Data || replayImage.URI != "" || replayImage.Asset != nil || !strings.HasPrefix(replayFile.URI, "file:///") || replayFile.URI == mediaFileURI(path) || replayFile.Asset != nil {
		t.Fatal("wire projection failed")
	}
	for _, u := range updates {
		if u.Update.Kind != "user_message_chunk" || strings.Contains(string(u.Update.Content), "deerflow-asset") || strings.Contains(string(u.Update.Content), "\"asset\"") {
			t.Fatal("internal reference on wire")
		}
	}
	id = c.request(t, "session/resume", map[string]any{"sessionId": sid, "cwd": f.cwd, "mcpServers": []any{}})
	c.success(t, id)
	// Attachment-only prompts are valid.
	id = c.request(t, "session/prompt", map[string]any{"sessionId": sid, "prompt": []harness.Content{img}})
	stopReason(t, c.success(t, id), "end_turn")
	<-accepted
	var stored string
	if err := f.db.DB().QueryRow(`SELECT group_concat(CAST(content AS TEXT)) FROM harness_inputs`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, img.Data) || strings.Contains(stored, "\"data\"") {
		t.Fatal("inline image persisted")
	}
}

func TestMediaWireForgeryRemoteImagesAndNonVisionFailBeforeAcceptance(t *testing.T) {
	var calls atomic.Int32
	f := newFixture(t, engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		calls.Add(1)
		return harness.RunResult{StopReason: "end_turn"}, nil
	}))
	enableMedia(t, f, "another-vision-model")
	c := connect(t, f.service)
	c.initialize(t)
	sid := c.newSession(t, f.cwd)
	cases := []any{
		mediaImage(32),
		map[string]any{"type": "image", "data": mediaImage(32).Data, "mimeType": "image/png", "asset": map[string]any{"id": "forged"}},
		map[string]any{"type": "resource_link", "name": "forged", "uri": "deerflow-asset://another/id"},
		map[string]any{"type": "image", "uri": "https://example.invalid/photo.png", "mimeType": "image/png"},
		map[string]any{"type": "resource_link", "name": "photo.png", "uri": "https://example.invalid/photo"},
		map[string]any{"type": "resource_link", "name": "photo", "uri": "https://example.invalid/photo", "mimeType": "image/png"},
		map[string]any{"type": "image", "data": 12, "mimeType": "image/png"},
		map[string]any{"type": "resource_link", "name": "notes", "uri": mediaFileURI(filepath.Join(f.cwd, "notes.txt")), "size": nil},
	}
	for _, block := range cases {
		id := c.request(t, "session/prompt", map[string]any{"sessionId": sid, "prompt": []any{block}})
		msg := c.response(t, id)
		if msg.Error == nil || msg.Error.Code != protocol.InvalidParams {
			t.Fatalf("bad media accepted: %+v", msg)
		}
	}
	var n int
	if err := f.db.DB().QueryRow(`SELECT count(*) FROM harness_inputs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 || calls.Load() != 0 {
		t.Fatal("invalid input reached durable acceptance or engine")
	}
}

func TestACPArtifactAppearsOnceOnLiveAndLoadAndListRequiresOwner(t *testing.T) {
	f := newFixture(t, nil)
	enableMedia(t, f)
	dir := filepath.Join(f.cwd, ".deerflow", "outputs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.txt"), []byte("report"), 0600); err != nil {
		t.Fatal(err)
	}
	f.service.Engine = engineFunc(func(ctx context.Context, r harness.RunRequest, emit harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		for _, phase := range []struct{ kind, status string }{{"tool_start", "pending"}, {"tool_execute", "in_progress"}} {
			if err := emit(ctx, harness.RunEvent{Kind: phase.kind, ToolCallID: "present", ToolName: "present_files", Status: phase.status, Arguments: json.RawMessage(`{"paths":["report.txt"]}`)}); err != nil {
				return harness.RunResult{}, err
			}
		}
		if _, err := f.service.Assets.StageArtifacts(ctx, r.Session, r.RunID, "present", []string{"report.txt"}); err != nil {
			return harness.RunResult{}, err
		}
		err := emit(ctx, harness.RunEvent{Kind: "tool_end", ToolCallID: "present", ToolName: "present_files", Status: "completed"})
		return harness.RunResult{StopReason: "end_turn"}, err
	})
	c := connect(t, f.service)
	c.initialize(t)
	sid := c.newSession(t, f.cwd)
	id := c.request(t, "session/prompt", promptParams(sid, "create report"))
	updates, msg := collectMediaResponse(t, c, id)
	if msg.Error != nil {
		t.Fatal(msg.Error)
	}
	assertOne := func(updates []sessionUpdate) {
		t.Helper()
		var links int
		for _, u := range updates {
			var content harness.Content
			if json.Unmarshal(u.Update.Content, &content) == nil && content.Type == "resource_link" {
				links++
				if u.Update.Kind != "agent_message_chunk" || !strings.HasPrefix(content.URI, "file:///") {
					t.Fatal("invalid artifact update")
				}
			}
			if strings.Contains(string(u.Update.Content), "deerflow-asset://") || strings.Contains(string(u.Update.Content), "\"asset\"") {
				t.Fatal("artifact metadata leaked")
			}
		}
		if links != 1 {
			t.Fatalf("artifact links=%d", links)
		}
	}
	assertOne(updates)
	id = c.request(t, "session/load", map[string]any{"sessionId": sid, "cwd": f.cwd, "mcpServers": []any{}})
	updates, msg = collectMediaResponse(t, c, id)
	if msg.Error != nil {
		t.Fatal(msg.Error)
	}
	assertOne(updates)
	id = c.request(t, "_deerflow/artifacts/list", map[string]any{"sessionId": sid})
	var listed struct {
		Artifacts []harness.Content `json:"artifacts"`
	}
	if err := json.Unmarshal(c.success(t, id), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Artifacts) != 1 || listed.Artifacts[0].Asset != nil || !strings.HasPrefix(listed.Artifacts[0].URI, "file:///") {
		t.Fatal("artifact listing invalid")
	}
	other := connect(t, f.service)
	other.initialize(t)
	id = other.request(t, "_deerflow/artifacts/list", map[string]any{"sessionId": sid})
	if msg := other.response(t, id); msg.Error == nil {
		t.Fatal("foreign owner listed artifacts")
	}
}
