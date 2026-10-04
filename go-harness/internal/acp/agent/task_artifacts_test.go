package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
)

type taskArtifactStub struct {
	*taskTreeStub
	requests []harness.TaskArtifactReadRequest
}

func (s *taskArtifactStub) ReadTaskArtifact(ctx context.Context, actor harness.TaskActor, request harness.TaskArtifactReadRequest) (harness.TaskArtifactChunk, error) {
	if err := s.record(ctx, backgroundCall{Method: "artifact_read", Actor: actor, ID: request.ArtifactID}); err != nil {
		return harness.TaskArtifactChunk{}, err
	}
	s.mu.Lock()
	s.requests = append(s.requests, request)
	s.mu.Unlock()
	return harness.TaskArtifactChunk{
		RootID: request.RootID, NodeID: request.NodeID,
		Artifact: harness.TaskTreeArtifact{ID: request.ArtifactID, Name: "result.txt", MimeType: "text/plain", Size: 2},
		SHA256:   strings.Repeat("a", 64), Encoding: "base64", Data: []byte("ok"),
		Offset: request.Offset, NextOffset: request.Offset + 2, EOF: true,
	}, nil
}

func TestACPTaskArtifactCapabilityAuthorityAndBinaryContract(t *testing.T) {
	f := newFixture(t, engineFunc(func(context.Context, harness.RunRequest, harness.EventHandler, harness.PermissionHandler) (harness.RunResult, error) {
		return harness.RunResult{StopReason: "end_turn"}, nil
	}))
	stub := &taskArtifactStub{taskTreeStub: &taskTreeStub{&backgroundControllerStub{service: f.service}}}
	f.service.Background = stub
	c := connect(t, f.service)
	init := c.success(t, c.request(t, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}))
	if !bytes.Contains(init, []byte(`"artifactReadMethod":"_deerflow/tasks/tree/artifacts/read"`)) {
		t.Fatalf("missing read capability: %s", init)
	}
	sid := c.newSession(t, f.cwd)
	request := map[string]any{"sessionId": sid, "rootId": "root-run", "nodeId": "child-role", "artifactId": "artifact", "offset": 0}
	first := c.success(t, c.request(t, "_deerflow/tasks/tree/artifacts/read", request))
	var chunk harness.TaskArtifactChunk
	if err := json.Unmarshal(first, &chunk); err != nil || string(chunk.Data) != "ok" || chunk.Encoding != "base64" || chunk.NextOffset != 2 || !chunk.EOF {
		t.Fatalf("invalid binary chunk: %s %v", first, err)
	}
	if bytes.Contains(first, []byte(`"path"`)) || bytes.Contains(first, []byte(`"sessionId"`)) {
		t.Fatalf("private storage identity exposed: %s", first)
	}
	request["offset"], request["limit"], request["expectedSha256"] = int64(9007199254740993), 1048576, chunk.SHA256
	c.success(t, c.request(t, "_deerflow/tasks/tree/artifacts/read", request))
	stranger := connect(t, f.service)
	stranger.initialize(t)
	if msg := stranger.response(t, stranger.request(t, "_deerflow/tasks/tree/artifacts/read", request)); msg.Error == nil {
		t.Fatal("another connection read root artifacts")
	}
	stub.mu.Lock()
	requests := append([]harness.TaskArtifactReadRequest(nil), stub.requests...)
	stub.mu.Unlock()
	if len(requests) != 2 || requests[1].Offset != 9007199254740993 || requests[1].Limit != 1048576 || requests[1].ExpectedSHA256 != chunk.SHA256 || requests[1].RootID != "root-run" || requests[1].NodeID != "child-role" {
		t.Fatalf("lost identity, hash or integer precision: %+v", requests)
	}
	for _, call := range stub.snapshot() {
		if call.Actor.SessionID != sid || call.Actor.OwnerID == "" {
			t.Fatalf("missing live authority: %+v", call)
		}
	}
	stub.mu.Lock()
	stub.err = harness.ErrExecutionConflict
	stub.mu.Unlock()
	msg := c.response(t, c.request(t, "_deerflow/tasks/tree/artifacts/read", request))
	if msg.Error == nil || msg.Error.Code != -32021 || !strings.Contains(fmt.Sprint(msg.Error.Data), "_deerflow/tasks/tree/get") {
		t.Fatalf("incorrect artifact recovery: %+v", msg)
	}
	c.success(t, c.request(t, "session/prompt", map[string]any{"sessionId": sid, "prompt": []map[string]string{{"type": "text", "text": "continue"}}}))
}

func TestACPTaskArtifactRejectsUnboundAndMalformedRequests(t *testing.T) {
	f := newFixture(t, nil)
	stub := &taskArtifactStub{taskTreeStub: &taskTreeStub{&backgroundControllerStub{service: f.service}}}
	f.service.Background = stub
	c := connect(t, f.service)
	c.initialize(t)
	sid := c.newSession(t, f.cwd)
	for _, body := range []string{
		``, `,"offset":null`, `,"offset":-1`, `,"offset":1.5`, `,"offset":1e0`,
		`,"offset":"0"`, `,"offset":9223372036854775808`, `,"offset":1`,
		`,"offset":0,"limit":1048577`, `,"offset":0,"limit":null`, `,"offset":0,"limit":-1`,
		`,"offset":0,"offset":1`, `,"offset":0,"expectedSha256":null`, `,"offset":0,"expectedSha256":"invalid"`,
		`,"offset":0,"expectedSha256":"` + strings.Repeat("A", 64) + `"`,
		`,"offset":0,"path":"C:/private"`, `,"offset":0,"ownerId":"spoof"`,
		`,"offset":0,"childSessionId":"private"`, `,"offset":0,"assetRef":{}`, `,"Offset":0`,
	} {
		raw := json.RawMessage(fmt.Sprintf(`{"sessionId":%q,"rootId":"r","nodeId":"n","artifactId":"a"%s}`, sid, body))
		msg := c.response(t, c.request(t, "_deerflow/tasks/tree/artifacts/read", raw))
		if msg.Error == nil || msg.Error.Code != protocol.InvalidParams {
			t.Fatalf("accepted %s: %+v", raw, msg)
		}
	}
	if len(stub.snapshot()) != 0 {
		t.Fatal("invalid read reached controller")
	}
	unsupported := newFixture(t, nil)
	unsupported.service.Background = &taskTreeStub{&backgroundControllerStub{service: unsupported.service}}
	other := connect(t, unsupported.service)
	init := other.success(t, other.request(t, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}))
	if bytes.Contains(init, []byte(`"artifactReadMethod"`)) {
		t.Fatalf("unavailable artifact capability advertised: %s", init)
	}
	msg := other.response(t, other.request(t, "_deerflow/tasks/tree/artifacts/read", map[string]any{"sessionId": sid}))
	if msg.Error == nil || msg.Error.Code != protocol.MethodNotFound {
		t.Fatalf("optional artifact method available: %+v", msg)
	}
}
