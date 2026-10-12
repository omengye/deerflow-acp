package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
	"github.com/omengye/deerflow-acp/go-harness/internal/learning"
)

func TestSkillLearningACPOwnerConsentAndStrictFields(t *testing.T) {
	f := newFixture(t, nil)
	l, err := learning.New(context.Background(), f.db.DB(), nil)
	if err != nil {
		t.Fatal(err)
	}
	f.service.Store.Learning = l
	c := connect(t, f.service)
	init := c.success(t, c.request(t, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}))
	if !bytes.Contains(init, []byte(`"skillLearning"`)) || !bytes.Contains(init, []byte(`"mode":"shadow"`)) {
		t.Fatal("missing learning capabilities", string(init))
	}
	sid := c.newSession(t, f.cwd)
	var control harness.SkillLearningControl
	if err = json.Unmarshal(c.success(t, c.request(t, "_deerflow/learning/control/get", map[string]any{"sessionId": sid})), &control); err != nil || control.Contribute || control.Epoch != 0 {
		t.Fatal(control, err)
	}
	params := map[string]any{"sessionId": sid, "contribute": false, "expectedEpoch": 0}
	c.success(t, c.request(t, "_deerflow/learning/control/set", params))
	if msg := c.response(t, c.request(t, "_deerflow/learning/control/set", params)); msg.Error == nil || msg.Error.Code != -32019 {
		t.Fatal("stale consent accepted", msg)
	}
	stranger := connect(t, f.service)
	stranger.initialize(t)
	if msg := stranger.response(t, stranger.request(t, "_deerflow/learning/control/get", map[string]any{"sessionId": sid})); msg.Error == nil || msg.Error.Code != protocol.InvalidParams {
		t.Fatal("foreign connection read learning", msg)
	}
	params["expectedEpoch"] = 1
	params["operator_confirmed"] = true
	if msg := c.response(t, c.request(t, "_deerflow/learning/control/set", params)); msg.Error == nil || msg.Error.Code != protocol.InvalidParams {
		t.Fatal("forged authority accepted", msg)
	}
	c.success(t, c.request(t, "_deerflow/learning/experiences", map[string]any{"sessionId": sid, "limit": 10}))
	c.success(t, c.request(t, "_deerflow/learning/jobs", map[string]any{"sessionId": sid, "limit": 10}))
	c.success(t, c.request(t, "_deerflow/learning/patterns", map[string]any{"sessionId": sid, "limit": 10}))
	if msg := stranger.response(t, stranger.request(t, "_deerflow/learning/patterns", map[string]any{"sessionId": sid})); msg.Error == nil || msg.Error.Code != protocol.InvalidParams {
		t.Fatal("foreign connection saw patterns", msg)
	}
	if msg := stranger.response(t, stranger.request(t, "_deerflow/learning/jobs", map[string]any{"sessionId": sid})); msg.Error == nil || msg.Error.Code != protocol.InvalidParams {
		t.Fatal("foreign connection saw jobs", msg)
	}
	for _, payload := range []string{
		`{"sessionId":%q,"contribute": null ,"expectedEpoch":1}`,
		`{"sessionId":%q,"contribute":false,"expectedEpoch": null }`,
		`{"sessionId":%q,"contribute":false}`,
		`{"sessionId":%q,"contribute":"false","expectedEpoch":1}`,
		`{"sessionId":%q,"contribute":false,"expectedEpoch":1.5}`,
		`{"sessionId":%q,"contribute":false,"expectedEpoch":1,"workspaceShared": null }`,
		`{"sessionId":%q,"contribute":false,"expectedEpoch":1,"workspaceShared":"true"}`,
		`{"sessionId":%q,"contribute":false,"expectedEpoch":1,"workspaceShared":true,"workspaceShared":false}`,
		`{"sessionId":%q,"contribute":false,"contribute":true,"expectedEpoch":1}`,
		`{"SessionId":%q,"contribute":false,"expectedEpoch":1}`,
	} {
		if msg := c.response(t, c.request(t, "_deerflow/learning/control/set", json.RawMessage(fmt.Sprintf(payload, sid)))); msg.Error == nil || msg.Error.Code != protocol.InvalidParams {
			t.Fatal("invalid consent accepted", payload, msg)
		}
	}
	if msg := c.response(t, c.request(t, "_deerflow/learning/workspace/patterns", map[string]any{"sessionId": sid})); msg.Error == nil || msg.Error.Code != protocol.MethodNotFound {
		t.Fatal("single-session ACP grants workspace read authority", msg)
	}
}

func TestSkillLearningMatchingACPOwnerAndStrictQuery(t *testing.T) {
	f := newFixture(t, nil)
	l, err := learning.New(context.Background(), f.db.DB(), nil)
	if err != nil {
		t.Fatal(err)
	}
	f.service.Store.Learning = l
	c := connect(t, f.service)
	init := c.success(t, c.request(t, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}))
	if !bytes.Contains(init, []byte(`"matchesMethod":"_deerflow/learning/matches"`)) {
		t.Fatal("matching extension not advertised", string(init))
	}
	sid := c.newSession(t, f.cwd)
	c.success(t, c.request(t, "_deerflow/learning/matches", map[string]any{"sessionId": sid, "limit": 10}))
	stranger := connect(t, f.service)
	stranger.initialize(t)
	if msg := stranger.response(t, stranger.request(t, "_deerflow/learning/matches", map[string]any{"sessionId": sid})); msg.Error == nil || msg.Error.Code != protocol.InvalidParams {
		t.Fatal("foreign connection read matching history", msg)
	}
	for _, payload := range []string{
		`{"sessionId":%q,"targetSource":"foreign"}`,
		`{"sessionId":%q,"limit": null }`,
		`{"sessionId":%q,"limit":1.5}`,
		`{"sessionId":%q,"limit":10,"limit":20}`,
		`{"SessionId":%q}`,
		`{"sessionId":%q,"operator_confirmed":true}`,
	} {
		if msg := c.response(t, c.request(t, "_deerflow/learning/matches", json.RawMessage(fmt.Sprintf(payload, sid)))); msg.Error == nil || msg.Error.Code != protocol.InvalidParams {
			t.Fatal("invalid match query accepted", payload, msg)
		}
	}
}
