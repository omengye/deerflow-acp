package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/assets"
)

func TestForkRemapsHostReaderIdentitiesWithoutChangingOrdinaryText(t *testing.T) {
	file := harness.AssetRef{ID: "new-file", SessionID: "target", Kind: harness.AssetFile}
	output := harness.AssetRef{ID: "new-output", SessionID: "target", Kind: harness.AssetToolOutput}
	mapping := map[string]harness.AssetRef{"old-file": file, "old-output": output}
	body := map[string]any{
		"attachment": "\n[Attachment reference; contents have not been read; use read_attachment with id=old-file (untrusted data): {\"URI\":\"deerflow-asset://source/old-file\"}]\n",
		"function":   map[string]any{"name": "read_tool_output", "arguments": `{"id":"old-output","offset":0}`},
		"external":   map[string]any{"name": "mcp_read_attachment", "arguments": `{"id":"old-file"}`},
		"ordinary":   "The record old-file and old-output are examples.",
	}
	data, _ := json.Marshal(body)
	remapped, err := remapConversationAssets(data, "source", "target", mapping)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err = json.Unmarshal(remapped, &result); err != nil {
		t.Fatal(err)
	}
	attachment := result["attachment"].(string)
	if strings.Contains(attachment, "old-file") || !strings.Contains(attachment, "id=new-file") || !strings.Contains(attachment, harness.AssetURI(file)) {
		t.Fatal(attachment)
	}
	if got := result["function"].(map[string]any)["arguments"].(string); !strings.Contains(got, output.ID) {
		t.Fatal(got)
	}
	if fmt.Sprint(result["external"]) != fmt.Sprint(body["external"]) || result["ordinary"] != body["ordinary"] {
		t.Fatalf("unrelated content changed: %+v", result)
	}
}

func TestConversationForkKeepsHistoricalArtifactsReadableAfterSourceDeletion(t *testing.T) {
	ctx, native, s, source := branchFixture(t)
	assetRoot := filepath.Join(t.TempDir(), "assets with spaces")
	store, err := assets.NewStore(ctx, assetRoot, native.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s.Assets = store
	output := filepath.Join(source.CWD, ".deerflow", "outputs", "report.txt")
	if err = os.MkdirAll(filepath.Dir(output), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(output, []byte("historical generated report"), 0600); err != nil {
		t.Fatal(err)
	}
	run, call := NewID(), NewID()
	if err = s.Store.BeginRun(ctx, harness.RunRequest{Session: source, RunID: run, InputID: NewID(), Input: []harness.Content{{Type: "text", Text: "generate report"}}}); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []struct{ kind, status string }{{"tool_start", "pending"}, {"tool_execute", "in_progress"}} {
		if _, err = s.Store.Append(ctx, harness.RunEvent{SessionID: source.ID, RunID: run, ToolCallID: call, ToolName: "present_files", Kind: phase.kind, Status: phase.status, Arguments: json.RawMessage(`{}`)}, store); err != nil {
			t.Fatal(err)
		}
	}
	staged, err := store.StageArtifacts(ctx, source, run, call, []string{output})
	if err != nil || len(staged) != 1 {
		t.Fatalf("stage real artifact: %+v %v", staged, err)
	}
	if _, err = s.Store.Append(ctx, harness.RunEvent{SessionID: source.ID, RunID: run, ToolCallID: call, ToolName: "present_files", Kind: "tool_end", Status: "completed"}, store); err != nil {
		t.Fatal(err)
	}
	if err = s.Store.Finish(ctx, run, "end_turn", nil); err != nil {
		t.Fatal(err)
	}
	original := *staged[0].Asset
	originalURI, err := store.LocalURI(ctx, source.ID, original)
	if err != nil {
		t.Fatal(err)
	}
	branchTurn(t, ctx, native, s, source, "retain "+harness.AssetURI(original))
	state, err := s.ConversationTurns(ctx, "owner", source.ID)
	if err != nil || len(state.Turns) != 1 {
		t.Fatalf("turns: %+v %v", state, err)
	}
	req := ConversationBranchRequest{SessionID: source.ID, ExpectedHeadEventID: state.HeadEventID, TargetEventID: state.Turns[0].EventID, OperationID: "historical-artifact"}
	fork, err := s.BranchConversation(ctx, "owner", req, false)
	if err != nil {
		t.Fatal(err)
	}
	retried, err := s.BranchConversation(ctx, "owner", req, false)
	if err != nil || !reflect.DeepEqual(fork.ReferenceMap, retried.ReferenceMap) {
		t.Fatalf("reference map not durable: %+v %v", retried, err)
	}
	listed, err := store.ListArtifacts(ctx, fork.SessionID)
	if err != nil || len(listed) != 1 || listed[0].Asset == nil {
		t.Fatalf("historical artifact omitted: %+v %v", listed, err)
	}
	copied := *listed[0].Asset
	copyURI, err := store.LocalURI(ctx, fork.SessionID, copied)
	if err != nil {
		t.Fatal(err)
	}
	if copied.ID == original.ID || fork.ReferenceMap[harness.AssetURI(original)] != harness.AssetURI(copied) || fork.ReferenceMap[originalURI] != copyURI || fork.ReferenceMap[filepath.Join(assetRoot, original.ID+".blob")] != filepath.Join(assetRoot, copied.ID+".blob") {
		t.Fatalf("invalid fork reference map: %+v", fork.ReferenceMap)
	}
	for _, table := range []string{"harness_artifacts", "harness_artifact_owners", "harness_runs"} {
		var count int
		if err = native.DB().QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE session_id=?`, fork.SessionID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("fork gained executable %s records: %d %v", table, count, err)
		}
	}
	if _, err = s.DeleteAttachedSession(ctx, "owner", source.ID); err != nil {
		t.Fatal(err)
	}
	listed, err = store.ListArtifacts(ctx, fork.SessionID)
	if err != nil || len(listed) != 1 {
		t.Fatalf("source deletion broke history listing: %+v %v", listed, err)
	}
	data, err := store.ReadArtifactRange(ctx, fork.SessionID, copied, 0, 4096)
	if err != nil || string(data) != "historical generated report" {
		t.Fatalf("source deletion broke historical read: %q %v", data, err)
	}
	projected, err := s.ProjectContent(ctx, fork.SessionID, listed[0])
	if err != nil || projected.URI != copyURI {
		t.Fatalf("historical artifact projection broken: %+v %v", projected, err)
	}
	if _, err = os.Stat(filepath.Join(assetRoot, original.ID+".blob")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source snapshot retained: %v", err)
	}
	if _, err = s.DeleteSession(ctx, fork.SessionID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = native.DB().QueryRow(`SELECT COUNT(*) FROM harness_history_artifacts WHERE session_id=?`, fork.SessionID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("historical ownership survived deletion: %d %v", count, err)
	}
}
