package deerflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// Runs the real SDK extension, Eino role, tool receipts and runtime inventory.
// Publication before a checkpoint and after resume stays with one logical role.
func TestSDKForegroundArtifactAttributionCheckpointAndPaging(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string
			Stream   bool
			Messages []struct{ Role string }
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			http.Error(w, "invalid", 400)
			return
		}
		count := 0
		for _, m := range req.Messages {
			if m.Role == "tool" {
				count++
			}
		}
		name, args, id := "", "", ""
		if req.Model == "root" && count == 0 {
			name, args, id = "task", `{"subagent_type":"writer","prompt":"publish output snapshots","description":"writer"}`, "delegate"
		}
		if req.Model == "writer" && count < 3 {
			paths := []string{}
			for i := count * 16; i < (count+1)*16 && i < 33; i++ {
				paths = append(paths, fmt.Sprintf("result-%02d.txt", i))
			}
			data, _ := json.Marshal(map[string]any{"paths": paths})
			name, args, id = "present_files", string(data), fmt.Sprintf("publish-%d", count)
		}
		message := map[string]any{"role": "assistant", "content": "published"}
		finish := "stop"
		if name != "" {
			message["content"] = nil
			message["tool_calls"] = []any{map[string]any{"index": 0, "id": id, "type": "function", "function": map[string]string{"name": name, "arguments": args}}}
			finish = "tool_calls"
		}
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			data, _ := json.Marshal(map[string]any{"id": "fixture", "choices": []any{map[string]any{"index": 0, "delta": message, "finish_reason": finish}}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
		} else {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "fixture", "object": "chat.completion", "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}})
		}
	}))
	defer provider.Close()
	cfg := Config{DataDir: t.TempDir(), Provider: "openai", Model: "root", APIKey: "fixture", BaseURL: provider.URL + "/v1", PermissionMode: harness.PermissionModeAll,
		ModelRoutes: map[string]harness.ModelRoute{"writer": {Provider: "openai", Model: "writer", BaseURL: provider.URL + "/v1", APIKey: "fixture"}},
		Subagents:   &harness.SubagentsConfig{CustomAgents: map[string]harness.AgentConfig{"writer": {Description: "writer", SystemPrompt: "Publish outputs", Model: "writer", Tools: []string{"present_files"}}}},
	}
	c, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if c != nil {
			_ = c.Close()
		}
	}()
	workspace := t.TempDir()
	dir := filepath.Join(workspace, ".deerflow", "outputs")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 33; i++ {
		if err = os.WriteFile(filepath.Join(dir, fmt.Sprintf("result-%02d.txt", i)), []byte(fmt.Sprintf("immutable output %02d", i)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	session, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	paused, err := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "delegate output publication"}}, nil, func(_ context.Context, p harness.PermissionRequest) (harness.PermissionDecision, error) {
		if p.ToolName == "present_files" && p.ToolCallID == "publish-2" {
			return harness.PermissionCancelled, nil
		}
		return harness.AllowOnce, nil
	})
	if err != nil || paused.Execution == nil || paused.Execution.Status != harness.ExecutionWaitingInput {
		t.Fatalf("pause=%+v execution=%+v err=%v", paused, paused.Execution, err)
	}
	root := paused.Execution.RunID
	readNodes := func() (harness.TaskTreeNode, harness.TaskTreeNode) {
		page, e := c.TaskTree(ctx, session.ID, harness.TaskTreeQuery{RootID: root, Limit: 100})
		if e != nil {
			t.Fatal(e)
		}
		var lead, role harness.TaskTreeNode
		for _, n := range page.Nodes {
			if n.Mode == "root" {
				lead = n
			}
			if n.Role == "writer" {
				role = n
			}
		}
		return lead, role
	}
	lead, role := readNodes()
	if role.ID == "" || len(role.Artifacts) != 32 || len(lead.Artifacts) != 0 {
		t.Fatalf("checkpoint attribution root=%+v role=%+v", lead, role)
	}
	query := harness.TaskArtifactQuery{RootID: root, NodeID: role.ID, Limit: 32}
	checkpoint, err := c.TaskArtifacts(ctx, session.ID, query)
	if err != nil || len(checkpoint.Artifacts) != 32 || checkpoint.HasMore {
		t.Fatalf("checkpoint page=%+v %v", checkpoint, err)
	}
	oldID := checkpoint.Artifacts[0].ID
	read := harness.TaskArtifactReadRequest{RootID: root, NodeID: role.ID, ArtifactID: oldID, Limit: 5}
	if _, err = c.ReadTaskArtifact(ctx, session.ID, read); err != nil {
		t.Fatal(err)
	}
	read.NodeID = lead.ID
	if _, err = c.ReadTaskArtifact(ctx, session.ID, read); err == nil {
		t.Fatal("role artifact leaked into root")
	}
	read.NodeID = role.ID
	completed, err := c.ResumeExecution(ctx, session.ID, harness.ResumeExecutionRequest{RunID: root, ExpectedVersion: paused.Execution.Version}, nil, func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
		return harness.AllowOnce, nil
	})
	if err != nil || completed.Execution == nil || completed.Execution.Status != harness.ExecutionCompleted {
		t.Fatalf("resume=%+v execution=%+v err=%v", completed, completed.Execution, err)
	}
	var attempts, owners, joined int
	err = c.store.DB().QueryRowContext(ctx, `SELECT COUNT(*),COUNT(DISTINCT o.role_attempt_id),COUNT(a.id) FROM harness_artifact_owners o JOIN harness_agent_attempts a ON a.id=o.role_attempt_id AND a.invocation_id=o.invocation_id WHERE o.root_id=? AND o.node_id=?`, root, role.ID).Scan(&owners, &attempts, &joined)
	if err != nil || owners != 33 || joined != 33 || attempts < 2 {
		t.Fatalf("persisted owners=%d attempts=%d joined=%d err=%v", owners, attempts, joined, err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	c = nil
	c, err = Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.LoadSession(ctx, session.ID, workspace, false, nil); err != nil {
		t.Fatal(err)
	}
	page, err := c.TaskArtifacts(ctx, session.ID, query)
	if err != nil || len(page.Artifacts) != 32 || !page.HasMore || page.NextCursor == "" {
		t.Fatalf("first page=%+v %v", page, err)
	}
	query.Cursor = page.NextCursor
	last, err := c.TaskArtifacts(ctx, session.ID, query)
	if err != nil || len(last.Artifacts) != 1 || last.HasMore || last.NextCursor != "" || last.Artifacts[0].ID <= page.Artifacts[31].ID {
		t.Fatalf("last page=%+v %v", last, err)
	}
	query.NodeID = lead.ID
	if _, err = c.TaskArtifacts(ctx, session.ID, query); err == nil {
		t.Fatal("cursor rebound to a different node")
	}
	// A source edit cannot change the published snapshot or the chunk hash.
	meta := last.Artifacts[0]
	expected, err := os.ReadFile(filepath.Join(dir, meta.Name))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, meta.Name), []byte("edited later"), 0600); err != nil {
		t.Fatal(err)
	}
	read.ArtifactID = meta.ID
	read.Offset = 0
	read.ExpectedSHA256 = ""
	var data []byte
	for {
		chunk, e := c.ReadTaskArtifact(ctx, session.ID, read)
		if e != nil {
			t.Fatal(e)
		}
		data = append(data, chunk.Data...)
		read.Offset = chunk.NextOffset
		read.ExpectedSHA256 = chunk.SHA256
		if chunk.EOF {
			break
		}
	}
	sum := sha256.Sum256(expected)
	if !bytes.Equal(data, expected) || read.ExpectedSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("snapshot changed: %q", data)
	}
	read.ArtifactID = oldID
	read.Offset = 0
	read.ExpectedSHA256 = ""
	if _, err = c.ReadTaskArtifact(ctx, session.ID, read); err != nil {
		t.Fatalf("historical attempt unreadable: %v", err)
	}
}
