package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/acp/protocol"
)

func TestPermissionResponseWaitsForEarlierCancelNotification(t *testing.T) {
	for _, outcome := range []string{"cancelled", "selected"} {
		t.Run(outcome, func(t *testing.T) {
			input, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			reader, output, err := os.Pipe()
			if err != nil {
				input.Close()
				writer.Close()
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered, finishCancel := make(chan struct{}), make(chan struct{})
			var once sync.Once
			a := &Agent{}
			a.peer = protocol.NewPeer(input, output, func(_ context.Context, method string, _ json.RawMessage) (any, error) {
				if method == "session/cancel" {
					close(entered)
					<-finishCancel
					cancel()
				}
				return nil, nil
			}, protocol.Options{})
			served := make(chan error, 1)
			go func() { served <- a.peer.Serve(context.Background()) }()
			t.Cleanup(func() {
				once.Do(func() { close(finishCancel) })
				a.peer.Close()
				writer.Close()
				reader.Close()
				select {
				case <-served:
				case <-time.After(5 * time.Second):
					t.Error("permission test peer did not stop")
				}
			})
			type answer struct {
				decision harness.PermissionDecision
				err      error
			}
			answers := make(chan answer, 1)
			go func() {
				d, err := a.permission(ctx, harness.PermissionRequest{SessionID: "session", ToolCallID: "tool"})
				answers <- answer{d, err}
			}()
			var request struct {
				ID json.RawMessage `json:"id"`
			}
			if err = json.NewDecoder(reader).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if _, err = io.WriteString(writer, "{\"jsonrpc\":\"2.0\",\"method\":\"session/cancel\",\"params\":{\"sessionId\":\"session\"}}\n"); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("cancel was not dispatched")
			}
			response, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"outcome": map[string]string{"outcome": outcome, "optionId": "allow_once"}}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = writer.Write(append(response, '\n')); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-answers:
				t.Fatalf("permission response overtook cancellation: %+v", got)
			case <-time.After(50 * time.Millisecond):
			}
			once.Do(func() { close(finishCancel) })
			select {
			case got := <-answers:
				if got.decision != harness.PermissionCancelled || !errors.Is(got.err, context.Canceled) {
					t.Fatalf("explicit cancellation lost: %+v", got)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("permission did not settle after cancellation")
			}
		})
	}
}
