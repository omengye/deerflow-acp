package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
)

func TestLifecycleFailureReportsOperationAndContinuesPolling(t *testing.T) {
	a, _ := offlineApp(t)
	a.Config.Buzz.Profile.Name = "DeerFlow"
	a.Config.Buzz.Lifecycle.Status = true
	a.Config.Buzz.Lifecycle.Presence = true
	a.Buzz.Config = a.Config
	t.Setenv("BUZZ_AUTH_TAG", "test-owner-attestation")
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	var operations []string
	a.Buzz.runner = func(_ context.Context, args []string, _ string, _ bool, _ io.Writer, _ int64) ([]byte, error) {
		operation := strings.Join(args[:2], " ")
		operations = append(operations, operation)
		if operation == "users set-presence" {
			return nil, &CLIError{"rejected", "Authentication failed: error: internal error recording agent owner " + testSecret + " test-owner-attestation"}
		}
		if operation == "messages get" {
			return []byte(`[]`), nil
		}
		return []byte(`{"accepted":true}`), nil
	}
	if err := a.Run(context.Background(), true); err != nil {
		t.Fatal("optional lifecycle failure stopped polling", err)
	}
	want := []string{"users set-profile", "users set-status", "users set-presence", "messages get", "users set-status", "users set-presence"}
	if !reflect.DeepEqual(operations, want) {
		t.Fatal("unexpected lifecycle or polling operations", operations)
	}
	if strings.Contains(logs.String(), testSecret) || strings.Contains(logs.String(), "test-owner-attestation") {
		t.Fatal("lifecycle warning exposed credentials")
	}
	warnings := 0
	decoder := json.NewDecoder(&logs)
	for {
		var record struct {
			Level, Operation, Category, Detail string
		}
		if err := decoder.Decode(&record); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if record.Level != "WARN" {
			continue
		}
		warnings++
		if record.Operation != "users set-presence" || record.Category != "rejected" || !strings.Contains(record.Detail, "internal error recording agent owner") || strings.Count(record.Detail, "[redacted]") != 2 {
			t.Fatal("lifecycle warning lost safe diagnostics", record)
		}
	}
	if warnings != 2 {
		t.Fatal("expected startup and shutdown warnings", warnings)
	}
}

func TestLifecyclePresenceCanBeDisabledWithoutDisablingProfileOrStatus(t *testing.T) {
	a, _ := offlineApp(t)
	a.Config.Buzz.Profile.Name = "DeerFlow"
	a.Config.Buzz.Lifecycle.Status = true
	a.Config.Buzz.Lifecycle.Presence = false
	var commands [][]string
	a.Buzz.runner = func(_ context.Context, args []string, _ string, _ bool, _ io.Writer, _ int64) ([]byte, error) {
		commands = append(commands, append([]string{}, args...))
		return []byte(`{"accepted":true}`), nil
	}
	a.lifecycle(context.Background(), true)
	a.lifecycle(context.Background(), false)
	want := [][]string{
		{"users", "set-profile", "--name", "DeerFlow"},
		{"users", "set-status", "--text", a.Config.Buzz.Lifecycle.Ready},
		{"users", "set-status", "--clear"},
	}
	if !reflect.DeepEqual(commands, want) {
		t.Fatal("disabling presence changed other lifecycle updates", commands)
	}
}

func TestCLIWriteRejectionPreservesSafeRelayReason(t *testing.T) {
	for _, message := range []string{"", " ", "internal error recording agent owner", "rejected " + testSecret, "rejected test-owner-attestation"} {
		t.Run(message, func(t *testing.T) {
			t.Setenv("BUZZ_AUTH_TAG", "test-owner-attestation")
			calls := 0
			b := BuzzCLI{Config: testConfig(t), runner: func(context.Context, []string, string, bool, io.Writer, int64) ([]byte, error) {
				calls++
				return json.Marshal(map[string]any{"accepted": false, "message": message})
			}}
			_, err := b.write(context.Background(), []string{"users", "set-presence", "--status", "online"}, "")
			var failure *CLIError
			if !errors.As(err, &failure) || failure.Kind != "rejected" || calls != 1 {
				t.Fatal("rejected lifecycle write was retried or misclassified", err, calls)
			}
			want := "relay rejected the write"
			if strings.TrimSpace(message) != "" {
				want += ": " + b.Config.redact(message)
			}
			if failure.Detail != want || strings.Contains(failure.Detail, testSecret) || strings.Contains(failure.Detail, "test-owner-attestation") {
				t.Fatal("relay rejection lost its safe reason", failure.Detail)
			}
		})
	}
}
