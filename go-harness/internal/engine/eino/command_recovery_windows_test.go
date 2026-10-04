package eino

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/sandbox"
	nativetools "github.com/omengye/deerflow-acp/go-harness/internal/tools"
)

// Exercise the real PowerShell process, Eino adapter, durable receipt, model
// correction, and subsequent prompt. No user paths or external services run.
func TestPowerShellFailureContinuesConversation(t *testing.T) {
	for _, partial := range []bool{false, true} {
		name := "parser_error"
		if partial {
			name = "failed_after_partial_effect"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			workspace := t.TempDir()
			backend, err := sandbox.New(ctx, harness.SandboxConfig{Enabled: true, Provider: harness.SandboxPowerShell, AllowShell: true}, workspace)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = backend.Close() })
			command, err := nativetools.CommandTool(backend, harness.SandboxPowerShell)
			if err != nil {
				t.Fatal(err)
			}
			bad := "foreach ($p in @('fixture')) { $p } | Sort-Object"
			good := "[Console]::Write('corrected')"
			if partial {
				bad = "[IO.File]::AppendAllText('effect.txt', 'once'); exit 7"
				good = "[Console]::Write([IO.File]::ReadAllText('effect.txt'))"
			}
			model := &scriptedModel{stream: func(_ context.Context, call int, input []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
				if call > 1 {
					return textStream("conversation continued"), nil
				}
				script, id := bad, "bad-command"
				if call == 1 {
					seen := false
					for _, message := range input {
						if message.Role == schema.Tool && message.ToolCallID == id && strings.Contains(message.Content, "Tool failed") && strings.Contains(message.Content, `"terminationConfirmed":true`) && strings.Contains(message.Content, "Partial effects") {
							seen = true
						}
					}
					if !seen {
						t.Error("model lost the failed command's diagnostic evidence")
					}
					script, id = good, "corrected-command"
				}
				args, _ := json.Marshal(map[string]any{"script": script})
				return schema.StreamReaderFromArray([]*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{ID: id, Type: "function", Function: schema.FunctionCall{Name: "execute", Arguments: string(args)}}}}}), nil
			}}
			service, session, _ := runtimeWithReceiptModel(t, command, model)
			result, err := service.Run(ctx, "owner", session.ID, []harness.Content{{Type: "text", Text: "run"}}, nil, approveReceiptTool)
			if err != nil || result.StopReason != "end_turn" || model.calls != 3 {
				t.Fatalf("result=%+v err=%v model calls=%d", result, err, model.calls)
			}
			receipts, err := service.ListToolReceipts(ctx, "owner", session.ID)
			if err != nil || len(receipts) != 2 {
				t.Fatalf("receipts=%+v err=%v", receipts, err)
			}
			for _, receipt := range receipts {
				want := harness.ReceiptCompleted
				if receipt.ToolCallID == "bad-command" {
					want = harness.ReceiptFailed
					if receipt.Error == "" {
						t.Error("failure receipt lost its error")
					}
				}
				if receipt.State != want {
					t.Fatalf("receipt=%+v want=%s", receipt, want)
				}
			}
			if _, err = service.Run(ctx, "owner", session.ID, []harness.Content{{Type: "text", Text: "next prompt"}}, nil, approveReceiptTool); err != nil {
				t.Fatalf("next prompt blocked: %v", err)
			}
			if partial {
				body, err := os.ReadFile(filepath.Join(workspace, "effect.txt"))
				if err != nil || string(body) != "once" {
					t.Fatalf("failed write was replayed or rolled back: %q %v", body, err)
				}
			}
		})
	}
}
