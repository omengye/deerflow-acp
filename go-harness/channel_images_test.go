package deerflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/channelproto"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/automation"
	"github.com/omengye/deerflow-acp/go-harness/internal/testutil"
)

func TestChannelImageCollectionUsesOnlyCurrentCommittedOutputs(t *testing.T) {
	ref := harness.AssetRef{ID: "image", SessionID: "session", SHA256: "image-digest", MimeType: "image/png", Kind: harness.AssetImage}
	base := harness.RunEvent{SessionID: "session", Kind: "image_delta", Content: []harness.Content{{Type: "image", Asset: &ref}}}
	for _, mode := range []string{"model", "tool", "present_files", "view_image", "failed", "uncommitted", "foreign", "text_path"} {
		t.Run(mode, func(t *testing.T) {
			copyRef, event := ref, base
			event.Content = []harness.Content{{Type: "image", Asset: &copyRef}}
			want := 1
			switch mode {
			case "tool":
				event.Kind, event.ToolName, event.Status = "tool_end", "image_generator", "completed"
			case "present_files":
				event.Kind, event.ToolName, event.Status = "tool_end", "present_files", "completed"
				event.Content[0].Type, copyRef.Kind = "resource_link", harness.AssetArtifact
			case "view_image":
				event.Kind, event.ToolName, event.Status = "tool_end", "view_image", "completed"
				want = 0
			case "failed":
				event.Kind, event.Status = "tool_end", "failed"
				want = 0
			case "uncommitted":
				event.Content[0].Asset = nil
				event.Content[0].Data = "inline"
				want = 0
			case "foreign":
				copyRef.SessionID = "another-session"
				want = 0
			case "text_path":
				event.Kind, event.Text = "text_delta", "![](C:/private/photo.png)"
				want = 0
			}
			collector := channelImages{session: "session"}
			collector.observe(event)
			collector.observe(event)
			var out automation.Outcome
			collector.finish(&out)
			if len(out.Images) != want {
				t.Fatal("missing image or duplicate/leak", out.Images)
			}
		})
	}
	collector := channelImages{session: "session"}
	for n := 0; n < channelproto.MaxReplyImages+2; n++ {
		copyRef := ref
		copyRef.ID, copyRef.SHA256 = fmt.Sprint(n), fmt.Sprint(n)
		e := base
		e.Content = []harness.Content{{Type: "image", Asset: &copyRef}}
		collector.observe(e)
	}
	var out automation.Outcome
	collector.finish(&out)
	if len(out.Images) != channelproto.MaxReplyImages || !strings.Contains(out.Text, "数量超过") {
		t.Fatal("unbounded or silently truncated images")
	}
}

func TestChannelGeneratedImageThroughRuntimeAndStdioPlugin(t *testing.T) {
	dir, workspace := t.TempDir(), testutil.CanonicalTempDir(t)
	data, _ := base64.StdEncoding.DecodeString(imageFixture(t))
	// Bigger than a stdio frame, while still a valid PNG byte stream.
	data = append(data, bytes.Repeat([]byte{17}, channelproto.MaxFrame+1024)...)
	var calls atomic.Int32
	var c *Client
	cfg := Config{DataDir: t.TempDir(), Engine: engineFunc(func(ctx context.Context, r harness.RunRequest, emit harness.EventHandler, _ harness.PermissionHandler) (harness.RunResult, error) {
		if calls.Add(1) == 1 {
			content, err := c.assets.StageModelImage(ctx, r.Session, r.RunID, harness.Content{Type: "image", MimeType: "image/png", Data: base64.StdEncoding.EncodeToString(data)})
			if err != nil {
				return harness.RunResult{}, err
			}
			if err = emit(ctx, harness.RunEvent{Kind: "image_delta", Content: []harness.Content{content}}); err != nil {
				return harness.RunResult{}, err
			}
			return harness.RunResult{StopReason: "end_turn"}, nil
		}
		return harness.RunResult{StopReason: "end_turn"}, emit(ctx, harness.RunEvent{Kind: "text_delta", Text: "second answer"})
	})}
	var err error
	c, err = Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	setupNativeFeedbackFixture(t, c, workspace, dir, "summary")
	putFeedbackMessage(t, dir, 1, "draw a picture")
	body := waitFeedbackFile(t, dir, "image-1-*.json", func([]byte) bool { return true })
	var sent channelproto.Send
	if json.Unmarshal(body, &sent) != nil || sent.Image == nil || sent.Text != "" || sent.Image.Size != int64(len(data)) || sent.Image.SHA256 != fmt.Sprintf("%x", sha256.Sum256(data)) {
		t.Fatal("image output did not reach plugin")
	}
	if len(body) >= channelproto.MaxFrame {
		t.Fatal("image data entered protocol")
	}
	putFeedbackMessage(t, dir, 2, "hello")
	if reply := feedbackReply(t, dir, 2); reply.Text != "second answer" {
		t.Fatal(reply.Text)
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "image-*.json"))
	if len(paths) != 1 || calls.Load() != 2 {
		t.Fatal("old image resent on next turn", len(paths), calls.Load())
	}
	var raw []byte
	if err := c.store.DB().QueryRow(`SELECT asset FROM harness_channel_images WHERE id='1'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var ref harness.AssetRef
	if err := json.Unmarshal(raw, &ref); err != nil {
		t.Fatal(err)
	}
	if _, err := c.channelImage(t.Context(), ref.SessionID, t.TempDir(), ref); err == nil {
		t.Fatal("cross-workspace image was authorized")
	}
	if _, err := c.channelImage(t.Context(), "another-session", workspace, ref); err == nil {
		t.Fatal("cross-session image was authorized")
	}
	if err := os.WriteFile(sent.Image.Path, []byte("mutated"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.channelImage(t.Context(), ref.SessionID, workspace, ref); err == nil {
		t.Fatal("modified snapshot was authorized")
	}
}

func TestChannelPresentFilesImageIncludingPermissionResume(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(fmt.Sprintf("resume=%v", resume), func(t *testing.T) {
			dir, workspace := t.TempDir(), testutil.CanonicalTempDir(t)
			data, _ := base64.StdEncoding.DecodeString(imageFixture(t))
			path := filepath.Join(workspace, ".deerflow", "outputs", "drawing.png")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var req struct {
					Stream   bool
					Messages []struct{ Role string }
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				for _, msg := range req.Messages {
					if msg.Role == "tool" {
						// The host must send the published snapshot, not this mutable source.
						if err := os.WriteFile(path, []byte("modified source"), 0600); err != nil {
							t.Error(err)
						}
						skillToolResponse(w, req.Stream, "", "", "drawing ready")
						return
					}
				}
				skillToolResponse(w, req.Stream, "present_files", `{"paths":[".deerflow/outputs/drawing.png"]}`, "")
			}))
			defer provider.Close()
			cfg := Config{DataDir: t.TempDir(), Provider: "openai", Model: "fixture", APIKey: "fixture", BaseURL: provider.URL + "/v1", DisableSubagents: true, PermissionMode: harness.PermissionModeOff}
			if resume {
				cfg.PermissionMode = harness.PermissionModeAll
			}
			c, err := Open(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			setupNativeFeedbackFixture(t, c, workspace, dir, "summary")
			putFeedbackMessage(t, dir, 1, "present the drawing")
			n := 1
			if resume {
				first := feedbackReply(t, dir, 1)
				if !strings.Contains(first.Text, "waiting_input") {
					t.Fatal("missing permission wait", first.Text)
				}
				status, err := c.channels.Status(t.Context(), "native-test")
				if err != nil {
					t.Fatal(err)
				}
				revision := status.(map[string]any)["revision"].(int64)
				// The fixture writes before the host records the send ACK and joins
				// optional feedback. Wait for that lifecycle before changing policy.
				deadline := time.NewTimer(5 * time.Second)
				ticker := time.NewTicker(25 * time.Millisecond)
				defer deadline.Stop()
				defer ticker.Stop()
				for {
					if _, err = c.channels.SetPermissions(t.Context(), "native-test", revision, harness.ApprovalAllowAlways); err == nil {
						break
					}
					if !strings.Contains(err.Error(), "任务正在运行") && !strings.Contains(err.Error(), "正在处理消息") {
						t.Fatal(err)
					}
					select {
					case <-deadline.C:
						t.Fatal(err)
					case <-ticker.C:
					}
				}
				n = 2
				putFeedbackMessage(t, dir, n, "/resume")
			}
			if reply := feedbackReply(t, dir, n); !strings.Contains(reply.Text, "drawing ready") {
				t.Fatal("image run did not complete", reply.Text, calls.Load())
			}
			body := waitFeedbackFile(t, dir, fmt.Sprintf("image-%d-*.json", n), func([]byte) bool { return true })
			var sent channelproto.Send
			if json.Unmarshal(body, &sent) != nil || sent.Image == nil || sent.Image.SHA256 != fmt.Sprintf("%x", sha256.Sum256(data)) {
				t.Fatal("present_files snapshot missing")
			}
			if calls.Load() != 2 {
				t.Fatal("unexpected model replay", calls.Load())
			}
		})
	}
}
