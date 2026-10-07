package adapter

import (
	"context"
	"encoding/json"
	"github.com/nbd-wtf/go-nostr/nip44"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestObserverEncryptionAndRedaction(t *testing.T) {
	c := testConfig(t)
	c.Observability.Owner = c.Buzz.AllowedPubkeys[0]
	key, e := nip44.GenerateConversationKey(c.Observability.Owner, testSecret)
	if e != nil {
		t.Fatal(e)
	}
	o := &Observer{config: c, queue: make(chan observerFrame, 1), seq: map[string]uint64{}, conversationKey: key}
	u := map[string]any{"sessionUpdate": "tool_call", "title": "Read", "rawInput": map[string]any{"secret": "private"}, "content": []any{map[string]any{"text": "private"}}}
	o.Update("channel", "session", "turn", u)
	frame := <-o.queue
	event, e := o.event(frame)
	if e != nil {
		t.Fatal(e)
	}
	if valid, e := event.CheckSignature(); e != nil || !valid {
		t.Fatal(valid, e)
	}
	plain, e := nip44.Decrypt(event.Content, key)
	if e != nil || strings.Contains(plain, "private") || !strings.Contains(plain, "Read") {
		t.Fatal("redaction or encryption failed", e)
	}
	if event.Kind != 24200 || strings.Contains(event.Content, "Read") {
		t.Fatal("observer frame not encrypted")
	}
}
func TestObserverQueueBoundedAndNonblocking(t *testing.T) {
	o := &Observer{config: testConfig(t), queue: make(chan observerFrame, 2), seq: map[string]uint64{}}
	for i := 0; i < 1000; i++ {
		o.Emit("test", "c", "s", "t", map[string]any{"status": "ok"})
	}
	if len(o.queue) != 2 {
		t.Fatal("queue unbounded")
	}
	first := <-o.queue
	if first.Seq != 999 {
		t.Fatal("did not retain recent frames")
	}
}
func TestProgressCompletionAndPrivacy(t *testing.T) {
	c := testConfig(t)
	c.Observability.Progress = true
	c.Observability.Interval = .01
	var mu sync.Mutex
	var publications []string
	published := make(chan struct{}, 10)
	b := &BuzzCLI{Config: c, runner: func(_ context.Context, args []string, input string, _ bool, _ io.Writer, _ int64) ([]byte, error) {
		body := input
		if args[1] == "edit" {
			body = args[len(args)-1]
		}
		mu.Lock()
		publications = append(publications, body)
		mu.Unlock()
		published <- struct{}{}
		return json.Marshal(map[string]any{"accepted": true, "event_id": strings.Repeat("a", 64)})
	}}
	p := NewProgress(context.Background(), b, Pending{Message: Message{ID: strings.Repeat("b", 64), Channel: "c"}})
	p.Update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "call", "title": "Read", "status": "completed", "rawInput": "private"})
	select {
	case <-published:
	case <-time.After(2 * time.Second):
		t.Fatal("no progress")
	}
	p.Finish("ok")
	mu.Lock()
	defer mu.Unlock()
	if len(publications) != 2 || !strings.Contains(publications[1], "完成") || strings.Contains(strings.Join(publications, ""), "private") {
		t.Fatal(publications)
	}
}
