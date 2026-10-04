package deerflow

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
)

func TestSkillFeedbackSDKAndAuthenticatedManagement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	workspace, cfg := skillFixture(t)
	var modelCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "fixture request", 400)
			return
		}
		call := modelCalls.Add(1)
		message := map[string]any{"role": "assistant", "content": "The source has been checked."}
		finish := "stop"
		if call <= 2 {
			name, args := "skill", `{"skill":"research"}`
			if call == 2 {
				name, args = "read_skill_file", `{"skill":"research","path":"guide.txt"}`
			}
			message["content"] = nil
			message["tool_calls"] = []any{map[string]any{"index": 0, "id": fmt.Sprintf("read-%d", call), "type": "function", "function": map[string]string{"name": name, "arguments": args}}}
			finish = "tool_calls"
		}
		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			body, _ := json.Marshal(map[string]any{"id": "fixture", "choices": []any{map[string]any{"index": 0, "delta": message, "finish_reason": finish}}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", body)
		} else {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "fixture", "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}})
		}
	}))
	defer provider.Close()
	cfg.Provider, cfg.Model, cfg.BaseURL, cfg.APIKey = "openai", "fixture", provider.URL+"/v1", "fixture"
	cfg.Skills.Install = []harness.SkillInstall{{SourceID: "project", Directory: "research", Enabled: true}}
	c, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	session, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.SetConfigOption(ctx, session.ID, "approval", harness.ApprovalReadOnly); err != nil {
		t.Fatal(err)
	}
	firstRun, err := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "Read research and its reference."}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	signal := waitSkillSignal(t, ctx, c, 2)
	evidence, err := c.GetSkillSignalEvidence(ctx, signal.ID, "", 10)
	if err != nil || len(evidence.Reads) != 2 || signal.RootID != firstRun.Execution.RunID || modelCalls.Load() != 3 {
		t.Fatalf("real skill evidence: %+v calls=%d %v", evidence, modelCalls.Load(), err)
	}
	snapshot, err := c.GetSkillFeedback(ctx, signal.ID)
	if err != nil || snapshot.Signal.ID != signal.ID || snapshot.Feedback != nil {
		t.Fatalf("initial feedback: %+v %v", snapshot, err)
	}
	request := harness.SkillFeedbackRequest{
		SignalID: signal.ID, ExpectedSignalStateVersion: signal.StateVersion,
		ExpectedFeedbackVersion: 0, RequestKey: "operator-correction", Kind: "correction",
		Summary: "Verify the source date before summarizing.", ReadIDs: []int64{evidence.Reads[0].ID},
	}
	cancelled, stop := context.WithCancel(ctx)
	stop()
	if _, err = c.SubmitSkillFeedback(cancelled, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled submit: %v", err)
	}
	if _, err = c.GetSkillFeedback(cancelled, signal.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled get: %v", err)
	}
	if _, err = c.ListSkillFeedback(cancelled, signal.ID, "", 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled list: %v", err)
	}
	submitted, err := c.SubmitSkillFeedback(ctx, request)
	if err != nil || submitted.Replayed || submitted.CurrentRevision != 1 || submitted.Feedback.Revision != 1 || submitted.Feedback.Origin != "operator" || submitted.Feedback.Target != signal.Target || submitted.Feedback.RootSessionID != session.ID || submitted.Feedback.SkillVersion != signal.Version {
		t.Fatalf("submit: %+v %v", submitted, err)
	}
	// Reuse the ordinary SDK fixture and authenticate the real DFACP/1 control
	// channel. No direct table inserts stand in for skill reads or feedback.
	host, err := localhost.New(localhost.Config{RuntimeDir: t.TempDir(), Manage: c.ManageLocal, ServeACP: func(context.Context, io.ReadCloser, io.WriteCloser) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	ep, err := host.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	wire := func(fields map[string]any, expectedCode string, out any) {
		t.Helper()
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(ep.Host, fmt.Sprint(ep.Port)), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err = fmt.Fprintf(conn, "DFACP/1 %s MANAGE\n", ep.Token); err != nil {
			t.Fatal(err)
		}
		reader := bufio.NewReader(conn)
		if line, err := reader.ReadString('\n'); err != nil || strings.TrimSpace(line) != "OK" {
			t.Fatalf("handshake: %q %v", line, err)
		}
		if err = json.NewEncoder(conn).Encode(fields); err != nil {
			t.Fatal(err)
		}
		var response struct {
			OK   bool            `json:"ok"`
			Code string          `json:"code"`
			Data json.RawMessage `json:"data"`
		}
		if err = json.NewDecoder(reader).Decode(&response); err != nil {
			t.Fatal(err)
		}
		if response.OK != (expectedCode == "") || response.Code != expectedCode {
			t.Fatalf("MANAGE %v: %+v", fields, response)
		}
		if out != nil {
			if err = json.Unmarshal(response.Data, out); err != nil {
				t.Fatal(err)
			}
		}
	}
	fields := map[string]any{
		"operation": "skill.feedback.submit", "signal_id": request.SignalID,
		"expected_signal_state_version": request.ExpectedSignalStateVersion,
		"expected_feedback_version":     request.ExpectedFeedbackVersion,
		"request_key":                   request.RequestKey, "kind": request.Kind, "summary": request.Summary, "read_ids": request.ReadIDs,
	}
	var replay harness.SkillFeedbackResult
	wire(fields, "", &replay)
	if !replay.Replayed || replay.CurrentRevision != 1 || replay.Feedback.Revision != 1 || replay.Feedback.Origin != submitted.Feedback.Origin {
		t.Fatalf("SDK/MANAGE replay changed feedback: %+v", replay)
	}
	wire(map[string]any{"operation": "skill.feedback.get", "signal_id": signal.ID}, "", &snapshot)
	if snapshot.Feedback == nil || snapshot.Feedback.Revision != 1 || !snapshot.EvidenceCurrent || snapshot.Signal.Attribution != "unassessed" {
		t.Fatalf("feedback was mistaken for causal attribution: %+v", snapshot)
	}
	for _, key := range []string{"signal_id", "expected_signal_state_version", "expected_feedback_version", "request_key", "kind", "summary", "read_ids"} {
		bad := maps.Clone(fields)
		delete(bad, key)
		wire(bad, "invalid_request", nil)
		bad[key] = nil
		wire(bad, "invalid_request", nil)
	}
	for _, key := range []string{"actor", "origin", "target", "source_id", "source_identity", "root_id", "root_session_id", "skill_version"} {
		bad := maps.Clone(fields)
		bad[key] = "client-supplied-identity"
		wire(bad, "invalid_request", nil)
	}
	for _, invalid := range []struct {
		key   string
		value any
	}{
		{"expected_signal_state_version", 0},
		{"expected_signal_state_version", 1.5},
		{"expected_feedback_version", -1},
		{"expected_feedback_version", 0.5},
		{"request_key", 123},
		{"kind", "guaranteed_skill_failure"},
		{"summary", strings.Repeat("字", 667)},
		{"summary", []string{"not a string"}},
		{"read_ids", []any{nil}},
		{"read_ids", []any{1.5}},
		{"read_ids", []int64{0}},
		{"read_ids", make([]int64, 17)},
	} {
		bad := maps.Clone(fields)
		bad[invalid.key] = invalid.value
		wire(bad, "invalid_request", nil)
	}
	for _, operation := range []string{"skill.feedback.get", "skill.feedback.list"} {
		wire(map[string]any{"operation": operation}, "invalid_request", nil)
		wire(map[string]any{"operation": operation, "signal_id": nil}, "invalid_request", nil)
		wire(map[string]any{"operation": operation, "signal_id": signal.ID, "actor": "forged"}, "invalid_request", nil)
	}
	for _, limit := range []any{nil, 0, -1, 101, 1.5} {
		wire(map[string]any{"operation": "skill.feedback.list", "signal_id": signal.ID, "limit": limit}, "invalid_request", nil)
	}
	for _, cursor := range []any{nil, 1} {
		wire(map[string]any{"operation": "skill.feedback.list", "signal_id": signal.ID, "cursor": cursor}, "invalid_request", nil)
	}
	for _, limit := range []int{-1, 0, 101} {
		if _, err = c.ListSkillFeedback(ctx, signal.ID, "", limit); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatalf("SDK accepted invalid history limit %d: %v", limit, err)
		}
	}
	stale := maps.Clone(fields)
	stale["request_key"] = "stale-operator-correction"
	wire(stale, "conflict", nil)
	changedReplay := maps.Clone(fields)
	changedReplay["summary"] = "A changed request cannot borrow an earlier request key."
	wire(changedReplay, "conflict", nil)
	foreignRead := request
	foreignRead.RequestKey, foreignRead.ExpectedFeedbackVersion = "foreign-read", 1
	foreignRead.ReadIDs = []int64{1 << 62}
	if _, err = c.SubmitSkillFeedback(ctx, foreignRead); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatalf("unobserved read identity: %v", err)
	}
	// Replace the current assessment; external network failure is not an
	// additional vote against skill quality.
	replacement := maps.Clone(fields)
	replacement["expected_feedback_version"], replacement["request_key"], replacement["kind"] = 1, "network-correction", "network"
	replacement["summary"] = "The upstream endpoint could not be reached."
	var revised harness.SkillFeedbackResult
	wire(replacement, "", &revised)
	if revised.CurrentRevision != 2 || revised.Feedback.Kind != "network" || revised.Replayed {
		t.Fatalf("revision: %+v", revised)
	}
	withdrawal := maps.Clone(fields)
	withdrawal["expected_feedback_version"], withdrawal["request_key"], withdrawal["kind"] = 2, "withdraw-assessment", "withdrawn"
	withdrawal["summary"], withdrawal["read_ids"] = "", []int64{}
	wire(withdrawal, "", &revised)
	if revised.CurrentRevision != 3 || revised.Feedback.Kind != "withdrawn" || len(revised.Feedback.ReadIDs) != 0 {
		t.Fatalf("withdrawal: %+v", revised)
	}
	wire(withdrawal, "", &replay)
	if !replay.Replayed || replay.CurrentRevision != 3 || replay.Feedback.Revision != 3 {
		t.Fatalf("withdrawal replay: %+v", replay)
	}
	wire(fields, "", &replay)
	if !replay.Replayed || replay.Feedback.Revision != 1 || replay.CurrentRevision != 3 {
		t.Fatalf("old request lost its receipt after withdrawal: %+v", replay)
	}
	snapshot, err = c.GetSkillFeedback(ctx, signal.ID)
	if err != nil || snapshot.Feedback == nil || snapshot.Feedback.Revision != 3 || snapshot.Feedback.Kind != "withdrawn" || snapshot.EvidenceCurrent {
		t.Fatalf("old replay restored a withdrawn assessment: %+v %v", snapshot, err)
	}
	var page harness.SkillFeedbackPage
	wire(map[string]any{"operation": "skill.feedback.list", "signal_id": signal.ID, "limit": 1}, "", &page)
	if len(page.Revisions) != 1 || page.CurrentRevision != 3 || page.Next == "" {
		t.Fatalf("history page: %+v", page)
	}
	seen := map[int64]bool{page.Revisions[0].Revision: true}
	for page.Next != "" {
		page, err = c.ListSkillFeedback(ctx, signal.ID, page.Next, 1)
		if err != nil || len(page.Revisions) != 1 || page.CurrentRevision != 3 || seen[page.Revisions[0].Revision] {
			t.Fatalf("history pagination: %+v %v", page, err)
		}
		seen[page.Revisions[0].Revision] = true
	}
	if len(seen) != 3 || modelCalls.Load() != 3 {
		t.Fatalf("feedback changed user work: revisions=%v calls=%d", seen, modelCalls.Load())
	}
	if _, err = c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "Continue the same conversation."}}, nil, nil); err != nil || modelCalls.Load() != 4 {
		t.Fatalf("feedback rejection/withdrawal broke next prompt: calls=%d %v", modelCalls.Load(), err)
	}
	if err = host.Close(); err != nil {
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = c.SubmitSkillFeedback(ctx, request); err == nil {
		t.Fatal("closed SDK accepted feedback")
	}
	if _, err = c.GetSkillFeedback(ctx, signal.ID); err == nil {
		t.Fatal("closed SDK read feedback")
	}
	if _, err = c.ListSkillFeedback(ctx, signal.ID, "", 10); err == nil {
		t.Fatal("closed SDK listed feedback")
	}
	reopened, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	snapshot, err = reopened.GetSkillFeedback(ctx, signal.ID)
	if err != nil || snapshot.Feedback == nil || snapshot.Feedback.Revision != 3 || snapshot.Feedback.Kind != "withdrawn" || modelCalls.Load() != 4 {
		t.Fatalf("restart lost or replayed feedback: %+v calls=%d %v", snapshot, modelCalls.Load(), err)
	}
}
