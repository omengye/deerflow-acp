package eino

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"sync"
	"testing"

	einosession "github.com/cloudwego/eino/adk/session"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
	"github.com/omengye/deerflow-acp/go-harness/internal/toolidentity"
)

// placeholderTool records the arguments each call receives and echoes them.
type placeholderTool struct {
	name string
	mu   sync.Mutex
	args []string
}

func (t *placeholderTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: t.name, Desc: "placeholder fixture", ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{"query": {Type: schema.String}})}, nil
}

func (t *placeholderTool) record(args string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.args = append(t.args, args)
	return `{"received":` + args + `}`
}

func (t *placeholderTool) received() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.args...)
}

type placeholderInvokeTool struct{ *placeholderTool }

func (t placeholderInvokeTool) InvokableRun(_ context.Context, args string, _ ...tool.Option) (string, error) {
	return t.record(args), nil
}

type placeholderStreamTool struct{ *placeholderTool }

func (t placeholderStreamTool) StreamableRun(_ context.Context, args string, _ ...tool.Option) (*schema.StreamReader[string], error) {
	out := t.record(args)
	// Split inside the echoed value so the stream masker must join it.
	return schema.StreamReaderFromArray([]string{out[:len(out)/2], out[len(out)/2:]}), nil
}

type placeholderRichTool struct{ *placeholderTool }

func (t placeholderRichTool) InvokableRun(_ context.Context, args *schema.ToolArgument, _ ...tool.Option) (*schema.ToolResult, error) {
	return &schema.ToolResult{Parts: []schema.ToolOutputPart{{Type: schema.ToolPartTypeText, Text: t.record(args.Text)}}}, nil
}

type placeholderRichStreamTool struct{ *placeholderTool }

func (t placeholderRichStreamTool) StreamableRun(_ context.Context, args *schema.ToolArgument, _ ...tool.Option) (*schema.StreamReader[*schema.ToolResult], error) {
	return schema.StreamReaderFromArray([]*schema.ToolResult{{Parts: []schema.ToolOutputPart{{Type: schema.ToolPartTypeText, Text: t.record(args.Text)}}}}), nil
}

type builtinPlaceholderTool struct {
	placeholderInvokeTool
	toolidentity.Native
}

func builtinPlaceholder(name string) (*placeholderTool, tool.BaseTool) {
	fixture := &placeholderTool{name: name}
	return fixture, builtinPlaceholderTool{placeholderInvokeTool{fixture}, toolidentity.Builtin(name)}
}

// backslash builds JSON escapes at run time.
const backslash = "\x5c"

type scriptedCall struct{ id, name, args string }

func scriptedCalls(calls []scriptedCall) *scriptedModel {
	return &scriptedModel{stream: func(_ context.Context, call int, _ []*schema.Message) (*schema.StreamReader[*schema.Message], error) {
		if call < len(calls) {
			return streamedCall(calls[call].id, calls[call].name, calls[call].args, 2), nil
		}
		return textStream("done"), nil
	}}
}

func decodedField(t *testing.T, args, field string) string {
	t.Helper()
	var fields map[string]string
	if err := json.Unmarshal([]byte(args), &fields); err != nil {
		t.Fatalf("tool arguments are not JSON: %v: %s", err, args)
	}
	return fields[field]
}

// External tools and web_fetch receive vault values; commands refuse
// placeholders; other built-in tools keep them as text. Values never reach
// approval prompts, events, receipts, history or the model.
func TestSecretPlaceholdersReachOnlyTheTool(t *testing.T) {
	ctx := context.Background()
	v := engineTestVault(t)
	core := randomSecretValue(t, "", 12)
	value := "pt-" + core + "+/="
	for name, stored := range map[string]string{"API_TOKEN": value, "SHORT_LIVED": randomSecretValue(t, "sl-", 12)} {
		if _, err := v.Set(ctx, secrets.SetRequest{Name: name, Kind: secrets.KindString, Value: []byte(stored), Actor: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	plain := &placeholderTool{name: "mcp_plain"}
	stream := &placeholderTool{name: "mcp_stream"}
	rich := &placeholderTool{name: "mcp_rich"}
	richStream := &placeholderTool{name: "mcp_rich_stream"}
	fetch, fetchTool := builtinPlaceholder("web_fetch")
	command, commandTool := builtinPlaceholder("execute")
	write, writeTool := builtinPlaceholder("write_file")
	model := scriptedCalls([]scriptedCall{
		{"plain", "mcp_plain", `{"query":"Bearer {{secret:API_TOKEN}}","url":"https://h/?k={{secret:API_TOKEN:url}}"}`},
		{"stream", "mcp_stream", `{"query":"{{secret:API_TOKEN}}"}`},
		{"rich", "mcp_rich", `{"query":"` + backslash + `u007b{secret:API_TOKEN}}"}`}, // a JSON escape still names the secret
		{"rich-stream", "mcp_rich_stream", `{"query":"{{secret:API_TOKEN}}"}`},
		{"fetch", "web_fetch", `{"url":"https://h/?k={{secret:API_TOKEN:url}}"}`},
		{"command", "execute", `{"command":"curl -H 'Authorization: {{secret:API_TOKEN}}' https://h"}`},
		{"write", "write_file", `{"path":"notes.md","content":"use {{secret:API_TOKEN}}"}`},
		{"missing", "mcp_plain", `{"query":"{{secret:NOT_STORED}}"}`},
		{"malformed", "mcp_plain", `{"query":"{{secret:api_token}}"}`},
		{"deleted", "mcp_plain", `{"query":"{{secret:SHORT_LIVED}}"}`},
	})
	store := einosession.NewInMemoryStore[*schema.Message](nil)
	e := newTestEngine(t, Config{ChatModel: model, Model: "fixture", SessionStore: store, Secrets: v, PermissionMode: harness.PermissionModeDangerous,
		ToolFactory: func(context.Context, harness.RunRequest) ([]tool.BaseTool, func() error, error) {
			return []tool.BaseTool{placeholderInvokeTool{plain}, placeholderStreamTool{stream}, placeholderRichTool{rich}, placeholderRichStreamTool{richStream}, fetchTool, commandTool, writeTool}, nil, nil
		}})
	var events []harness.RunEvent
	var requests []harness.PermissionRequest
	if _, err := e.Run(ctx, request("placeholder-run"), func(_ context.Context, event harness.RunEvent) error {
		events = append(events, event)
		return nil
	}, func(ctx context.Context, req harness.PermissionRequest) (harness.PermissionDecision, error) {
		requests = append(requests, req)
		if req.ToolCallID == "deleted" {
			// The secret disappears between approval and dispatch.
			if _, err := v.Delete(ctx, "SHORT_LIVED", "test"); err != nil {
				return harness.RejectOnce, err
			}
		}
		return harness.AllowOnce, nil
	}); err != nil {
		t.Fatal(err)
	}

	encoded := url.QueryEscape(value)
	for _, check := range []struct {
		fixture        *placeholderTool
		field, want    string
		otherField     string
		otherWantValue string
	}{
		{plain, "query", "Bearer " + value, "url", "https://h/?k=" + encoded},
		{stream, "query", value, "", ""},
		{rich, "query", value, "", ""},
		{richStream, "query", value, "", ""},
		{fetch, "url", "https://h/?k=" + encoded, "", ""},
		{write, "content", "use {{secret:API_TOKEN}}", "", ""},
	} {
		got := check.fixture.received()
		if len(got) != 1 {
			t.Fatalf("%s ran %d times: %q", check.fixture.name, len(got), got)
		}
		if field := decodedField(t, got[0], check.field); field != check.want {
			t.Errorf("%s received %s=%q, want %q", check.fixture.name, check.field, field, check.want)
		}
		if check.otherField != "" {
			if field := decodedField(t, got[0], check.otherField); field != check.otherWantValue {
				t.Errorf("%s received %s=%q, want %q", check.fixture.name, check.otherField, field, check.otherWantValue)
			}
		}
	}
	if got := command.received(); len(got) != 0 {
		t.Fatalf("command ran with a placeholder: %q", got)
	}

	asked := map[string]bool{}
	for _, req := range requests {
		asked[req.ToolCallID] = true
		if data, _ := json.Marshal(req); strings.Contains(string(data), core) {
			t.Fatalf("approval request %s shows the value: %s", req.ToolCallID, data)
		}
		if req.ToolCallID == "plain" && !strings.Contains(string(req.Arguments), "{{secret:API_TOKEN}}") {
			t.Fatalf("approval request does not name the secret: %s", req.Arguments)
		}
	}
	for _, id := range []string{"plain", "stream", "rich", "rich-stream", "fetch", "write", "deleted"} {
		if !asked[id] {
			t.Errorf("call %s was not approved before it ran", id)
		}
	}
	for _, id := range []string{"command", "missing", "malformed"} {
		if asked[id] {
			t.Errorf("call %s asked for approval although it cannot run", id)
		}
	}

	results := map[string]string{}
	for _, message := range model.seen[len(model.seen)-1] {
		if data, _ := json.Marshal(message); strings.Contains(string(data), core) {
			t.Fatalf("the model saw the value: %s", data)
		}
		if message.Role == schema.Tool {
			results[message.ToolCallID] = message.Content
			for _, part := range message.UserInputMultiContent {
				results[message.ToolCallID] += part.Text
			}
		}
	}
	for id, want := range map[string][]string{
		"plain":     {"[secret:API_TOKEN]", "[secret:API_TOKEN:url]"},
		"stream":    {"[secret:API_TOKEN]"},
		"fetch":     {"[secret:API_TOKEN:url]"},
		"command":   {"does not substitute", `secrets={"ENV_VAR":"NAME"}`},
		"missing":   {"secrets not found: NOT_STORED"},
		"malformed": {"malformed secret placeholder"},
		"deleted":   {"secrets not found: SHORT_LIVED"},
	} {
		for _, text := range want {
			if !strings.Contains(results[id], text) {
				t.Errorf("model result for %s = %q, want %q", id, results[id], text)
			}
		}
	}

	notExecuted := map[string]bool{}
	for _, event := range events {
		if data, _ := json.Marshal(event); strings.Contains(string(data), core) {
			t.Fatalf("event %s for %s carries the value: %.300s", event.Kind, event.ToolCallID, data)
		}
		if event.Kind == "tool_end" && event.Receipt != nil && event.Receipt.State == harness.ReceiptNotExecuted {
			notExecuted[event.ToolCallID] = true
		}
	}
	for _, id := range []string{"command", "missing", "malformed", "deleted"} {
		if !notExecuted[id] {
			t.Errorf("call %s has no not-executed receipt", id)
		}
	}
	persisted, err := store.LoadEvents(ctx, "test-session", nil)
	if err != nil {
		t.Fatal(err)
	}
	history, _ := json.Marshal(persisted.Events)
	if strings.Contains(string(history), core) || !strings.Contains(string(history), "{{secret:API_TOKEN:url}}") {
		t.Fatal("history carries the value or lost the placeholder")
	}
}

// Without a vault the feature is off and placeholders are ordinary text.
func TestSecretPlaceholdersWithoutVaultStayText(t *testing.T) {
	external := &placeholderTool{name: "mcp_plain"}
	command, commandTool := builtinPlaceholder("execute")
	model := scriptedCalls([]scriptedCall{
		{"external", "mcp_plain", `{"query":"{{secret:API_TOKEN}}"}`},
		{"command", "execute", `{"command":"echo {{secret:API_TOKEN}}"}`},
	})
	e := newTestEngine(t, Config{ChatModel: model, Model: "fixture", PermissionMode: harness.PermissionModeOff,
		ToolFactory: func(context.Context, harness.RunRequest) ([]tool.BaseTool, func() error, error) {
			return []tool.BaseTool{placeholderInvokeTool{external}, commandTool}, nil, nil
		}})
	if _, err := e.Run(context.Background(), request("no-vault"), func(context.Context, harness.RunEvent) error { return nil }, nil); err != nil {
		t.Fatal(err)
	}
	if got := external.received(); len(got) != 1 || decodedField(t, got[0], "query") != "{{secret:API_TOKEN}}" {
		t.Fatalf("external tool received %q", got)
	}
	if got := command.received(); len(got) != 1 || decodedField(t, got[0], "command") != "echo {{secret:API_TOKEN}}" {
		t.Fatalf("command received %q", got)
	}
}

// Built-in command and delegation tools must never receive substituted
// plaintext even if a caller constructs them without explicit mock wrappers.
func TestUntrustedCommandAndDelegationNamesNeverSubstitutePlaceholders(t *testing.T) {
	ctx := context.Background()
	v := engineTestVault(t)
	secretVal := randomSecretValue(t, "cmd-", 12)
	if _, err := v.Set(ctx, secrets.SetRequest{Name: "API_TOKEN", Kind: secrets.KindString, Value: []byte(secretVal), Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	execTool := &placeholderTool{name: "execute"}
	skillTool := &placeholderTool{name: "skill_run"}
	opencliTool := &placeholderTool{name: "host_opencli"}
	acpTool := &placeholderTool{name: "invoke_acp_agent"}
	presentTool := &placeholderTool{name: "present_files"}
	model := scriptedCalls([]scriptedCall{
		{"exec", "execute", `{"script":"echo {{secret:API_TOKEN}}"}`},
		{"srun", "skill_run", `{"args":["{{secret:API_TOKEN}}"]}`},
		{"ocli", "host_opencli", `{"command":"{{secret:API_TOKEN}}"}`},
		{"acp", "invoke_acp_agent", `{"agent":"ext","prompt":"use {{secret:API_TOKEN}}"}`},
		{"pres", "present_files", `{"paths":["{{secret:API_TOKEN}}"]}`},
	})
	e := newTestEngine(t, Config{ChatModel: model, Model: "fixture", Secrets: v, PermissionMode: harness.PermissionModeDangerous,
		ToolFactory: func(context.Context, harness.RunRequest) ([]tool.BaseTool, func() error, error) {
			return []tool.BaseTool{
				placeholderInvokeTool{execTool}, placeholderInvokeTool{skillTool},
				placeholderInvokeTool{opencliTool}, placeholderInvokeTool{acpTool},
				placeholderInvokeTool{presentTool},
			}, nil, nil
		}})
	if _, err := e.Run(ctx, request("builtin-guard"), func(context.Context, harness.RunEvent) error { return nil },
		func(context.Context, harness.PermissionRequest) (harness.PermissionDecision, error) {
			return harness.AllowOnce, nil
		}); err != nil {
		t.Fatal(err)
	}
	for _, blocked := range []*placeholderTool{execTool, skillTool, opencliTool} {
		if got := blocked.received(); len(got) != 0 {
			t.Fatalf("%s ran with placeholder instead of being rejected: %q", blocked.name, got)
		}
	}
	for _, literal := range []*placeholderTool{acpTool, presentTool} {
		got := literal.received()
		if len(got) != 1 || strings.Contains(got[0], secretVal) || !strings.Contains(got[0], "{{secret:API_TOKEN}}") {
			t.Fatalf("%s received %q; want literal placeholder only", literal.name, got)
		}
	}
}
