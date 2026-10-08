package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

type fakeAgent struct {
	prompts     int
	failure     error
	openFailure error
	onCall      func(string)
}

func (a *fakeAgent) Open(context.Context) error {
	if a.onCall != nil {
		a.onCall("open")
	}
	return a.openFailure
}
func (a *fakeAgent) Close() {}
func (a *fakeAgent) AttachOrCreate(context.Context, string) (string, error) {
	if a.onCall != nil {
		a.onCall("attach")
	}
	return "session", nil
}
func (a *fakeAgent) Prompt(_ context.Context, _ string, _ []map[string]any, _ func(map[string]any)) (string, error) {
	if a.onCall != nil {
		a.onCall("prompt")
	}
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
	reactions := 0
	a.Buzz.runner = func(_ context.Context, args []string, _ string, _ bool, _ io.Writer, _ int64) ([]byte, error) {
		if args[0] == "reactions" {
			reactions++
			return []byte(`{"accepted":true}`), nil
		}
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
	if agent.prompts != 0 || sends != 0 || reactions != 0 {
		t.Fatal("history replayed")
	}
	messages = append(messages, newMessage)
	if e := a.Drain(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e := a.Drain(context.Background()); e != nil {
		t.Fatal(e)
	}
	if agent.prompts != 1 || sends != 1 || reactions != 1 {
		t.Fatal("same-second admission or dedup failed", agent.prompts, sends, reactions)
	}
}
func TestAppReplyRetryNeverRepeatsPrompt(t *testing.T) {
	a, agent := offlineApp(t)
	m := testMessage(t, "a", 1)
	_ = a.State.EnqueueAndAdvance(context.Background(), m.Channel, 1, []Message{m}, "thread")
	fail := true
	reactions := 0
	a.Buzz.runner = func(_ context.Context, args []string, _ string, _ bool, _ io.Writer, _ int64) ([]byte, error) {
		if args[0] == "reactions" {
			reactions++
			return []byte(`{"accepted":true}`), nil
		}
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
	if agent.prompts != 1 || reactions != 1 {
		t.Fatal("prompt or reaction replayed", agent.prompts, reactions)
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
			a.Buzz.runner = func(_ context.Context, args []string, _ string, _ bool, _ io.Writer, _ int64) ([]byte, error) {
				if args[0] == "reactions" {
					return []byte(`{"accepted":true}`), nil
				}
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

func TestAppAcknowledgesBatchBeforeACP(t *testing.T) {
	a, agent := offlineApp(t)
	m1, m2 := testMessage(t, "a", 1), testMessage(t, "b", 2)
	if err := a.State.EnqueueAndAdvance(context.Background(), m1.Channel, 2, []Message{m1, m2}, "thread"); err != nil {
		t.Fatal(err)
	}
	var events []string
	agent.onCall = func(event string) { events = append(events, event) }
	a.Buzz.runner = func(_ context.Context, args []string, _ string, _ bool, _ io.Writer, _ int64) ([]byte, error) {
		if args[0] == "reactions" {
			events = append(events, "reaction:"+args[3])
			return []byte(`{"accepted":true}`), nil
		}
		events = append(events, "reply")
		return []byte(`{"accepted":true,"event_id":"` + strings.Repeat("c", 64) + `"}`), nil
	}
	if err := a.processPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"reaction:" + m1.ID, "reaction:" + m2.ID, "open", "attach", "prompt", "reply", "open", "attach", "prompt", "reply"}
	if !reflect.DeepEqual(events, want) {
		t.Fatal("receipt did not precede ACP", events)
	}
}

func TestAppReactionFailureDoesNotBlockACP(t *testing.T) {
	for _, mode := range []string{"rejected", "transport", "delivery_unknown", "invalid receipt", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			a, agent := offlineApp(t)
			a.Config.Buzz.Acknowledgement.Timeout = .01
			m := testMessage(t, "a", 1)
			if err := a.State.EnqueueAndAdvance(context.Background(), m.Channel, 1, []Message{m}, "thread"); err != nil {
				t.Fatal(err)
			}
			reactions := 0
			a.Buzz.runner = func(ctx context.Context, args []string, _ string, _ bool, _ io.Writer, _ int64) ([]byte, error) {
				if args[0] == "reactions" {
					reactions++
					if mode == "timeout" {
						<-ctx.Done()
						return nil, &CLIError{"delivery_unknown", "receipt timed out"}
					}
					if mode == "invalid receipt" {
						return []byte("invalid"), nil
					}
					return nil, &CLIError{mode, "receipt unavailable"}
				}
				if ctx.Err() != nil {
					t.Fatal("receipt deadline cancelled task context")
				}
				return []byte(`{"accepted":true,"event_id":"` + strings.Repeat("c", 64) + `"}`), nil
			}
			if err := a.processPending(context.Background()); err != nil {
				t.Fatal(err)
			}
			counts, err := a.State.Counts()
			want := 1
			if mode == "transport" {
				want = a.Config.Adapter.RetryAttempts
			}
			if err != nil || counts["done"] != 1 || agent.prompts != 1 || reactions != want {
				t.Fatal(counts, agent.prompts, reactions, err)
			}
		})
	}
}

func TestAppReactionPersistsAcrossRestart(t *testing.T) {
	a, agent := offlineApp(t)
	m := testMessage(t, "a", 1)
	if err := a.State.EnqueueAndAdvance(context.Background(), m.Channel, 1, []Message{m}, "thread"); err != nil {
		t.Fatal(err)
	}
	agent.openFailure = errors.New("ACP temporarily unavailable")
	reactions := 0
	a.Buzz.runner = func(_ context.Context, args []string, _ string, _ bool, _ io.Writer, _ int64) ([]byte, error) {
		if args[0] == "reactions" {
			reactions++
			return nil, &CLIError{"delivery_unknown", "receipt lost"}
		}
		return []byte(`{"accepted":true,"event_id":"` + strings.Repeat("c", 64) + `"}`), nil
	}
	if err := a.processPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	var path string
	if err := a.State.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	if err := a.State.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := OpenState(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	a.State = s
	if err = s.RecoverInterrupted(); err != nil {
		t.Fatal(err)
	}
	agent.openFailure = nil
	if err = a.processPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	counts, err := s.Counts()
	if err != nil || counts["done"] != 1 || reactions != 1 || agent.prompts != 1 {
		t.Fatal("reaction replayed or task quarantined", counts, reactions, agent.prompts, err)
	}
}

func TestAppReactionOnlyForEligibleWork(t *testing.T) {
	for _, mode := range []string{"disabled", "unauthorized", "no mention", "self", "saved reply"} {
		t.Run(mode, func(t *testing.T) {
			a, agent := offlineApp(t)
			m := testMessage(t, "a", 1)
			wantPrompts := 0
			switch mode {
			case "disabled":
				a.Config.Buzz.Acknowledgement.Enabled = false
				wantPrompts = 1
			case "unauthorized":
				m.Author = strings.Repeat("b", 64)
			case "no mention":
				m.Tags = nil
			case "self":
				m.Author = a.Config.Buzz.AgentPubkey
			}
			if err := a.State.EnqueueAndAdvance(context.Background(), m.Channel, 1, []Message{m}, "thread"); err != nil {
				t.Fatal(err)
			}
			if mode == "saved reply" {
				if err := a.State.SaveResponse(m.ID, "answer"); err != nil {
					t.Fatal(err)
				}
			}
			a.Buzz.runner = func(_ context.Context, args []string, _ string, _ bool, _ io.Writer, _ int64) ([]byte, error) {
				if args[0] == "reactions" {
					t.Fatal("unexpected reaction", mode)
				}
				return []byte(`{"accepted":true,"event_id":"` + strings.Repeat("c", 64) + `"}`), nil
			}
			if err := a.processPending(context.Background()); err != nil || agent.prompts != wantPrompts {
				t.Fatal(err, agent.prompts)
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
