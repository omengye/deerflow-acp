package eino

import (
	"bytes"
	"context"
	"testing"

	"github.com/cloudwego/eino/adk"
)

type nestedCheckpointAgent struct{ state []byte }

func (*nestedCheckpointAgent) Name(context.Context) string { return "checkpoint-size-probe" }
func (*nestedCheckpointAgent) Description(context.Context) string {
	return "checkpoint size regression"
}
func (a *nestedCheckpointAgent) Run(ctx context.Context, _ *adk.AgentInput, _ ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	iter, generator := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	go func() {
		defer generator.Close()
		// Match the public ChatModelAgent interrupt: canonical state plus its
		// legacy presentation view. Runner must persist the bytes only once.
		event := adk.StatefulInterrupt(ctx, "native state", a.state)
		event.Action.Interrupted.Data = &adk.ChatModelAgentInterruptInfo{Data: a.state}
		generator.Send(event)
	}()
	return iter
}

func TestEinoNestedCheckpointSizeDoesNotMultiply(t *testing.T) {
	ctx := context.Background()
	original := bytes.Repeat([]byte{0xab}, 1<<20)
	payload := original
	for depth := 1; depth <= 3; depth++ {
		store := &memoryCheckpoints{items: make(map[string][]byte)}
		runner := adk.NewRunner(ctx, adk.RunnerConfig{Agent: &nestedCheckpointAgent{state: payload}, CheckPointStore: store})
		iter := runner.Run(ctx, nil, adk.WithCheckPointID("nested-size"))
		for {
			event, ok := iter.Next()
			if !ok {
				break
			}
			if event.Err != nil {
				t.Fatal(event.Err)
			}
		}
		var found bool
		var err error
		payload, found, err = store.Get(ctx, "nested-size")
		if err != nil || !found {
			t.Fatalf("depth %d checkpoint missing: %v", depth, err)
		}
		t.Logf("depth=%d original_bytes=%d native_checkpoint_bytes=%d", depth, len(original), len(payload))
	}
	if len(payload) >= len(original)+(64<<10) {
		t.Fatalf("nested checkpoint duplicates payload: original=%d nested=%d", len(original), len(payload))
	}
}
