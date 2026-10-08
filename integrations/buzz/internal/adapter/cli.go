package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

type CLIError struct {
	Kind   string
	Detail string
}

func (e *CLIError) Error() string { return "Buzz CLI " + e.Kind + ": " + e.Detail }
func errorKind(err error, kind string) bool {
	var e *CLIError
	return errors.As(err, &e) && e.Kind == kind
}

type limitedOutput struct {
	w         io.Writer
	remaining int64
	overflow  bool
	cancel    context.CancelFunc
}

func (w *limitedOutput) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		w.overflow = true
		if w.cancel != nil {
			w.cancel()
		}
		return 0, errors.New("subprocess output exceeds limit")
	}
	n, err := w.w.Write(p)
	w.remaining -= int64(n)
	return n, err
}

type BuzzCLI struct {
	Config Config
	runner func(context.Context, []string, string, bool, io.Writer, int64) ([]byte, error)
}

func (b *BuzzCLI) run(ctx context.Context, args []string, input string, write bool, output io.Writer, limit int64) ([]byte, error) {
	if b.runner != nil {
		return b.runner(ctx, args, input, write, output, limit)
	}
	deadline, cancel := context.WithTimeout(ctx, seconds(b.Config.Buzz.Timeout))
	defer cancel()
	cmd := exec.CommandContext(deadline, b.Config.Buzz.Command, append(append([]string{}, b.Config.Buzz.Args...), args...)...)
	hideCommand(cmd)
	cmd.WaitDelay = 2 * time.Second
	// Normalize the CLI's HTTP endpoint explicitly; native observation uses WS.
	relay := b.Config.Buzz.RelayURL
	relay = strings.Replace(relay, "wss://", "https://", 1)
	relay = strings.Replace(relay, "ws://", "http://", 1)
	cmd.Env = mergeEnv(os.Environ(), map[string]string{"BUZZ_RELAY_URL": relay, "BUZZ_PRIVATE_KEY": b.Config.PrivateKey})
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	var stdout, stderr bytes.Buffer
	target := output
	if target == nil {
		target = &stdout
	}
	if limit <= 0 {
		limit = 8 << 20
	}
	boundedOut := &limitedOutput{w: target, remaining: limit, cancel: cancel}
	boundedErr := &limitedOutput{w: &stderr, remaining: 64 << 10, cancel: cancel}
	cmd.Stdout = boundedOut
	cmd.Stderr = boundedErr
	if err := cmd.Start(); err != nil {
		return nil, &CLIError{"transport", b.Config.redact(fmt.Sprintf("cannot start %s: %v", b.Config.Buzz.Command, err))}
	}
	err := cmd.Wait()
	if deadline.Err() != nil {
		kind := "transport"
		if write {
			kind = "delivery_unknown"
		}
		return nil, &CLIError{kind, "subprocess interrupted; delivery may be uncertain"}
	}
	if err != nil || boundedOut.overflow || boundedErr.overflow {
		var payload struct {
			Error     string `json:"error"`
			Message   string `json:"message"`
			Retryable bool   `json:"retryable"`
		}
		valid := json.Unmarshal(stderr.Bytes(), &payload) == nil
		if !valid {
			valid = json.Unmarshal(stdout.Bytes(), &payload) == nil
		}
		kind := "rejected"
		detail := "subprocess failed"
		if valid {
			if payload.Error == "delivery_unknown" {
				kind = "delivery_unknown"
			} else if payload.Retryable {
				kind = "transport"
			}
			if payload.Message != "" {
				detail = payload.Message
			}
		} else if write {
			kind = "delivery_unknown"
		}
		return nil, &CLIError{kind, b.Config.redact(detail)}
	}
	return stdout.Bytes(), nil
}
func (b *BuzzCLI) retry(ctx context.Context, operation func() error) error {
	var err error
	delay := seconds(b.Config.Adapter.RetryBase)
	for i := 0; i < b.Config.Adapter.RetryAttempts; i++ {
		err = operation()
		if !errorKind(err, "transport") {
			return err
		}
		if i+1 < b.Config.Adapter.RetryAttempts {
			if e := sleep(ctx, delay); e != nil {
				return e
			}
			delay = min(delay*2, seconds(b.Config.Adapter.RetryMax))
		}
	}
	return err
}
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func mergeEnv(base []string, values map[string]string) []string {
	out := make([]string, 0, len(base)+len(values))
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		found := false
		for candidate := range values {
			if strings.EqualFold(key, candidate) {
				found = true
				break
			}
		}
		if !found {
			out = append(out, entry)
		}
	}
	for k, v := range values {
		out = append(out, k+"="+v)
	}
	return out
}
func (b *BuzzCLI) array(ctx context.Context, args ...string) ([]json.RawMessage, error) {
	var rows []json.RawMessage
	err := b.retry(ctx, func() error {
		body, e := b.run(ctx, args, "", false, nil, 0)
		if e != nil {
			return e
		}
		if json.Unmarshal(body, &rows) != nil || rows == nil {
			return &CLIError{"rejected", "invalid JSON array"}
		}
		return nil
	})
	return rows, err
}
func (b *BuzzCLI) Channels(ctx context.Context) ([]Channel, error) {
	channels := map[string]Channel{}
	for _, id := range b.Config.Buzz.Channels {
		channels[id] = Channel{ID: id}
	}
	if b.Config.Buzz.AutoDiscover {
		rows, err := b.array(ctx, "channels", "list", "--member", "--limit", "500")
		if err != nil {
			return nil, err
		}
		for _, raw := range rows {
			var value struct {
				ID   string `json:"channel_id"`
				Name string `json:"name"`
			}
			if json.Unmarshal(raw, &value) == nil && value.ID != "" {
				channels[value.ID] = Channel{ID: value.ID, Name: value.Name}
			}
		}
	}
	if b.Config.Buzz.IncludeDMs {
		rows, err := b.array(ctx, "dms", "list", "--limit", "200")
		if err != nil {
			return nil, err
		}
		for _, raw := range rows {
			var value struct {
				ID string `json:"dm_id"`
			}
			if json.Unmarshal(raw, &value) == nil && value.ID != "" {
				channels[value.ID] = Channel{ID: value.ID, DM: true}
			}
		}
	}
	result := make([]Channel, 0, len(channels))
	for _, channel := range channels {
		result = append(result, channel)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}
func (b *BuzzCLI) Messages(ctx context.Context, channel Channel, limit int, before *int64, since *int64) ([]Message, error) {
	kinds := []string{}
	for _, kind := range b.Config.Buzz.MessageKinds {
		kinds = append(kinds, strconv.Itoa(kind))
	}
	args := []string{"messages", "get", "--channel", channel.ID, "--limit", strconv.Itoa(limit), "--kinds", strings.Join(kinds, ",")}
	if before != nil {
		args = append(args, "--before", strconv.FormatInt(*before, 10))
	}
	if since != nil {
		args = append(args, "--since", strconv.FormatInt(*since, 10))
	}
	rows, err := b.array(ctx, args...)
	if err != nil {
		return nil, err
	}
	result := []Message{}
	for _, row := range rows {
		if m, ok := parseMessage(channel, row); ok {
			result = append(result, m)
		} else {
			return nil, &CLIError{"rejected", "invalid message in relay page; cursor was not advanced"}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Created == result[j].Created {
			return result[i].ID < result[j].ID
		}
		return result[i].Created < result[j].Created
	})
	return result, nil
}
func (b *BuzzCLI) write(ctx context.Context, args []string, input string) (map[string]any, error) {
	var result map[string]any
	err := b.retry(ctx, func() error {
		body, e := b.run(ctx, args, input, true, nil, 0)
		if e != nil {
			return e
		}
		if json.Unmarshal(body, &result) != nil || result == nil {
			return &CLIError{"delivery_unknown", "successful write returned an invalid receipt"}
		}
		if result["accepted"] == false {
			detail := "relay rejected the write"
			if message, ok := result["message"].(string); ok && strings.TrimSpace(message) != "" {
				detail += ": " + message
			}
			return &CLIError{"rejected", b.Config.redact(detail)}
		}
		return nil
	})
	return result, err
}
func (b *BuzzCLI) Send(ctx context.Context, channel, reply, body string) (string, error) {
	result, err := b.write(ctx, []string{"messages", "send", "--channel", channel, "--reply-to", reply, "--content", "-"}, body)
	if err != nil {
		return "", err
	}
	id, _ := result["event_id"].(string)
	if id == "" {
		id, _ = result["id"].(string)
	}
	if !hexID(id) {
		return "", &CLIError{"delivery_unknown", "successful send did not return an event ID"}
	}
	return id, nil
}
func (b *BuzzCLI) Edit(ctx context.Context, id, body string) error {
	_, err := b.write(ctx, []string{"messages", "edit", "--event", id, "--content", body}, "")
	return err
}
func (b *BuzzCLI) React(ctx context.Context, id, emoji string) error {
	_, err := b.write(ctx, []string{"reactions", "add", "--event", id, "--emoji", emoji}, "")
	return err
}
