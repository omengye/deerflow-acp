package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
)

type fakeAgent struct {
	prompts int
	failure error
}

func (a *fakeAgent) Open(context.Context) error                             { return nil }
func (a *fakeAgent) Close()                                                 {}
func (a *fakeAgent) AttachOrCreate(context.Context, string) (string, error) { return "session", nil }
func (a *fakeAgent) Prompt(_ context.Context, _ string, _ []map[string]any, _ func(map[string]any)) (string, error) {
	a.prompts++
	return "answer", a.failure
}
func offlineApp(t *testing.T) (*App, *fakeAgent) {
	t.Helper()
	c := testConfig(t)
	s := testState(t)
	agent := &fakeAgent{}
	return &App{Config: c, State: s, Buzz: &BuzzCLI{Config: c}, Agent: agent}, agent
}
func TestAppBootstrapAndNewMessage(t *testing.T) {
	a, agent := offlineApp(t)
	old, newMessage := testMessage(t, "a", 100), testMessage(t, "b", 100)
	messages := []Message{old}
	sends := 0
	a.Buzz.runner = func(_ context.Context, args []string, _ string, _ bool, _ io.Writer, _ int64) ([]byte, error) {
		if args[1] == "send" {
			sends++
			return []byte(`{"accepted":true,"event_id":"` + strings.Repeat("c", 64) + `"}`), nil
		}
		limit := 200
		for i, arg := range args {
			if arg == "--limit" {
				limit, _ = strconv.Atoi(args[i+1])
			}
		}
		result := messages
		if limit == 1 {
			result = messages[len(messages)-1:]
		}
		return json.Marshal(result)
	}
	if e := a.Drain(context.Background()); e != nil {
		t.Fatal(e)
	}
	if agent.prompts != 0 || sends != 0 {
		t.Fatal("history replayed")
	}
	messages = append(messages, newMessage)
	if e := a.Drain(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e := a.Drain(context.Background()); e != nil {
		t.Fatal(e)
	}
	if agent.prompts != 1 || sends != 1 {
		t.Fatal("same-second admission or dedup failed", agent.prompts, sends)
	}
}
func TestAppReplyRetryNeverRepeatsPrompt(t *testing.T) {
	a, agent := offlineApp(t)
	m := testMessage(t, "a", 1)
	_ = a.State.EnqueueAndAdvance(context.Background(), m.Channel, 1, []Message{m}, "thread")
	fail := true
	a.Buzz.runner = func(context.Context, []string, string, bool, io.Writer, int64) ([]byte, error) {
		if fail {
			return nil, &CLIError{"transport", "not launched"}
		}
		return []byte(`{"accepted":true,"event_id":"` + strings.Repeat("c", 64) + `"}`), nil
	}
	if e := a.processPending(context.Background()); !errorKind(e, "transport") {
		t.Fatal(e)
	}
	p, _ := a.State.Pending()
	if len(p) != 1 || p[0].Response == nil || p[0].Attempts != 0 {
		t.Fatal("reply not retained")
	}
	fail = false
	if e := a.processPending(context.Background()); e != nil {
		t.Fatal(e)
	}
	if agent.prompts != 1 {
		t.Fatal("prompt replayed", agent.prompts)
	}
}
func TestAppQuarantinesUncertainWork(t *testing.T) {
	for _, mode := range []string{"prompt", "send", "cancelled-send"} {
		t.Run(mode, func(t *testing.T) {
			a, agent := offlineApp(t)
			m := testMessage(t, "a", 1)
			_ = a.State.EnqueueAndAdvance(context.Background(), m.Channel, 1, []Message{m}, "thread")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sends := 0
			if mode == "prompt" {
				agent.failure = errors.New("opaque failure after execution")
			}
			a.Buzz.runner = func(context.Context, []string, string, bool, io.Writer, int64) ([]byte, error) {
				sends++
				if mode == "cancelled-send" {
					cancel()
				}
				return nil, &CLIError{"delivery_unknown", "acknowledgement lost"}
			}
			_ = a.processPending(ctx)
			_ = a.processPending(context.Background())
			counts, _ := a.State.Counts()
			want := "delivery_unknown"
			if mode == "prompt" {
				want = "uncertain"
			}
			if counts[want] != 1 || agent.prompts != 1 || (mode != "prompt" && sends != 1) {
				t.Fatal(counts, agent.prompts, sends)
			}
		})
	}
}
func TestAppTimestampPageStall(t *testing.T) {
	a, _ := offlineApp(t)
	a.Buzz.runner = func(context.Context, []string, string, bool, io.Writer, int64) ([]byte, error) {
		rows := []Message{}
		for i := 0; i < 200; i++ {
			m := testMessage(t, "a", 10)
			m.ID = strings.Repeat("0", 60) + strings.ToLower(strconv.FormatInt(int64(i+4096), 16))
			rows = append(rows, m)
		}
		return json.Marshal(rows)
	}
	if _, e := a.readPages(context.Background(), Channel{ID: "channel"}, nil); e == nil {
		t.Fatal("timestamp tie stalled silently")
	}
	_, exists, _ := a.State.Cursor("channel")
	if exists {
		t.Fatal("cursor moved despite incomplete page")
	}
}
