package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
)

func TestACPHistoryPageSnapshotAndOwner(t *testing.T) {
	f := newFixture(t, nil)
	c := connect(t, f.service)
	init := c.success(t, c.request(t, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}))
	if !bytes.Contains(init, []byte(`"listMethod":"_deerflow/history/list"`)) {
		t.Fatalf("missing history capability: %s", init)
	}
	id := c.newSession(t, f.cwd)
	for _, text := range []string{"one", "two", "three"} {
		if _, err := f.service.Store.Append(context.Background(), harness.RunEvent{SessionID: id, Kind: "text_delta", Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	var first harness.EventPage
	if err := json.Unmarshal(c.success(t, c.request(t, "_deerflow/history/list", map[string]any{"sessionId": id, "limit": 2})), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Events) != 2 || first.NextCursor == "" {
		t.Fatalf("first page=%+v", first)
	}
	if _, err := f.service.Store.Append(context.Background(), harness.RunEvent{SessionID: id, Kind: "text_delta", Text: "later"}); err != nil {
		t.Fatal(err)
	}
	var last harness.EventPage
	if err := json.Unmarshal(c.success(t, c.request(t, "_deerflow/history/list", map[string]any{"sessionId": id, "cursor": first.NextCursor, "limit": 2})), &last); err != nil {
		t.Fatal(err)
	}
	if len(last.Events) != 1 || last.Events[0].Text != "three" || last.NextCursor != "" {
		t.Fatalf("last page=%+v", last)
	}
	stranger := connect(t, f.service)
	stranger.initialize(t)
	response := stranger.response(t, stranger.request(t, "_deerflow/history/list", map[string]any{"sessionId": id}))
	if response.Error == nil || response.Error.Code != protocol.InvalidParams {
		t.Fatalf("owner response=%+v", response)
	}
	for _, params := range []map[string]any{{"sessionId": id, "cursor": "%%%"}, {"sessionId": id, "limit": 501}, {"sessionId": id, "extra": true}} {
		response := c.response(t, c.request(t, "_deerflow/history/list", params))
		if response.Error == nil || response.Error.Code != protocol.InvalidParams {
			t.Fatalf("invalid response=%+v", response)
		}
	}
}
