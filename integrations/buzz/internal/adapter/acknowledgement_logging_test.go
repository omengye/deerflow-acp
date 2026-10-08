package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestAppAcknowledgementLogsOutcomeBeforeACP(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		name, wantLevel, wantMessage := "sent", "INFO", "Buzz receipt reaction sent"
		if rejected {
			name, wantLevel, wantMessage = "rejected", "WARN", "Buzz receipt reaction unavailable; continuing task"
		}
		t.Run(name, func(t *testing.T) {
			a, agent := offlineApp(t)
			m := testMessage(t, "a", 1)
			if err := a.State.EnqueueAndAdvance(context.Background(), m.Channel, 1, []Message{m}, "thread"); err != nil {
				t.Fatal(err)
			}
			t.Setenv("BUZZ_AUTH_TAG", "test-owner-attestation")
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })
			agent.onCall = func(event string) {
				if event == "prompt" && !strings.Contains(logs.String(), wantMessage) {
					t.Fatal("task started before the receipt outcome was logged")
				}
			}
			reactions := 0
			a.Buzz.runner = func(_ context.Context, args []string, _ string, _ bool, _ io.Writer, _ int64) ([]byte, error) {
				if args[0] == "reactions" {
					reactions++
					if rejected {
						return nil, &CLIError{"rejected", "relay refused receipt " + testSecret + " test-owner-attestation"}
					}
					return []byte(`{"accepted":true}`), nil
				}
				return []byte(`{"accepted":true,"event_id":"` + strings.Repeat("c", 64) + `"}`), nil
			}
			for range 2 {
				if err := a.processPending(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if agent.prompts != 1 || reactions != 1 {
				t.Fatal("receipt or task was replayed", agent.prompts, reactions)
			}
			if strings.Contains(logs.String(), testSecret) || strings.Contains(logs.String(), "test-owner-attestation") {
				t.Fatal("receipt log exposed credentials")
			}
			var record struct {
				Level, Msg, Category, Detail, Emoji string
				EventID                             string `json:"event_id"`
			}
			decoder := json.NewDecoder(&logs)
			if err := decoder.Decode(&record); err != nil {
				t.Fatal(err)
			}
			if record.Level != wantLevel || record.Msg != wantMessage || record.EventID != m.ID {
				t.Fatal("receipt outcome was not identifiable", record)
			}
			if rejected {
				if record.Category != "rejected" || !strings.Contains(record.Detail, "relay refused receipt") || strings.Count(record.Detail, "[redacted]") != 2 {
					t.Fatal("receipt warning lost safe diagnostics", record)
				}
			} else if record.Emoji != a.Config.Buzz.Acknowledgement.Emoji {
				t.Fatal("receipt log lost the configured emoji", record)
			}
			if err := decoder.Decode(&record); err != io.EOF {
				t.Fatal("unexpected additional receipt log", err, record)
			}
		})
	}
}
