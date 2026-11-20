package eino

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	einosession "github.com/cloudwego/eino/adk/session"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
	"github.com/omengye/deerflow-acp/go-harness/internal/toolidentity"
)

func engineTestVault(t *testing.T) *secrets.Vault {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	v, err := secrets.Open(context.Background(), store.DB(), filepath.Join(dir, "secrets"))
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		v.Close()
		store.Close()
	})
	return v
}

func randomSecretValue(t *testing.T, prefix string, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return prefix + hex.EncodeToString(b)
}

func unsealedValue(t *testing.T, v *secrets.Vault, callID, arguments string) string {
	t.Helper()
	var fields struct {
		Name        string `json:"name"`
		ValueSealed string `json:"value_sealed"`
	}
	if err := json.Unmarshal([]byte(arguments), &fields); err != nil || fields.ValueSealed == "" {
		t.Fatalf("arguments are not sealed: %s", arguments)
	}
	raw, err := v.Unseal(secrets.ValueBinding(callID, fields.Name), fields.ValueSealed)
	if err != nil {
		t.Fatal(err)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestSealSecretArgumentsMovesValueOutOfArguments(t *testing.T) {
	v := engineTestVault(t)
	value := randomSecretValue(t, "tok-", 12)
	// Go's struct decoder matches keys case-insensitively, so every spelling
	// of the key must be sealed.
	for _, key := range []string{"value", "Value", "VALUE"} {
		args := fmt.Sprintf(`{"action":"set","name":"API_TOKEN",%q:%q,"description":"CI token"}`, key, value)
		sealed, err := sealSecretArguments(v, "call-1", args)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(sealed, value) || strings.Contains(sealed, `"`+key+`"`) {
			t.Fatalf("%s: value left in arguments: %s", key, sealed)
		}
		if got := unsealedValue(t, v, "call-1", sealed); got != value {
			t.Fatalf("%s: unsealed %q", key, got)
		}
		var fields map[string]string
		if err := json.Unmarshal([]byte(sealed), &fields); err != nil || fields["action"] != "set" || fields["description"] != "CI token" {
			t.Fatalf("other arguments changed: %s", sealed)
		}
		// The sealed value is bound to its call and its secret name.
		for _, binding := range []string{secrets.ValueBinding("call-2", "API_TOKEN"), secrets.ValueBinding("call-1", "OTHER_TOKEN")} {
			if _, err := v.Unseal(binding, fields["value_sealed"]); err == nil {
				t.Fatalf("sealed value opened with binding %q", binding)
			}
		}
	}
	for _, args := range []string{`{"name":"A","value":"one-value","Value":"two-value"}`, `{"name":"A","value":"cut-off`, `null`, `["value"]`, `"value"`, ``} {
		sealed, err := sealSecretArguments(v, "call-1", args)
		if err != nil || sealed != MalformedSecretArguments {
			t.Fatalf("%q sealed to %q, %v", args, sealed, err)
		}
	}
	plain := `{"action":"delete", "name":"API_TOKEN"}`
	if sealed, err := sealSecretArguments(v, "call-1", plain); err != nil || sealed != plain {
		t.Fatalf("arguments without a value changed: %q, %v", sealed, err)
	}
}

func collectMessages(t *testing.T, stream *schema.StreamReader[*schema.Message]) []*schema.Message {
	t.Helper()
	defer stream.Close()
	var out []*schema.Message
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, chunk)
	}
}

func TestSealSecretStreamWithholdsValueAtEverySplit(t *testing.T) {
	v := engineTestVault(t)
	value := randomSecretValue(t, "tok-", 8)
	args := `{"action":"set","name":"API_TOKEN","value":"` + value + `"}`
	other := `{"path":"notes.txt"}`
	at := func(i int) *int { return &i }
	for _, variant := range []string{"names-first", "secret-name-late", "other-name-late"} {
		for i := 0; i <= len(args); i++ {
			for j := i; j <= len(args); j++ {
				chunks := []*schema.Message{
					{Role: schema.Assistant, Content: "storing it", ToolCalls: []schema.ToolCall{{Index: at(0), ID: "call-secret", Type: "function", Function: schema.FunctionCall{Name: SecretManageToolName, Arguments: args[:i]}}}},
					{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{Index: at(1), ID: "call-read", Type: "function", Function: schema.FunctionCall{Name: "read_file", Arguments: other[:5]}}, {Index: at(0), Function: schema.FunctionCall{Arguments: args[i:j]}}}},
					{Role: schema.Assistant, ToolCalls: []schema.ToolCall{{Index: at(0), Function: schema.FunctionCall{Arguments: args[j:]}}, {Index: at(1), Function: schema.FunctionCall{Arguments: other[5:]}}}},
				}
				switch variant {
				case "secret-name-late":
					chunks[0].ToolCalls[0].Function.Name = ""
					chunks[1].ToolCalls[1].Function.Name = SecretManageToolName
				case "other-name-late":
					chunks[1].ToolCalls[0].Function.Name = ""
					chunks[2].ToolCalls[1].Function.Name = "read_file"
				}
				got := collectMessages(t, sealSecretStream(v, schema.StreamReaderFromArray(chunks)))
				// Every delta of the secret call is withheld; it appears once, sealed.
				secretParts := 0
				for _, chunk := range got {
					for _, call := range chunk.ToolCalls {
						if call.Index != nil && *call.Index == 0 {
							secretParts++
						}
					}
					if data, _ := json.Marshal(chunk); strings.Contains(string(data), value) {
						t.Fatalf("%s %d/%d: value in chunk %s", variant, i, j, data)
					}
				}
				if secretParts != 1 {
					t.Fatalf("%s %d/%d: secret call emitted in %d parts", variant, i, j, secretParts)
				}
				merged, err := schema.ConcatMessages(got)
				if err != nil {
					t.Fatalf("%s %d/%d: %v", variant, i, j, err)
				}
				if merged.Content != "storing it" || len(merged.ToolCalls) != 2 {
					t.Fatalf("%s %d/%d: merged %+v", variant, i, j, merged)
				}
				for _, call := range merged.ToolCalls {
					switch call.ID {
					case "call-read":
						if call.Function.Name != "read_file" || call.Function.Arguments != other {
							t.Fatalf("%s %d/%d: other call changed: %+v", variant, i, j, call)
						}
					case "call-secret":
						if call.Function.Name != SecretManageToolName || call.Type != "function" || unsealedValue(t, v, "call-secret", call.Function.Arguments) != value {
							t.Fatalf("%s %d/%d: secret call %+v", variant, i, j, call)
						}
					default:
						t.Fatalf("unexpected call %+v", call)
					}
				}
			}
		}
	}
	// A complete call without an index is sealed in place.
	complete := []*schema.Message{{Role: schema.Assistant, ToolCalls: []schema.ToolCall{
		{ID: "whole", Type: "function", Function: schema.FunctionCall{Name: SecretManageToolName, Arguments: args}},
		{ID: "plain", Type: "function", Function: schema.FunctionCall{Name: "read_file", Arguments: other}},
	}}}
	got := collectMessages(t, sealSecretStream(v, schema.StreamReaderFromArray(complete)))
	if len(got) != 1 || len(got[0].ToolCalls) != 2 || got[0].ToolCalls[1].Function.Arguments != other || unsealedValue(t, v, "whole", got[0].ToolCalls[0].Function.Arguments) != value {
		t.Fatalf("complete calls: %+v", got)
	}
	if complete[0].ToolCalls[0].Function.Arguments != args {
		t.Fatal("sealing changed the source message")
	}
}

func TestMaskToolEventsResultsAndStreams(t *testing.T) {
	ctx := context.Background()
	v := engineTestVault(t)
	value := randomSecretValue(t, "tok-", 10)
	if _, err := v.Set(ctx, secrets.SetRequest{Name: "API_TOKEN", Kind: secrets.KindString, Value: []byte(value), Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	event := harness.RunEvent{Kind: "tool_end", ToolName: "read_file", Content: []harness.Content{{Type: "text", Text: `{"content":"token=` + value + `\n"}`}}, Receipt: &harness.ToolReceipt{Error: "request failed with " + value}}
	masked := maskToolEvent(event)
	if data, _ := json.Marshal(masked); strings.Contains(string(data), value) || !strings.Contains(masked.Content[0].Text, "[secret:API_TOKEN]") || masked.Receipt.Error != "request failed with [secret:API_TOKEN]" {
		t.Fatalf("masked event: %s", data)
	}
	if !strings.Contains(event.Content[0].Text, value) || !strings.Contains(event.Receipt.Error, value) {
		t.Fatal("masking changed the source event")
	}
	delta := harness.RunEvent{Kind: "text_delta", Content: []harness.Content{{Type: "text", Text: value}}}
	if maskToolEvent(delta).Content[0].Text != value {
		t.Fatal("model text was masked")
	}

	image := &schema.ToolOutputImage{}
	result := &schema.ToolResult{Parts: []schema.ToolOutputPart{{Type: schema.ToolPartTypeText, Text: "key " + value}, {Type: schema.ToolPartTypeImage, Image: image}}}
	maskedResult := maskToolResult(result)
	if maskedResult.Parts[0].Text != "key [secret:API_TOKEN]" || maskedResult.Parts[1].Image != image || result.Parts[0].Text != "key "+value {
		t.Fatalf("masked result: %+v", maskedResult)
	}
	clean := &schema.ToolResult{Parts: []schema.ToolOutputPart{{Type: schema.ToolPartTypeText, Text: "nothing secret"}}}
	if maskToolResult(clean) != clean {
		t.Fatal("clean result was copied")
	}

	text := "before " + value + " after"
	for split := 0; split <= len(text); split++ {
		for second := split; second <= len(text); second++ {
			stream := maskStringStream(schema.StreamReaderFromArray([]string{text[:split], text[split:second], text[second:]}))
			var b strings.Builder
			for {
				chunk, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				b.WriteString(chunk)
			}
			if b.String() != "before [secret:API_TOKEN] after" {
				t.Fatalf("split %d/%d: %q", split, second, b.String())
			}
		}
	}
	// A failing stream still delivers its masked text before the error.
	reader, writer := schema.Pipe[string](2)
	writer.Send("partial "+value, nil)
	writer.Send("", errors.New("tool stream failed"))
	writer.Close()
	stream := maskStringStream(reader)
	var b strings.Builder
	var streamErr error
	for {
		chunk, err := stream.Recv()
		if err != nil {
			streamErr = err
			break
		}
		b.WriteString(chunk)
	}
	if b.String() != "partial [secret:API_TOKEN]" || streamErr == nil || streamErr.Error() != "tool stream failed" {
		t.Fatalf("failing stream: %q, %v", b.String(), streamErr)
	}
}

type secretFixtureTool struct {
	name string
	run  func(context.Context, string) (string, error)
}

func (t *secretFixtureTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: t.name, Desc: "secret fixture", ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{"action": {Type: schema.String}})}, nil
}

func (t *secretFixtureTool) InvokableRun(ctx context.Context, arguments string, _ ...tool.Option) (string, error) {
	return t.run(ctx, arguments)
}

type trustedSecretFixtureTool struct {
	*secretFixtureTool
	toolidentity.Native
}

func streamedCall(id, name, arguments string, parts int) *schema.StreamReader[*schema.Message] {
	index := 0
	size := (len(arguments) + parts - 1) / parts
	var chunks []*schema.Message
	for start := 0; start < len(arguments); start += size {
		call := schema.ToolCall{Index: &index, Function: schema.FunctionCall{Arguments: arguments[start:min(len(arguments), start+size)]}}
		if start == 0 {
			call.ID, call.Type, call.Function.Name = id, "function", name
		}
		chunks = append(chunks, &schema.Message{Role: schema.Assistant, ToolCalls: []schema.ToolCall{call}})
	}
	return schema.StreamReaderFromArray(chunks)
}

// The model stores a value and reads it back in tool output. Neither trusted
// nor external tools ever return stored secret plaintext to the model, history,
// events or receipts.
func TestSecretValuesStayOutOfHistoryEventsAndReceipts(t *testing.T) {
	ctx := context.Background()
	v := engineTestVault(t)
	valueA := randomSecretValue(t, "a-", 16)
	valueB := randomSecretValue(t, "b-", 1500) // over the inline snapshot threshold
	if _, err := v.Set(ctx, secrets.SetRequest{Name: "SECRET_B", Kind: secrets.KindString, Value: []byte(valueB), Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	manage := &trustedSecretFixtureTool{&secretFixtureTool{name: SecretManageToolName, run: func(ctx context.Context, arguments string) (string, error) {
		var in struct {
			Action      string `json:"action"`
			Name        string `json:"name"`
			Value       string `json:"value"`
			ValueSealed string `json:"value_sealed"`
		}
		if err := json.Unmarshal([]byte(arguments), &in); err != nil {
			return "", err
		}
		switch in.Action {
		case "set":
			if in.Value != "" {
				return "", errors.New("plaintext value reached the tool")
			}
			raw, err := v.Unseal(secrets.ValueBinding(compose.GetToolCallID(ctx), in.Name), in.ValueSealed)
			if err != nil {
				return "", err
			}
			var value string
			if err := json.Unmarshal(raw, &value); err != nil {
				return "", err
			}
			meta, err := v.Set(ctx, secrets.SetRequest{Name: in.Name, Kind: secrets.KindString, Value: []byte(value), Actor: "test"})
			if err != nil {
				return "", err
			}
			return fmt.Sprintf(`{"status":"stored","marker":%q}`, meta.Marker), nil
		}
		return "", errors.New("unexpected action")
	}}, toolidentity.Builtin(SecretManageToolName)}
	read := &trustedSecretFixtureTool{&secretFixtureTool{name: "read_file", run: func(context.Context, string) (string, error) {
		return `{"content":"TOKEN=` + valueA + `\n"}`, nil
	}}, toolidentity.Builtin("read_file")}
	external := &secretFixtureTool{name: "mcp_echo", run: func(ctx context.Context, _ string) (string, error) {
		return `{"echo":"` + valueB + `"}`, nil
	}}
	model := &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		switch call {
		case 0:
			return streamedCall("set-a", SecretManageToolName, `{"action":"set","name":"SECRET_A","value":"`+valueA+`"}`, 4), nil
		case 1:
			return streamedCall("read-a", "read_file", `{"path":".env"}`, 1), nil
		case 2:
			return streamedCall("echo-b", "mcp_echo", `{"action":"echo"}`, 1), nil
		default:
			return textStream("done"), nil
		}
	}}
	store := einosession.NewInMemoryStore[*schema.Message](nil)
	outputs := &capturedToolOutputs{}
	e := newTestEngine(t, Config{ChatModel: model, Model: "fixture", SessionStore: store, Secrets: v, ToolOutputStore: outputs, PermissionMode: harness.PermissionModeOff,
		ToolFactory: func(context.Context, harness.RunRequest) ([]tool.BaseTool, func() error, error) {
			return []tool.BaseTool{manage, read, external}, nil, nil
		}})
	var events []harness.RunEvent
	if _, err := e.Run(ctx, request("secret-run"), func(_ context.Context, event harness.RunEvent) error {
		events = append(events, event)
		return nil
	}, nil); err != nil {
		t.Fatal(err)
	}
	if model.calls != 4 {
		t.Fatalf("model calls = %d", model.calls)
	}
	meta, err := v.Get(ctx, "SECRET_A", false)
	if err != nil || meta.Version != 1 {
		t.Fatalf("SECRET_A was not stored: %+v, %v", meta, err)
	}

	toolMessage := func(call int, id string) string {
		for _, message := range model.seen[call] {
			if message.Role == schema.Tool && message.ToolCallID == id {
				return message.Content
			}
		}
		t.Fatalf("model call %d did not see tool result %s", call, id)
		return ""
	}
	for call := 1; call < len(model.seen); call++ {
		for _, message := range model.seen[call] {
			if data, _ := json.Marshal(message); strings.Contains(string(data), valueA) || strings.Contains(string(data), valueB) {
				t.Fatalf("model call %d saw a secret value: %s", call, data)
			}
		}
	}
	if got := toolMessage(2, "read-a"); got != `{"content":"TOKEN=[secret:SECRET_A]\n"}` {
		t.Fatalf("read result seen by model: %q", got)
	}
	if got := toolMessage(3, "echo-b"); got != `{"echo":"[secret:SECRET_B]"}` {
		t.Fatalf("external tool result seen by model: %.200q", got)
	}
	if outputs.stored != 0 {
		t.Fatalf("%d tool output snapshots were stored", outputs.stored)
	}

	for _, event := range events {
		data, _ := json.Marshal(event)
		if strings.Contains(string(data), valueA) || strings.Contains(string(data), valueB) {
			t.Fatalf("event %s carries a secret value: %.300s", event.Kind, data)
		}
	}

	persisted, err := store.LoadEvents(ctx, "test-session", nil)
	if err != nil {
		t.Fatal(err)
	}
	history, _ := json.Marshal(persisted.Events)
	if strings.Contains(string(history), valueA) || strings.Contains(string(history), valueB) || !strings.Contains(string(history), "value_sealed") {
		t.Fatal("stored value is in the session history or its call was not sealed")
	}
}

func TestResponsesAPIExtraSealsSecretManageArgumentsAndRejectsUnknownKeys(t *testing.T) {
	v := engineTestVault(t)
	secretVal := randomSecretValue(t, "resp-", 16)
	rawArgs := `{"action":"set","name":"API_KEY","value":"` + secretVal + `"}`
	extraItems, _ := json.Marshal([]map[string]any{
		{"type": "function_call", "call_id": "call-r1", "name": SecretManageToolName, "arguments": rawArgs},
	})
	idx := 0
	msg := &schema.Message{
		Role:      schema.Assistant,
		ToolCalls: []schema.ToolCall{{Index: &idx, ID: "call-r1", Type: "function", Function: schema.FunctionCall{Name: SecretManageToolName, Arguments: rawArgs}}},
		Extra:     map[string]any{responsesOutputKey: string(extraItems), responsesTextKey: ""},
	}
	sealed, err := sealSecretCalls(v, msg)
	if err != nil {
		t.Fatal(err)
	}
	gotExtra, _ := sealed.Extra[responsesOutputKey].(string)
	if strings.Contains(gotExtra, secretVal) || !strings.Contains(gotExtra, "value_sealed") {
		t.Fatalf("responses extra was not sealed: %s", gotExtra)
	}
	// Stream path must also seal Extra[responsesOutputKey].
	stream := sealSecretStream(v, schema.StreamReaderFromArray([]*schema.Message{msg}))
	var streamedExtra string
	for {
		chunk, err := stream.Recv()
		if err != nil {
			break
		}
		if s, ok := chunk.Extra[responsesOutputKey].(string); ok {
			streamedExtra = s
		}
	}
	if strings.Contains(streamedExtra, secretVal) || !strings.Contains(streamedExtra, "value_sealed") {
		t.Fatalf("streamed responses extra was not sealed: %s", streamedExtra)
	}
	// Unknown field name carrying a misplaced plaintext secret must be discarded.
	misplaced, err := sealSecretArguments(v, "call-r2", `{"action":"set","name":"API_KEY","token":"`+secretVal+`"}`)
	if err != nil || misplaced != MalformedSecretArguments || strings.Contains(misplaced, secretVal) {
		t.Fatalf("misplaced field not discarded: %s, %v", misplaced, err)
	}
}

func TestMaskEnhancedStreamMasksSecretSplitAcrossChunks(t *testing.T) {
	v := engineTestVault(t)
	secretVal := randomSecretValue(t, "enh-", 16)
	if _, err := v.Set(context.Background(), secrets.SetRequest{Name: "ENH_KEY", Kind: secrets.KindString, Value: []byte(secretVal), Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	mid := len(secretVal) / 2
	src := schema.StreamReaderFromArray([]*schema.ToolResult{
		{Parts: []schema.ToolOutputPart{{Type: schema.ToolPartTypeText, Text: "before " + secretVal[:mid]}}},
		{Parts: []schema.ToolOutputPart{{Type: schema.ToolPartTypeText, Text: secretVal[mid:] + " after"}}},
	})
	masked := maskEnhancedStream(src)
	var b strings.Builder
	for {
		chunk, err := masked.Recv()
		if err != nil {
			break
		}
		for _, p := range chunk.Parts {
			b.WriteString(p.Text)
		}
	}
	if got := b.String(); got != "before [secret:ENH_KEY] after" {
		t.Fatalf("enhanced stream output = %q", got)
	}
}

// A summary may quote a credential the user pasted before it was stored.
func TestCompactionSummaryMasksVaultValues(t *testing.T) {
	v := engineTestVault(t)
	value := randomSecretValue(t, "tok-", 10)
	if _, err := v.Set(context.Background(), secrets.SetRequest{Name: "API_TOKEN", Kind: secrets.KindString, Value: []byte(value), Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	original := []*schema.Message{schema.SystemMessage("policy"), schema.UserMessage("old request " + value), schema.AssistantMessage("old answer", nil), schema.UserMessage("recent request"), schema.AssistantMessage("recent answer", nil), schema.UserMessage("active request")}
	summary := schema.AssistantMessage("The user shared the token "+value+" and asked to store it.", nil)
	after, err := finalizeCompaction(original, summary, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(after[1].Content, "The user shared the token [secret:API_TOKEN] and asked") || strings.Contains(after[1].Content, value) {
		t.Fatalf("summary = %q", after[1].Content)
	}
	if !strings.Contains(summary.Content, value) {
		t.Fatal("masking changed the model's summary message")
	}
}
