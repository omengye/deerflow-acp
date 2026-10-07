package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/coder/websocket"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip44"
	"log/slog"
	"strings"
	"sync"
	"time"
)

type observerFrame struct {
	Seq        uint64         `json:"seq"`
	Timestamp  string         `json:"timestamp"`
	Kind       string         `json:"kind"`
	AgentIndex int            `json:"agentIndex"`
	Channel    string         `json:"channelId"`
	Session    string         `json:"sessionId"`
	Turn       string         `json:"turnId"`
	Payload    map[string]any `json:"payload"`
}
type Observer struct {
	config          Config
	queue           chan observerFrame
	cancel          context.CancelFunc
	done            chan struct{}
	mu              sync.Mutex
	seq             map[string]uint64
	connection      *websocket.Conn
	conversationKey [32]byte
}

func NewObserver(ctx context.Context, c Config) (*Observer, error) {
	if !c.Observability.Enabled {
		return nil, nil
	}
	key, err := nip44.GenerateConversationKey(c.Observability.Owner, c.PrivateKey)
	if err != nil {
		return nil, errors.New("cannot initialize owner encryption")
	}
	workerCtx, cancel := context.WithCancel(ctx)
	o := &Observer{config: c, queue: make(chan observerFrame, c.Observability.QueueSize), cancel: cancel, done: make(chan struct{}), seq: map[string]uint64{}, conversationKey: key}
	go o.worker(workerCtx)
	return o, nil
}
func scrub(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := map[string]any{}
		for key, item := range v {
			switch key {
			case "content", "rawInput", "rawOutput", "raw_input", "raw_output":
				continue
			}
			out[key] = scrub(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = scrub(item)
		}
		return out
	case string:
		if len(v) > 4000 {
			return v[:4000] + "…"
		}
	}
	return value
}
func (o *Observer) Emit(kind, channel, session, turn string, payload map[string]any) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seq[session]++
	frame := observerFrame{Seq: o.seq[session], Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Kind: kind, Channel: channel, Session: session, Turn: turn, Payload: payload}
	raw, err := json.Marshal(frame)
	if err != nil || len(raw) > nip44.MaxPlaintextSize {
		return
	}
	select {
	case o.queue <- frame:
	default:
		select {
		case <-o.queue:
		default:
		}
		select {
		case o.queue <- frame:
		default:
		}
	}
}
func (o *Observer) Update(channel, session, turn string, update map[string]any) {
	if o == nil {
		return
	}
	kind, _ := update["sessionUpdate"].(string)
	switch kind {
	case "agent_thought_chunk", "plan", "state_update", "tool_call", "tool_call_update":
	default:
		return
	}
	var payload any = update
	if !o.config.Observability.Raw {
		payload = scrub(update)
	}
	o.Emit("acp_read", channel, session, turn, map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": session, "update": payload}})
}
func (o *Observer) event(frame observerFrame) (nostr.Event, error) {
	body, err := json.Marshal(frame)
	if err != nil {
		return nostr.Event{}, err
	}
	content, err := nip44.Encrypt(string(body), o.conversationKey)
	if err != nil {
		return nostr.Event{}, err
	}
	event := nostr.Event{CreatedAt: nostr.Now(), Kind: 24200, Content: content, Tags: nostr.Tags{{"p", o.config.Observability.Owner}, {"agent", o.config.Buzz.AgentPubkey}, {"frame", "telemetry"}}}
	if frame.Channel != "" {
		event.Tags = append(event.Tags, nostr.Tag{"h", frame.Channel})
	}
	err = event.Sign(o.config.PrivateKey)
	return event, err
}
func (o *Observer) read(ctx context.Context) ([]json.RawMessage, error) {
	_, raw, err := o.connection.Read(ctx)
	if err != nil {
		return nil, err
	}
	var frame []json.RawMessage
	if json.Unmarshal(raw, &frame) != nil || len(frame) == 0 {
		return nil, errors.New("invalid relay frame")
	}
	return frame, nil
}
func (o *Observer) waitOK(ctx context.Context, id string) error {
	for {
		frame, err := o.read(ctx)
		if err != nil {
			return err
		}
		var kind string
		_ = json.Unmarshal(frame[0], &kind)
		if kind == "OK" && len(frame) >= 4 {
			var received string
			var accepted bool
			_ = json.Unmarshal(frame[1], &received)
			_ = json.Unmarshal(frame[2], &accepted)
			if received != id {
				continue
			}
			if !accepted {
				return errors.New("relay rejected observer event")
			}
			return nil
		}
		if kind == "NOTICE" {
			return errors.New("relay observer notice")
		}
	}
}
func (o *Observer) connect(ctx context.Context) error {
	if o.connection != nil {
		return nil
	}
	relay := strings.Replace(strings.Replace(o.config.Buzz.RelayURL, "https://", "wss://", 1), "http://", "ws://", 1)
	connection, _, err := websocket.Dial(ctx, relay, nil)
	if err != nil {
		return err
	}
	o.connection = connection
	connection.SetReadLimit(1 << 20)
	for {
		frame, e := o.read(ctx)
		if e != nil {
			return e
		}
		var kind string
		_ = json.Unmarshal(frame[0], &kind)
		if kind != "AUTH" || len(frame) < 2 {
			continue
		}
		var challenge string
		if json.Unmarshal(frame[1], &challenge) != nil {
			return errors.New("invalid relay auth challenge")
		}
		event := nostr.Event{CreatedAt: nostr.Now(), Kind: 22242, Tags: nostr.Tags{{"relay", relay}, {"challenge", challenge}}}
		if len(o.config.AuthTag) > 0 {
			event.Tags = append(event.Tags, nostr.Tag(o.config.AuthTag))
		}
		if e = event.Sign(o.config.PrivateKey); e != nil {
			return e
		}
		raw, _ := json.Marshal([]any{"AUTH", event})
		if e = connection.Write(ctx, websocket.MessageText, raw); e != nil {
			return e
		}
		return o.waitOK(ctx, event.ID)
	}
}
func (o *Observer) disconnect() {
	if o.connection != nil {
		_ = o.connection.CloseNow()
		o.connection = nil
	}
}
func (o *Observer) worker(ctx context.Context) {
	defer close(o.done)
	defer o.disconnect()
	for {
		select {
		case <-ctx.Done():
			return
		case frame := <-o.queue:
			deadline, cancel := context.WithTimeout(ctx, seconds(o.config.Observability.Timeout))
			event, err := o.event(frame)
			if err == nil {
				err = o.connect(deadline)
			}
			if err == nil {
				raw, _ := json.Marshal([]any{"EVENT", event})
				err = o.connection.Write(deadline, websocket.MessageText, raw)
				if err == nil {
					err = o.waitOK(deadline, event.ID)
				}
			}
			cancel()
			if err != nil {
				o.disconnect()
				slog.Warn("Buzz native observer publish failed; final reply delivery remains available")
				if sleep(ctx, time.Second) != nil {
					return
				}
			}
		}
	}
}
func (o *Observer) Close() {
	if o != nil {
		o.cancel()
		<-o.done
	}
}
