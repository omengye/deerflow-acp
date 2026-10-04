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
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
)

// The ordinary skill fixture supplies real runtime storage and skill sources.
// The provider never reads a skill. It delegates only the explicit private-
// session fixture task, so reports cannot manufacture an access signal.
func missingSkillFixture(t *testing.T, background bool) (string, Config, *atomic.Int32) {
	t.Helper()
	workspace, cfg := skillFixture(t)
	calls := new(atomic.Int32)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Stream   bool `json:"stream"`
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "fixture request", 400)
			return
		}
		calls.Add(1)
		delegate, hasTool := false, false
		for _, m := range request.Messages {
			delegate = delegate || m.Role == "user" && strings.Contains(string(m.Content), "CREATE_PRIVATE_TASK")
			hasTool = hasTool || m.Role == "tool"
		}
		message := map[string]any{"role": "assistant", "content": "The task is complete without reading a skill."}
		finish := "stop"
		if background && delegate && !hasTool {
			message["content"] = nil
			message["tool_calls"] = []any{map[string]any{"index": 0, "id": "private-child", "type": "function", "function": map[string]string{"name": "background_agent", "arguments": `{"instruction":"Complete this bounded child task without tools.","description":"private session fixture"}`}}}
			finish = "tool_calls"
		}
		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			body, _ := json.Marshal(map[string]any{"id": "fixture", "choices": []any{map[string]any{"index": 0, "delta": message, "finish_reason": finish}}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", body)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "fixture", "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}}})
	}))
	t.Cleanup(provider.Close)
	cfg.Provider, cfg.Model, cfg.BaseURL, cfg.APIKey = "openai", "fixture", provider.URL+"/v1", "fixture"
	cfg.DisableSubagents, cfg.PermissionMode = !background, harness.PermissionModeOff
	cfg.BackgroundWorkers = 1
	cfg.Budget = &harness.BudgetLimits{MaxModelCalls: 8, MaxToolCalls: 4, MaxTokens: 100000, MaxOutputTokens: 128}
	cfg.Skills.Install = []harness.SkillInstall{{SourceID: "project", Directory: "research", Enabled: true}}
	return workspace, cfg, calls
}

type missingManagement struct {
	t    *testing.T
	host *localhost.Host
	ep   localhost.Endpoint
}

func newMissingManagement(t *testing.T, ctx context.Context, c *Client, manage func(context.Context, localhost.ManagementRequest) (any, error)) *missingManagement {
	t.Helper()
	if manage == nil {
		manage = c.ManageLocal
	}
	host, err := localhost.New(localhost.Config{RuntimeDir: t.TempDir(), Manage: manage, ServeACP: c.ServeACP})
	if err != nil {
		t.Fatal(err)
	}
	ep, err := host.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := host.Close(); err != nil {
			t.Error(err)
		}
	})
	return &missingManagement{t: t, host: host, ep: ep}
}

func (m *missingManagement) connect(token string) (net.Conn, *bufio.Reader, string) {
	m.t.Helper()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(m.ep.Host, fmt.Sprint(m.ep.Port)), time.Second)
	if err != nil {
		m.t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(conn, "DFACP/1 %s MANAGE\n", token); err != nil {
		conn.Close()
		m.t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		conn.Close()
		m.t.Fatal(err)
	}
	return conn, reader, strings.TrimSpace(line)
}

func (m *missingManagement) wire(fields map[string]any, code string, out any) {
	m.t.Helper()
	conn, reader, line := m.connect(m.ep.Token)
	defer conn.Close()
	if line != "OK" {
		m.t.Fatalf("management handshake: %q", line)
	}
	if err := json.NewEncoder(conn).Encode(fields); err != nil {
		m.t.Fatal(err)
	}
	var response struct {
		OK   bool            `json:"ok"`
		Code string          `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(reader).Decode(&response); err != nil {
		m.t.Fatal(err)
	}
	if response.OK != (code == "") || response.Code != code {
		m.t.Fatalf("MANAGE %v: %+v", fields, response)
	}
	if out != nil {
		if err := json.Unmarshal(response.Data, out); err != nil {
			m.t.Fatal(err)
		}
	}
}

func missingSubmitFields(request harness.SkillMissingReportRequest) map[string]any {
	return map[string]any{"operation": "skill.missing.submit", "session_id": request.SessionID, "run_id": request.RunID, "expected_report_version": request.ExpectedReportVersion, "request_key": request.RequestKey, "kind": request.Kind, "summary": request.Summary}
}

func assertMissingReport(t *testing.T, report harness.SkillMissingReport, session, root string, revision int64, kind string) {
	t.Helper()
	if report.ID == "" || report.RootID != root || report.RootSessionID != session || report.Revision != revision || report.Kind != kind || report.Origin != "operator" || report.Attribution != "unassessed" {
		t.Fatalf("missing report identity/attribution: %+v", report)
	}
}

func TestMissingSkillReportSDKAndAuthenticatedManagement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	workspace, cfg, calls := missingSkillFixture(t, false)
	c, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	session, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	first, err := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "Answer this task without a skill."}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	root := first.Execution.RunID
	chatBefore, err := c.HistoryPage(ctx, session.ID, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	var reads int
	if err := c.store.DB().QueryRowContext(ctx, "SELECT count(*) FROM harness_skill_reads").Scan(&reads); err != nil || reads != 0 || calls.Load() != 1 {
		t.Fatalf("fixture read a skill: reads=%d calls=%d err=%v", reads, calls.Load(), err)
	}
	initial, err := c.GetMissingSkillReport(ctx, session.ID, root)
	if err != nil || initial.Report != nil || initial.RootID != root || initial.RootSessionID != session.ID {
		t.Fatalf("initial snapshot: %+v %v", initial, err)
	}
	targets, err := c.ListMissingSkillReportTargets(ctx, session.ID, "", 10)
	if err != nil || len(targets.Targets) != 1 || targets.Targets[0].RootID != root || targets.Targets[0].RootRunID != root || targets.Targets[0].RootSessionID != session.ID || targets.Targets[0].ReportRevision != 0 {
		t.Fatalf("SDK did not discover the real unread task without a report: %+v %v", targets, err)
	}
	request := harness.SkillMissingReportRequest{SessionID: session.ID, RunID: root, ExpectedReportVersion: 0, RequestKey: "missing-export", Kind: "missing_skill", Summary: "A repeatable report export workflow is missing."}
	cancelled, stop := context.WithCancel(ctx)
	stop()
	if _, err := c.SubmitMissingSkillReport(cancelled, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled submit: %v", err)
	}
	if _, err := c.GetMissingSkillReport(cancelled, session.ID, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled get: %v", err)
	}
	if _, err := c.ListMissingSkillReports(cancelled, session.ID, "", 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled list: %v", err)
	}
	if _, err := c.ListMissingSkillReportHistory(cancelled, session.ID, root, "", 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled history: %v", err)
	}
	if _, err := c.ListMissingSkillReportTargets(cancelled, session.ID, "", 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled targets: %v", err)
	}

	committed, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseResponse := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseResponse()
	m := newMissingManagement(t, ctx, c, func(ctx context.Context, req localhost.ManagementRequest) (any, error) {
		result, err := c.ManageLocal(ctx, req)
		var key string
		_ = json.Unmarshal(req.Fields["request_key"], &key)
		if saved, ok := result.(harness.SkillMissingReportResult); err == nil && ok && key == "lost-response" && !saved.Replayed {
			close(committed)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return result, err
	})
	conn, _, line := m.connect("wrong-token")
	conn.Close()
	if line != "UNAUTHORIZED" {
		t.Fatalf("unauthenticated management: %q", line)
	}
	m.wire(map[string]any{"operation": "skill.missing.targets", "session_id": session.ID}, "", &targets)
	if len(targets.Targets) != 1 || targets.Targets[0].RootRunID != root || targets.Targets[0].ReportRevision != 0 {
		t.Fatalf("MANAGE did not discover the real unread task without a report: %+v", targets)
	}
	submitted, err := c.SubmitMissingSkillReport(ctx, request)
	if err != nil || submitted.Replayed || submitted.CurrentRevision != 1 {
		t.Fatalf("submit: %+v %v", submitted, err)
	}
	assertMissingReport(t, submitted.Report, session.ID, root, 1, "missing_skill")
	fields := missingSubmitFields(request)
	var replay harness.SkillMissingReportResult
	m.wire(fields, "", &replay)
	if !replay.Replayed || replay.CurrentRevision != 1 || replay.Report.Revision != 1 {
		t.Fatalf("SDK/MANAGE replay: %+v", replay)
	}
	for _, key := range []string{"session_id", "run_id", "expected_report_version", "request_key", "kind", "summary"} {
		bad := maps.Clone(fields)
		delete(bad, key)
		m.wire(bad, "invalid_request", nil)
		bad[key] = nil
		m.wire(bad, "invalid_request", nil)
	}
	for _, key := range []string{"actor", "owner", "origin", "attribution", "root_id", "root_session_id", "source_id", "skill_name", "skill_version", "read_ids", "score"} {
		bad := maps.Clone(fields)
		bad[key] = "forged"
		m.wire(bad, "invalid_request", nil)
	}
	for _, badValue := range []struct {
		key   string
		value any
	}{
		{"expected_report_version", -1}, {"expected_report_version", 0.5}, {"expected_report_version", "0"}, {"expected_report_version", true},
		{"session_id", 7}, {"run_id", []string{"root"}}, {"request_key", 123}, {"kind", "skill_failure"}, {"summary", []string{"text"}}, {"summary", ""}, {"summary", strings.Repeat("字", 667)},
	} {
		bad := maps.Clone(fields)
		bad[badValue.key] = badValue.value
		m.wire(bad, "invalid_request", nil)
	}
	for _, operation := range []string{"skill.missing.get", "skill.missing.list", "skill.missing.history", "skill.missing.targets"} {
		valid := map[string]any{"operation": operation, "session_id": session.ID}
		if operation == "skill.missing.get" || operation == "skill.missing.history" {
			valid["run_id"] = root
		}
		for _, key := range []string{"session_id", "run_id"} {
			if _, ok := valid[key]; !ok {
				continue
			}
			bad := maps.Clone(valid)
			delete(bad, key)
			m.wire(bad, "invalid_request", nil)
			bad[key] = nil
			m.wire(bad, "invalid_request", nil)
		}
		bad := maps.Clone(valid)
		bad["owner"] = "forged"
		m.wire(bad, "invalid_request", nil)
		if operation != "skill.missing.get" {
			for _, limit := range []any{nil, 0, -1, 101, 1.5, "1", true} {
				bad := maps.Clone(valid)
				bad["limit"] = limit
				m.wire(bad, "invalid_request", nil)
			}
			for _, cursor := range []any{nil, 1, true, []string{"cursor"}} {
				bad := maps.Clone(valid)
				bad["cursor"] = cursor
				m.wire(bad, "invalid_request", nil)
			}
		}
	}
	for _, limit := range []int{-1, 0, 101} {
		if _, err := c.ListMissingSkillReports(ctx, session.ID, "", limit); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatalf("SDK list limit %d: %v", limit, err)
		}
		if _, err := c.ListMissingSkillReportHistory(ctx, session.ID, root, "", limit); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatalf("SDK history limit %d: %v", limit, err)
		}
		if _, err := c.ListMissingSkillReportTargets(ctx, session.ID, "", limit); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatalf("SDK target limit %d: %v", limit, err)
		}
	}
	stale := maps.Clone(fields)
	stale["request_key"] = "stale"
	m.wire(stale, "conflict", nil)
	changed := maps.Clone(fields)
	changed["summary"] = "Different intent"
	m.wire(changed, "conflict", nil)
	other, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	forged := request
	forged.SessionID, forged.RequestKey = other.ID, "forged-session"
	if _, err := c.SubmitMissingSkillReport(ctx, forged); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatalf("mismatched session/run: %v", err)
	}
	m.wire(missingSubmitFields(forged), "permission_denied", nil)
	m.wire(map[string]any{"operation": "skill.missing.get", "session_id": other.ID, "run_id": root}, "permission_denied", nil)
	unknown := maps.Clone(fields)
	unknown["run_id"] = "unknown-run"
	unknown["request_key"] = "unknown"
	m.wire(unknown, "not_found", nil)

	lost := maps.Clone(fields)
	lost["request_key"], lost["expected_report_version"], lost["summary"] = "lost-response", 1, "A source-checked export workflow is missing."
	conn, _, line = m.connect(m.ep.Token)
	if line != "OK" {
		t.Fatal(line)
	}
	if err := json.NewEncoder(conn).Encode(lost); err != nil {
		t.Fatal(err)
	}
	select {
	case <-committed:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	conn.Close()
	releaseResponse()
	m.wire(lost, "", &replay)
	if !replay.Replayed || replay.CurrentRevision != 2 {
		t.Fatalf("retry after committed response loss: %+v", replay)
	}
	assertMissingReport(t, replay.Report, session.ID, root, 2, "missing_skill")
	withdrawal := maps.Clone(fields)
	withdrawal["request_key"], withdrawal["expected_report_version"], withdrawal["kind"], withdrawal["summary"] = "withdraw", 2, "withdrawn", ""
	m.wire(withdrawal, "", &replay)
	assertMissingReport(t, replay.Report, session.ID, root, 3, "withdrawn")
	m.wire(fields, "", &replay)
	if !replay.Replayed || replay.CurrentRevision != 3 || replay.Report.Revision != 1 {
		t.Fatalf("old receipt changed current assessment: %+v", replay)
	}
	var snapshot harness.SkillMissingReportSnapshot
	m.wire(map[string]any{"operation": "skill.missing.get", "session_id": session.ID, "run_id": root}, "", &snapshot)
	if snapshot.Report == nil || snapshot.Report.Revision != 3 || snapshot.Report.Kind != "withdrawn" {
		t.Fatalf("withdrawal lost: %+v", snapshot)
	}
	var history harness.SkillMissingReportHistoryPage
	m.wire(map[string]any{"operation": "skill.missing.history", "session_id": session.ID, "run_id": root, "limit": 1}, "", &history)
	seen := map[int64]bool{}
	for {
		if len(history.Revisions) != 1 || history.CurrentRevision != 3 || seen[history.Revisions[0].Revision] {
			t.Fatalf("history page: %+v", history)
		}
		seen[history.Revisions[0].Revision] = true
		if history.Next == "" {
			break
		}
		history, err = c.ListMissingSkillReportHistory(ctx, session.ID, root, history.Next, 1)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 3 || calls.Load() != 1 {
		t.Fatalf("reports replayed user work: revisions=%v calls=%d", seen, calls.Load())
	}
	stateAfter, err := c.Execution(ctx, session.ID, root)
	if err != nil || stateAfter.RunID != first.Execution.RunID || stateAfter.InputID != first.Execution.InputID || stateAfter.Status != first.Execution.Status || stateAfter.Version != first.Execution.Version {
		t.Fatalf("report changed source execution: %+v %v", stateAfter, err)
	}
	chatAfter, err := c.HistoryPage(ctx, session.ID, "", 100)
	if err != nil || !reflect.DeepEqual(chatBefore, chatAfter) {
		t.Fatalf("report changed chat history: before=%+v after=%+v err=%v", chatBefore, chatAfter, err)
	}
	second, err := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "Continue after rejected and withdrawn reports."}}, nil, nil)
	if err != nil || calls.Load() != 2 {
		t.Fatalf("next prompt: calls=%d %v", calls.Load(), err)
	}
	secondRequest := request
	secondRequest.RunID, secondRequest.RequestKey = second.Execution.RunID, "other-root"
	secondReport, err := c.SubmitMissingSkillReport(ctx, secondRequest)
	if err != nil {
		t.Fatal(err)
	}
	assertMissingReport(t, secondReport.Report, session.ID, second.Execution.RunID, 1, "missing_skill")
	if secondReport.Report.ID == submitted.Report.ID {
		t.Fatal("independent roots shared a report identity")
	}
	var page harness.SkillMissingReportPage
	m.wire(map[string]any{"operation": "skill.missing.list", "session_id": session.ID, "limit": 1}, "", &page)
	if len(page.Reports) != 1 || page.Next == "" {
		t.Fatalf("root report list: %+v", page)
	}
	firstID, catalogVersion := page.Reports[0].ID, page.CatalogVersion
	page, err = c.ListMissingSkillReports(ctx, session.ID, page.Next, 1)
	if err != nil || len(page.Reports) != 1 || page.Next != "" || page.Reports[0].ID == firstID || page.CatalogVersion != catalogVersion {
		t.Fatalf("root report pagination: %+v %v", page, err)
	}
	if err := c.store.DB().QueryRowContext(ctx, "SELECT count(*) FROM harness_skill_reads").Scan(&reads); err != nil || reads != 0 {
		t.Fatalf("reports invented skill reads: %d %v", reads, err)
	}
	var generated int
	if err := c.store.DB().QueryRowContext(ctx, "SELECT (SELECT count(*) FROM df_skill_proposals)+(SELECT count(*) FROM df_skill_generation_inputs)+(SELECT count(*) FROM df_skill_publications)").Scan(&generated); err != nil || generated != 0 {
		t.Fatalf("reports generated/published candidates: %d %v", generated, err)
	}
	if err := m.host.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SubmitMissingSkillReport(ctx, request); err == nil {
		t.Fatal("closed SDK accepted report")
	}
	if _, err := c.GetMissingSkillReport(ctx, session.ID, root); err == nil {
		t.Fatal("closed SDK read report")
	}
	if _, err := c.ListMissingSkillReports(ctx, session.ID, "", 10); err == nil {
		t.Fatal("closed SDK listed reports")
	}
	if _, err := c.ListMissingSkillReportHistory(ctx, session.ID, root, "", 10); err == nil {
		t.Fatal("closed SDK listed report history")
	}
	if _, err := c.ListMissingSkillReportTargets(ctx, session.ID, "", 10); err == nil {
		t.Fatal("closed SDK listed report targets")
	}
	reopened, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	snapshot, err = reopened.GetMissingSkillReport(ctx, session.ID, root)
	if err != nil || snapshot.Report == nil || snapshot.Report.Kind != "withdrawn" || snapshot.Report.Revision != 3 {
		t.Fatalf("restart report: %+v %v", snapshot, err)
	}
	replay, err = reopened.SubmitMissingSkillReport(ctx, request)
	if err != nil || !replay.Replayed || replay.CurrentRevision != 3 || replay.Report.Revision != 1 {
		t.Fatalf("restart receipt: %+v %v", replay, err)
	}
	history, err = reopened.ListMissingSkillReportHistory(ctx, session.ID, root, "", 10)
	if err != nil || len(history.Revisions) != 3 {
		t.Fatalf("restart history: %+v %v", history, err)
	}
	if _, err := reopened.LoadSession(ctx, session.ID, workspace, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "Continue after restart."}}, nil, nil); err != nil || calls.Load() != 3 {
		t.Fatalf("restart next prompt: calls=%d %v", calls.Load(), err)
	}
}

func missingACPConnection(t *testing.T, ctx context.Context, c *Client) (*protocol.Peer, func()) {
	t.Helper()
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	peer := protocol.NewPeer(clientIn, clientOut, func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		if method == "session/update" {
			return nil, nil
		}
		return nil, &protocol.Error{Code: protocol.MethodNotFound, Message: "unexpected callback"}
	}, protocol.Options{})
	serverDone, clientDone := make(chan error, 1), make(chan error, 1)
	go func() { serverDone <- c.ServeACP(ctx, serverIn, serverOut) }()
	go func() { clientDone <- peer.Serve(ctx) }()
	var once sync.Once
	closeConnection := func() {
		once.Do(func() {
			_ = peer.Close()
			for _, done := range []chan error{clientDone, serverDone} {
				select {
				case err := <-done:
					if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.ErrClosedPipe) {
						t.Error(err)
					}
				case <-time.After(5 * time.Second):
					t.Error("ACP connection did not detach")
				}
			}
		})
	}
	t.Cleanup(closeConnection)
	if err := peer.Call(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}, nil); err != nil {
		t.Fatal(err)
	}
	return peer, closeConnection
}

func TestMissingSkillReportDetachedACPAndPrivateSessionBoundaries(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	workspace, cfg, calls := missingSkillFixture(t, true)
	c, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	peer, disconnect := missingACPConnection(t, ctx, c)
	var session struct {
		ID string `json:"sessionId"`
	}
	if err := peer.Call(ctx, "session/new", map[string]any{"cwd": workspace, "mcpServers": []any{}}, &session); err != nil || session.ID == "" {
		t.Fatalf("ACP session: %+v %v", session, err)
	}
	var prompt continuationACPResponse
	if err := peer.Call(ctx, "session/prompt", map[string]any{"sessionId": session.ID, "prompt": []harness.Content{{Type: "text", Text: "Answer without a skill."}}}, &prompt); err != nil || prompt.Meta.Deerflow.Execution == nil || prompt.StopReason != "end_turn" {
		t.Fatalf("ACP prompt: %+v %v", prompt, err)
	}
	root := prompt.Meta.Deerflow.Execution.RunID
	disconnect()
	m := newMissingManagement(t, ctx, c, nil)
	request := harness.SkillMissingReportRequest{SessionID: session.ID, RunID: root, RequestKey: "detached", Kind: "missing_skill", Summary: "A reusable cross-reference workflow is missing."}
	var saved harness.SkillMissingReportResult
	m.wire(missingSubmitFields(request), "", &saved)
	assertMissingReport(t, saved.Report, session.ID, root, 1, "missing_skill")
	if calls.Load() != 1 {
		t.Fatal("detached management caused a model call")
	}
	if _, err := c.LoadSession(ctx, session.ID, workspace, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Run(ctx, session.ID, []harness.Content{{Type: "text", Text: "Continue after detached reporting."}}, nil, nil); err != nil {
		t.Fatalf("detached report broke session: %v", err)
	}
	parent, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	parentRun, err := c.Run(ctx, parent.ID, []harness.Content{{Type: "text", Text: "CREATE_PRIVATE_TASK"}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := c.BackgroundTasks(ctx, parent.ID, "", 10)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("real private task: %+v %v", tasks, err)
	}
	task := tasks[0]
	for task.Status == "pending" || task.Status == "running" {
		task, err = c.WaitBackgroundTask(ctx, parent.ID, task.ID, task.Version)
		if err != nil {
			t.Fatal(err)
		}
	}
	if task.Status != "completed" || task.ChildSessionID == "" {
		t.Fatalf("private task: %+v", task)
	}
	private := request
	private.SessionID, private.RunID, private.RequestKey = task.ChildSessionID, task.ID, "private-root"
	if _, err := c.SubmitMissingSkillReport(ctx, private); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("private session accepted as root: %v", err)
	}
	m.wire(missingSubmitFields(private), "not_found", nil)
	m.wire(map[string]any{"operation": "skill.missing.get", "session_id": task.ChildSessionID, "run_id": task.ID}, "not_found", nil)
	m.wire(map[string]any{"operation": "skill.missing.list", "session_id": task.ChildSessionID}, "not_found", nil)
	m.wire(map[string]any{"operation": "skill.missing.targets", "session_id": task.ChildSessionID}, "not_found", nil)
	m.wire(map[string]any{"operation": "skill.missing.history", "session_id": task.ChildSessionID, "run_id": task.ID}, "not_found", nil)
	wrong := private
	wrong.SessionID, wrong.RequestKey = session.ID, "wrong-task-tree"
	m.wire(missingSubmitFields(wrong), "permission_denied", nil)
	childReport := private
	childReport.SessionID, childReport.RequestKey = parent.ID, "child-missing"
	m.wire(missingSubmitFields(childReport), "", &saved)
	assertMissingReport(t, saved.Report, parent.ID, parentRun.Execution.RunID, 1, "missing_skill")
	rootSnapshot, err := c.GetMissingSkillReport(ctx, parent.ID, parentRun.Execution.RunID)
	if err != nil || rootSnapshot.Report == nil || rootSnapshot.Report.ID != saved.Report.ID {
		t.Fatalf("child report did not resolve to root: %+v %v", rootSnapshot, err)
	}
	if err := c.CloseSession(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.service.DeleteSession(ctx, parent.ID); err != nil {
		t.Fatalf("descendant report blocked deleting the real task tree: %v", err)
	}
	if _, err := c.GetMissingSkillReport(ctx, parent.ID, parentRun.Execution.RunID); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("deleted tree retained report access: %v", err)
	}
	remaining, err := c.GetMissingSkillReport(ctx, session.ID, root)
	if err != nil || remaining.Report == nil || remaining.Report.Revision != 1 {
		t.Fatalf("deleting a different tree changed its report: %+v %v", remaining, err)
	}
	if err := m.host.Close(); err != nil {
		t.Fatal(err)
	}
}
