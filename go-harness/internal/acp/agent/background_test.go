package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
)

type backgroundCall struct {
	Method, ID, Cursor  string
	Actor               harness.TaskActor
	AfterVersion, After int64
	Limit               int
	Approval            harness.TaskApproval
	Deadline            time.Time
	Version             int64
}

type backgroundControllerStub struct {
	service *hr.Service
	mu      sync.Mutex
	calls   []backgroundCall
	err     error
	wait    func(context.Context) error
}

// A separate wrapper keeps suspension recovery an optional host capability.
type backgroundResumerStub struct {
	*backgroundControllerStub
	version int64
}

func (s *backgroundResumerStub) ResumeBackgroundTask(ctx context.Context, actor harness.TaskActor, id string, version int64) (harness.BackgroundTask, error) {
	if err := s.record(ctx, backgroundCall{Method: "resume", Actor: actor, ID: id, Version: version}); err != nil {
		return harness.BackgroundTask{}, err
	}
	if version != s.version {
		return harness.BackgroundTask{}, harness.ErrExecutionConflict
	}
	return harness.BackgroundTask{ID: id, SessionID: actor.SessionID, Status: "pending", Version: version + 1}, nil
}

func (s *backgroundControllerStub) record(ctx context.Context, c backgroundCall) error {
	if err := s.service.Coordinator.Authorize(c.Actor.SessionID, c.Actor.OwnerID); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, c)
	return s.err
}
func (s *backgroundControllerStub) snapshot() []backgroundCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]backgroundCall(nil), s.calls...)
}
func backgroundTask(actor harness.TaskActor, id string) harness.BackgroundTask {
	return harness.BackgroundTask{ID: id, SessionID: actor.SessionID, Status: "waiting_input", Version: 7, Interaction: &harness.BackgroundInteraction{
		ID: "batch-1", TaskVersion: 7, Resumable: true,
		WaitingInputs: []harness.ExecutionInteraction{{ID: "intent-1", Version: 1, Kind: "permission", ToolName: "write_file"}},
	}}
}
func (s *backgroundControllerStub) BackgroundTasks(ctx context.Context, actor harness.TaskActor, after string, limit int) ([]harness.BackgroundTask, error) {
	if err := s.record(ctx, backgroundCall{Method: "list", Actor: actor, Cursor: after, Limit: limit}); err != nil {
		return nil, err
	}
	return []harness.BackgroundTask{backgroundTask(actor, "task-1")}, nil
}
func (s *backgroundControllerStub) BackgroundTask(ctx context.Context, actor harness.TaskActor, id string) (harness.BackgroundTask, error) {
	err := s.record(ctx, backgroundCall{Method: "get", Actor: actor, ID: id})
	return backgroundTask(actor, id), err
}
func (s *backgroundControllerStub) WaitBackgroundTask(ctx context.Context, actor harness.TaskActor, id string, version int64) (harness.BackgroundTask, error) {
	deadline, _ := ctx.Deadline()
	if err := s.record(ctx, backgroundCall{Method: "wait", Actor: actor, ID: id, AfterVersion: version, Deadline: deadline}); err != nil {
		return harness.BackgroundTask{}, err
	}
	if s.wait != nil {
		if err := s.wait(ctx); err != nil {
			return harness.BackgroundTask{}, err
		}
	}
	return backgroundTask(actor, id), nil
}
func (s *backgroundControllerStub) CancelBackgroundTask(ctx context.Context, actor harness.TaskActor, id string) (harness.BackgroundTask, error) {
	err := s.record(ctx, backgroundCall{Method: "cancel", Actor: actor, ID: id})
	return harness.BackgroundTask{ID: id, SessionID: actor.SessionID, Status: "running", Version: 8}, err
}
func (s *backgroundControllerStub) ApproveBackgroundTask(ctx context.Context, actor harness.TaskActor, id string, approval harness.TaskApproval) (harness.BackgroundTask, error) {
	err := s.record(ctx, backgroundCall{Method: "approve", Actor: actor, ID: id, Approval: approval})
	return harness.BackgroundTask{ID: id, SessionID: actor.SessionID, Status: "pending", Version: 8}, err
}
func (s *backgroundControllerStub) BackgroundNotifications(ctx context.Context, actor harness.TaskActor, after int64, limit int) ([]harness.BackgroundNotification, error) {
	if err := s.record(ctx, backgroundCall{Method: "notifications", Actor: actor, After: after, Limit: limit}); err != nil {
		return nil, err
	}
	return []harness.BackgroundNotification{{ID: "notification-1", TaskID: "task-1", SessionID: actor.SessionID, Sequence: 18, Kind: "waiting_input", TaskVersion: 7}}, nil
}
func (s *backgroundControllerStub) AcknowledgeBackgroundNotification(ctx context.Context, actor harness.TaskActor, id string) error {
	return s.record(ctx, backgroundCall{Method: "ack", Actor: actor, ID: id})
}

func backgroundFixture(t *testing.T) (*fixture, *backgroundControllerStub, *client, string) {
	t.Helper()
	f := newFixture(t, nil)
	stub := &backgroundControllerStub{service: f.service}
	f.service.Background = stub
	c := connect(t, f.service)
	c.initialize(t)
	return f, stub, c, c.newSession(t, f.cwd)
}

func TestACPBackgroundCapabilitiesAndUnavailable(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			f := newFixture(t, nil)
			if enabled {
				f.service.Background = &backgroundControllerStub{service: f.service}
			}
			c := connect(t, f.service)
			msg := c.response(t, c.request(t, "_deerflow/tasks/list", map[string]any{"sessionId": "session"}))
			if msg.Error == nil || msg.Error.Code != protocol.InvalidRequest {
				t.Fatalf("preinitialize request=%+v", msg)
			}
			init := c.success(t, c.request(t, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}))
			if bytes.Contains(init, []byte(`"background"`)) != enabled {
				t.Fatalf("capabilities=%s", init)
			}
			if enabled && (!bytes.Contains(init, []byte(`"maxWaitMs":10000`)) || !bytes.Contains(init, []byte(`"approveMethod":"_deerflow/tasks/approve"`)) || !bytes.Contains(init, []byte(`"ackMethod":"_deerflow/notifications/ack"`))) {
				t.Fatalf("incomplete capability=%s", init)
			}
			if bytes.Contains(init, []byte(`"resumeMethod":"_deerflow/tasks/resume"`)) {
				t.Fatalf("unsupported resume capability=%s", init)
			}
			for _, method := range []string{"_deerflow/tasks/submit", "_deerflow/tasks/resume"} {
				msg = c.response(t, c.request(t, method, map[string]any{"sessionId": "session", "targets": map[string]any{}}))
				if msg.Error == nil || msg.Error.Code != protocol.MethodNotFound {
					t.Fatalf("unbound submission/native resume accepted: %+v", msg)
				}
			}
			if !enabled {
				for _, method := range []string{"_deerflow/tasks/list", "_deerflow/tasks/get", "_deerflow/tasks/wait", "_deerflow/tasks/cancel", "_deerflow/tasks/approve", "_deerflow/notifications/list", "_deerflow/notifications/ack"} {
					msg = c.response(t, c.request(t, method, map[string]any{"sessionId": "session"}))
					if msg.Error == nil || msg.Error.Code != protocol.MethodNotFound {
						t.Fatalf("disabled method=%s response=%+v", method, msg)
					}
				}
			}
		})
	}
}

func TestACPBackgroundResumeOptionalCapabilityVersionAndOwner(t *testing.T) {
	f := newFixture(t, nil)
	stub := &backgroundResumerStub{backgroundControllerStub: &backgroundControllerStub{service: f.service}, version: 9007199254740993}
	f.service.Background = stub
	c := connect(t, f.service)
	init := c.success(t, c.request(t, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}))
	if !bytes.Contains(init, []byte(`"resumeMethod":"_deerflow/tasks/resume"`)) {
		t.Fatalf("missing resume capability=%s", init)
	}
	sid := c.newSession(t, f.cwd)
	params := map[string]any{"sessionId": sid, "taskId": "task-1", "version": stub.version}
	stranger := connect(t, f.service)
	stranger.initialize(t)
	msg := stranger.response(t, stranger.request(t, "_deerflow/tasks/resume", params))
	if msg.Error == nil {
		t.Fatal("foreign actor resumed task")
	}
	c.notify(t, "_deerflow/tasks/resume", params)
	result := c.success(t, c.request(t, "_deerflow/tasks/resume", params))
	if !bytes.Contains(result, []byte(`"status":"pending"`)) || !bytes.Contains(result, []byte(`"version":9007199254740994`)) {
		t.Fatalf("resume snapshot=%s", result)
	}
	calls := stub.snapshot()
	if len(calls) != 1 || calls[0].Method != "resume" || calls[0].ID != "task-1" || calls[0].Actor.SessionID != sid || calls[0].Actor.OwnerID == "" || calls[0].Version != stub.version {
		t.Fatalf("resume actor/version binding=%+v", calls)
	}
	params["version"] = stub.version - 1
	msg = c.response(t, c.request(t, "_deerflow/tasks/resume", params))
	if msg.Error == nil || msg.Error.Code != -32021 || !strings.Contains(fmt.Sprint(msg.Error.Data), "_deerflow/tasks/get") {
		t.Fatalf("stale resume version=%+v", msg)
	}
}

func TestACPBackgroundResumeStrictParameters(t *testing.T) {
	f := newFixture(t, nil)
	stub := &backgroundResumerStub{backgroundControllerStub: &backgroundControllerStub{service: f.service}, version: 1}
	f.service.Background = stub
	c := connect(t, f.service)
	c.initialize(t)
	sid := c.newSession(t, f.cwd)
	for _, body := range []string{
		`"taskId":"task"`,
		`"taskId":"task","version":0`,
		`"taskId":"task","version":-1`,
		`"taskId":"task","version":1.5`,
		`"taskId":"task","version":1e0`,
		`"taskId":"task","version":9223372036854775808`,
		`"taskId":"task","version":"1"`,
		`"taskId":"task","version":null`,
		`"taskId":"task","Version":1`,
		`"taskId":"task","version":1,"version":2`,
		`"taskId":"task","version":1,"taskId":"other"`,
		`"taskId":"task","version":1,"ownerId":"spoof"`,
		`"taskId":"task","version":1,"afterVersion":1`,
		`"taskId":"task","version":1,"targets":{}`,
		`"taskId":"task","version":1,"approval":{"decision":"allow_once"}`,
		`"taskId":"task","version":1,"checkpoint":"native"`,
		`"taskId":null,"version":1`,
		`"taskId":" task","version":1`,
		`"version":1`,
	} {
		t.Run(body, func(t *testing.T) {
			raw := json.RawMessage(fmt.Sprintf(`{"sessionId":%q,%s}`, sid, body))
			msg := c.response(t, c.request(t, "_deerflow/tasks/resume", raw))
			if msg.Error == nil || msg.Error.Code != protocol.InvalidParams {
				t.Fatalf("malformed resume accepted: %+v", msg)
			}
		})
	}
	if calls := stub.snapshot(); len(calls) != 0 {
		t.Fatalf("invalid resume reached controller: %+v", calls)
	}
}

func TestACPBackgroundManagementBindsActorAndPublicApprovals(t *testing.T) {
	_, stub, c, sid := backgroundFixture(t)
	list := c.success(t, c.request(t, "_deerflow/tasks/list", map[string]any{"sessionId": sid, "after": "task-previous", "limit": 2}))
	if !bytes.Contains(list, []byte(`"tasks":[`)) || bytes.Contains(list, []byte("nativeInterruptId")) || bytes.Contains(list, []byte("grantId")) {
		t.Fatalf("list projection=%s", list)
	}
	c.success(t, c.request(t, "_deerflow/tasks/get", map[string]any{"sessionId": sid, "taskId": "task-1"}))
	beforeWait := time.Now()
	c.success(t, c.request(t, "_deerflow/tasks/wait", map[string]any{"sessionId": sid, "taskId": "task-1", "afterVersion": 6}))
	cancelled := c.success(t, c.request(t, "_deerflow/tasks/cancel", map[string]any{"sessionId": sid, "taskId": "task-1"}))
	if !bytes.Contains(cancelled, []byte(`"status":"running"`)) {
		t.Fatalf("cancel claimed terminal cleanup: %s", cancelled)
	}
	c.success(t, c.request(t, "_deerflow/tasks/approve", map[string]any{"sessionId": sid, "taskId": "task-1", "approval": map[string]any{"id": "batch-1", "taskVersion": 7, "decision": "allow_once"}}))
	c.success(t, c.request(t, "_deerflow/tasks/approve", map[string]any{"sessionId": sid, "taskId": "task-1", "approval": map[string]any{"id": "batch-1", "taskVersion": 7, "decisions": []any{map[string]any{"intentId": "intent-1", "version": 1, "decision": "reject_once"}, map[string]any{"intentId": "intent-2", "version": 2, "decision": "allow_once"}}}}))
	c.success(t, c.request(t, "_deerflow/notifications/list", map[string]any{"sessionId": sid, "after": 17, "limit": 5}))
	c.success(t, c.request(t, "_deerflow/notifications/ack", map[string]any{"sessionId": sid, "notificationId": "notification-1"}))
	calls := stub.snapshot()
	if len(calls) != 8 {
		t.Fatalf("controller calls=%+v", calls)
	}
	owner := calls[0].Actor.OwnerID
	for _, call := range calls {
		if call.Actor.SessionID != sid || call.Actor.OwnerID == "" || call.Actor.OwnerID != owner {
			t.Fatalf("unbound actor=%+v", call)
		}
	}
	if calls[0].Cursor != "task-previous" || calls[0].Limit != 2 || calls[2].AfterVersion != 6 || calls[2].Deadline.Before(beforeWait) || calls[2].Deadline.After(beforeWait.Add(11*time.Second)) {
		t.Fatalf("paging/wait binding=%+v", calls)
	}
	if calls[4].Approval.ID != "batch-1" || calls[4].Approval.TaskVersion != 7 || calls[4].Approval.Decision != harness.AllowOnce || len(calls[4].Approval.Evidence) != 0 {
		t.Fatalf("single approval=%+v", calls[4])
	}
	if calls[5].Approval.Decision != "" || string(calls[5].Approval.Evidence) != `{"decisions":[{"intentId":"intent-1","version":1,"decision":"reject_once"},{"intentId":"intent-2","version":2,"decision":"allow_once"}]}` {
		t.Fatalf("batch approval=%+v", calls[5])
	}
	if calls[6].After != 17 || calls[6].Limit != 5 || calls[7].ID != "notification-1" {
		t.Fatalf("notification binding=%+v", calls)
	}
}

func TestACPBackgroundRejectsUntrustedOrMalformedFields(t *testing.T) {
	_, stub, c, sid := backgroundFixture(t)
	for _, test := range []struct{ method, body string }{
		{"tasks/get", `"taskId":"task","ownerId":"spoof"`},
		{"tasks/get", `"taskId":"task","taskId":"other"`},
		{"tasks/get", `"TaskId":"task"`},
		{"tasks/get", `"taskId":null`},
		{"tasks/get", `"taskId":" task"`},
		{"tasks/list", `"after":42`},
		{"tasks/list", `"after":null`},
		{"tasks/list", `"limit":101`},
		{"tasks/list", `"limit":-1`},
		{"tasks/list", `"limit":1.5`},
		{"tasks/wait", `"taskId":"task","afterVersion":-1`},
		{"tasks/wait", `"taskId":"task","afterVersion":9223372036854775808`},
		{"tasks/wait", `"taskId":"task","afterVersion":1,"timeoutMs":100000`},
		{"tasks/cancel", `"taskId":"task","force":true`},
		{"tasks/approve", `"taskId":"task","targets":{}`},
		{"tasks/approve", `"taskId":"task","approval":{"id":"b","taskVersion":1,"decision":"allow_once","evidence":{}}`},
		{"tasks/approve", `"taskId":"task","approval":{"id":"b","taskVersion":1,"decision":"allow_once","targets":{}}`},
		{"tasks/approve", `"taskId":"task","approval":{"id":"b","taskVersion":1,"decision":"allow_once","grantId":"g"}`},
		{"tasks/approve", `"taskId":"task","approval":{"id":"b","taskVersion":1,"decision":"cancelled"}`},
		{"tasks/approve", `"taskId":"task","approval":{"id":"b","taskVersion":1,"decision":"allow_always"}`},
		{"tasks/approve", `"taskId":"task","approval":{"id":"b","taskVersion":1,"decision":"reject_always"}`},
		{"tasks/approve", `"taskId":"task","approval":{"id":"b","taskVersion":1,"decisions":[{"intentId":"i","version":1,"decision":"allow_always"}]}`},
		{"tasks/approve", `"taskId":"task","approval":{"id":"b","taskVersion":1,"decision":"allow_once","decision":"reject_once"}`},
		{"tasks/approve", `"taskId":"task","approval":{"id":"b","taskVersion":1,"decision":"allow_once","decisions":[]}`},
		{"tasks/approve", `"taskId":"task","approval":{"id":"b","taskVersion":1,"decisions":[]}`},
		{"tasks/approve", `"taskId":"task","approval":{"id":"b","taskVersion":1,"decisions":[{"intentId":"i","version":1,"decision":"allow_once","nativeInterruptId":"n"}]}`},
		{"tasks/approve", `"taskId":"task","approval":{"id":"b","taskVersion":1,"decisions":[{"intentId":"i","version":1,"decision":"allow_once"},{"intentId":"i","version":1,"decision":"allow_once"}]}`},
		{"tasks/approve", `"taskId":"task","approval":{"id":"b","taskVersion":0,"decision":"allow_once"}`},
		{"tasks/approve", `"taskId":"task","approval":{"id":"b","taskVersion":1,"decisions":[{"intentId":"i","version":1.5,"decision":"allow_once"}]}`},
		{"notifications/list", `"after":"17"`},
		{"notifications/list", `"after":-1`},
		{"notifications/ack", `"notificationId":"n","taskId":"task"`},
	} {
		t.Run(test.method+"/"+test.body, func(t *testing.T) {
			raw := json.RawMessage(fmt.Sprintf(`{"sessionId":%q,%s}`, sid, test.body))
			msg := c.response(t, c.request(t, "_deerflow/"+test.method, raw))
			if msg.Error == nil || msg.Error.Code != protocol.InvalidParams {
				t.Fatalf("malformed request accepted: %+v", msg)
			}
		})
	}
	if len(stub.snapshot()) != 0 {
		t.Fatalf("invalid request reached controller: %+v", stub.snapshot())
	}
}

func TestACPBackgroundOwnerChecksAndNoNotificationMutations(t *testing.T) {
	f, stub, c, sid := backgroundFixture(t)
	stranger := connect(t, f.service)
	stranger.initialize(t)
	for _, test := range []struct {
		method string
		params map[string]any
	}{
		{"tasks/list", map[string]any{}}, {"tasks/get", map[string]any{"taskId": "task"}}, {"tasks/wait", map[string]any{"taskId": "task"}}, {"tasks/cancel", map[string]any{"taskId": "task"}},
		{"tasks/approve", map[string]any{"taskId": "task", "approval": map[string]any{"id": "b", "taskVersion": 1, "decision": "allow_once"}}},
		{"notifications/list", map[string]any{}}, {"notifications/ack", map[string]any{"notificationId": "n"}},
	} {
		test.params["sessionId"] = sid
		msg := stranger.response(t, stranger.request(t, "_deerflow/"+test.method, test.params))
		if msg.Error == nil {
			t.Fatalf("foreign actor accepted %s", test.method)
		}
		c.notify(t, "_deerflow/"+test.method, test.params)
	}
	// A request on the same pipe establishes that preceding notifications have
	// been consumed. They must never mutate task approvals, cancellation or ack.
	c.success(t, c.request(t, "_deerflow/tasks/get", map[string]any{"sessionId": sid, "taskId": "task"}))
	if calls := stub.snapshot(); len(calls) != 1 || calls[0].Method != "get" {
		t.Fatalf("unauthorized/notification calls=%+v", calls)
	}
}

func TestACPBackgroundErrorsAndTimeoutSnapshot(t *testing.T) {
	_, stub, c, sid := backgroundFixture(t)
	for _, test := range []struct {
		err  error
		code int
	}{
		{harness.ErrBackgroundUnavailable, -32020}, {harness.ErrBackgroundClosed, -32020}, {harness.ErrExecutionConflict, -32021}, {harness.ErrTaskOriginConflict, -32021},
		{harness.ErrBackgroundUncertain, -32022}, {harness.ErrExecutionUnresumable, -32022}, {harness.ErrChildSessionBusy, protocol.ServerBusy}, {harness.ErrNotFound, protocol.InvalidParams},
	} {
		stub.mu.Lock()
		stub.err = test.err
		stub.mu.Unlock()
		msg := c.response(t, c.request(t, "_deerflow/tasks/get", map[string]any{"sessionId": sid, "taskId": "task"}))
		if msg.Error == nil || msg.Error.Code != test.code {
			t.Fatalf("error=%v response=%+v", test.err, msg)
		}
		if test.code == -32021 && !strings.Contains(fmt.Sprint(msg.Error.Data), "_deerflow/tasks/get") {
			t.Fatalf("wrong recovery method: %+v", msg.Error)
		}
	}
	stub.mu.Lock()
	stub.err = nil
	stub.mu.Unlock()
	stub.wait = func(context.Context) error { return context.DeadlineExceeded }
	result := c.success(t, c.request(t, "_deerflow/tasks/wait", map[string]any{"sessionId": sid, "taskId": "task", "afterVersion": 7}))
	if !bytes.Contains(result, []byte(`"id":"task"`)) {
		t.Fatalf("timeout did not return snapshot: %s", result)
	}
	calls := stub.snapshot()
	if calls[len(calls)-2].Method != "wait" || calls[len(calls)-1].Method != "get" {
		t.Fatalf("timeout did not refresh current state: %+v", calls)
	}
}

func TestACPBackgroundWaitDisconnectStopsOnlyWait(t *testing.T) {
	_, stub, c, sid := backgroundFixture(t)
	started, stopped := make(chan struct{}), make(chan struct{})
	stub.wait = func(ctx context.Context) error { close(started); <-ctx.Done(); close(stopped); return ctx.Err() }
	c.request(t, "_deerflow/tasks/wait", map[string]any{"sessionId": sid, "taskId": "task", "afterVersion": 7})
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("wait did not start")
	}
	if err := c.agent.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("disconnect did not cancel wait context")
	}
	select {
	case <-c.finished:
	case <-time.After(3 * time.Second):
		t.Fatal("disconnect did not join request")
	}
	for _, call := range stub.snapshot() {
		if call.Method == "cancel" {
			t.Fatal("disconnect canceled background task")
		}
	}
	if c.serveErr != nil && !errors.Is(c.serveErr, context.Canceled) {
		t.Fatal(c.serveErr)
	}
}

var _ harness.BackgroundController = (*backgroundControllerStub)(nil)
var _ harness.BackgroundTaskResumer = (*backgroundResumerStub)(nil)
