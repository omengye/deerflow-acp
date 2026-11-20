package eino

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
)

// SecretManageToolName is the host tool whose value argument is sealed in
// model output before any event, history row, checkpoint or receipt sees it.
const SecretManageToolName = "secret_manage"

// MalformedSecretArguments replaces secret_manage arguments that are not a
// JSON object, since they may still contain a value that cannot be sealed.
const MalformedSecretArguments = `{"malformed_arguments":true}`

// secretArgumentSealer must be the outermost WrapModel handler: the ADK event
// sender wraps all handlers, so the message it records, the native state and
// every later checkpoint already contain only the sealed value.
type secretArgumentSealer struct {
	adk.BaseChatModelAgentMiddleware
	vault *secrets.Vault
}

func newSecretArgumentSealer(vault *secrets.Vault) adk.ChatModelAgentMiddleware {
	if vault == nil {
		return nil
	}
	return &secretArgumentSealer{vault: vault}
}

func (s *secretArgumentSealer) WrapModel(_ context.Context, inner model.BaseModel[*schema.Message], _ *adk.ModelContext) (model.BaseModel[*schema.Message], error) {
	return &sealingModel{inner: inner, vault: s.vault}, nil
}

type sealingModel struct {
	inner model.BaseModel[*schema.Message]
	vault *secrets.Vault
}

func (m *sealingModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	msg, err := m.inner.Generate(ctx, input, opts...)
	if err != nil || msg == nil {
		return msg, err
	}
	return sealSecretCalls(m.vault, msg)
}

func (m *sealingModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	stream, err := m.inner.Stream(ctx, input, opts...)
	if err != nil || stream == nil {
		return stream, err
	}
	return sealSecretStream(m.vault, stream), nil
}

// sealSecretCalls copies msg only when a call or Responses output changes; the
// inner message may still be referenced by other wrappers.
func sealSecretCalls(v *secrets.Vault, msg *schema.Message) (*schema.Message, error) {
	var out *schema.Message
	for i, call := range msg.ToolCalls {
		if call.Function.Name != SecretManageToolName {
			continue
		}
		sealed, err := sealSecretArguments(v, call.ID, call.Function.Arguments)
		if err != nil {
			return nil, err
		}
		if sealed == call.Function.Arguments {
			continue
		}
		if out == nil {
			copy := *msg
			copy.ToolCalls = append([]schema.ToolCall(nil), msg.ToolCalls...)
			out = &copy
		}
		out.ToolCalls[i].Function.Arguments = sealed
	}
	target := msg
	if out != nil {
		target = out
	}
	return sealResponsesExtra(v, target, out != nil)
}

// sealResponsesExtra seals secret_manage function_call arguments inside
// msg.Extra["deerflow_responses_output"] so OpenAI Responses API history never
// persists or replays plaintext values alongside ToolCalls.
func sealResponsesExtra(v *secrets.Vault, msg *schema.Message, alreadyCopied bool) (*schema.Message, error) {
	if msg == nil || len(msg.Extra) == 0 {
		return msg, nil
	}
	saved, ok := msg.Extra[responsesOutputKey].(string)
	if !ok || saved == "" || !strings.Contains(saved, SecretManageToolName) {
		return msg, nil
	}
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(saved), &raw); err != nil {
		return msg, nil
	}
	changed := false
	for i, entry := range raw {
		var item struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}
		if json.Unmarshal(entry, &item) != nil || item.Type != "function_call" || item.Name != SecretManageToolName {
			continue
		}
		sealed, err := sealSecretArguments(v, item.CallID, item.Arguments)
		if err != nil {
			return nil, err
		}
		if sealed == item.Arguments {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal(entry, &obj); err != nil {
			return nil, err
		}
		obj["arguments"] = sealed
		updated, err := json.Marshal(obj)
		if err != nil {
			return nil, err
		}
		raw[i] = updated
		changed = true
	}
	if !changed {
		return msg, nil
	}
	reencoded, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	out := msg
	if !alreadyCopied {
		copy := *msg
		out = &copy
	}
	extra := make(map[string]any, len(out.Extra))
	for k, val := range out.Extra {
		extra[k] = val
	}
	extra[responsesOutputKey] = string(reencoded)
	out.Extra = extra
	return out, nil
}

// secretManageAllowedKeys lists every field secret_manage accepts. Unknown
// keys may carry a misplaced plaintext value and are discarded before history.
var secretManageAllowedKeys = map[string]bool{
	"action":              true,
	"name":                true,
	"kind":                true,
	"value":               true,
	"from_path":           true,
	"delete_source":       true,
	"generate":            true,
	"description":         true,
	"file_name":           true,
	"version":             true,
	"dry_run":             true,
	"fields":              true,
	"value_sealed":        true,
	"malformed_arguments": true,
}

// sealSecretArguments moves the raw JSON of "value" into "value_sealed",
// encrypted for this call ID and secret name. The tool decrypts it with the
// same binding, so a sealed value copied into another call cannot be used.
// Keys are matched case-insensitively, as a Go struct decoder would match
// them, and unknown keys are rejected as malformed so misplaced plaintext
// values never persist in history.
func sealSecretArguments(v *secrets.Vault, callID, arguments string) (string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(arguments), &fields); err != nil || fields == nil {
		return MalformedSecretArguments, nil
	}
	var raw json.RawMessage
	found := 0
	for key, value := range fields {
		if strings.EqualFold(key, "value") {
			raw = value
			found++
			delete(fields, key)
			continue
		}
		if !secretManageAllowedKeys[key] {
			return MalformedSecretArguments, nil
		}
	}
	if found == 0 {
		return arguments, nil
	}
	if found > 1 {
		return MalformedSecretArguments, nil
	}
	var name string
	_ = json.Unmarshal(fields["name"], &name)
	sealed, err := v.Seal(secrets.ValueBinding(callID, name), raw)
	if err != nil {
		return "", err
	}
	if fields["value_sealed"], err = json.Marshal(sealed); err != nil {
		return "", err
	}
	out, err := json.Marshal(fields)
	return string(out), err
}

// sealSecretStream withholds every streamed delta of a secret_manage call and
// emits the call once, sealed, after the stream ends. Deltas of a call whose
// name is not known yet are withheld until it is. Other content and calls
// pass through unchanged; consumers merge tool calls by index.
func sealSecretStream(v *secrets.Vault, source *schema.StreamReader[*schema.Message]) *schema.StreamReader[*schema.Message] {
	reader, writer := schema.Pipe[*schema.Message](1)
	go func() {
		defer source.Close()
		defer writer.Close()
		s := &secretStreamSealer{vault: v, names: map[int]string{}, held: map[int][]schema.ToolCall{}}
		for {
			chunk, err := source.Recv()
			if errors.Is(err, io.EOF) {
				final, sealErr := s.finish()
				if sealErr != nil {
					writer.Send(nil, sealErr)
				} else if final != nil {
					writer.Send(final, nil)
				}
				return
			}
			if err != nil {
				writer.Send(nil, err)
				return
			}
			out, sealErr := s.push(chunk)
			if sealErr != nil {
				writer.Send(nil, sealErr)
				return
			}
			if writer.Send(out, nil) {
				return
			}
		}
	}()
	return reader
}

type secretStreamSealer struct {
	vault *secrets.Vault
	names map[int]string
	held  map[int][]schema.ToolCall
	order []int
}

func (s *secretStreamSealer) push(chunk *schema.Message) (*schema.Message, error) {
	if chunk == nil {
		return chunk, nil
	}
	var err error
	if len(chunk.Extra) > 0 {
		if chunk, err = sealResponsesExtra(s.vault, chunk, false); err != nil {
			return nil, err
		}
	}
	if len(chunk.ToolCalls) == 0 {
		return chunk, nil
	}
	kept := make([]schema.ToolCall, 0, len(chunk.ToolCalls))
	for _, call := range chunk.ToolCalls {
		if call.Index == nil {
			// A complete call in one chunk is never merged with others.
			if call.Function.Name == SecretManageToolName {
				sealed, err := sealSecretArguments(s.vault, call.ID, call.Function.Arguments)
				if err != nil {
					return nil, err
				}
				call.Function.Arguments = sealed
			}
			kept = append(kept, call)
			continue
		}
		index := *call.Index
		if call.Function.Name != "" {
			s.names[index] = call.Function.Name
		}
		if name, known := s.names[index]; known && name != SecretManageToolName {
			kept = append(kept, s.held[index]...)
			delete(s.held, index)
			kept = append(kept, call)
			continue
		}
		if _, seen := s.held[index]; !seen {
			s.order = append(s.order, index)
		}
		s.held[index] = append(s.held[index], call)
	}
	copy := *chunk
	copy.ToolCalls = kept
	if len(kept) == 0 {
		copy.ToolCalls = nil
	}
	return &copy, nil
}

func (s *secretStreamSealer) finish() (*schema.Message, error) {
	var calls []schema.ToolCall
	for _, index := range s.order {
		parts, ok := s.held[index]
		if !ok || len(parts) == 0 {
			continue
		}
		merged := parts[0]
		var arguments strings.Builder
		for _, part := range parts {
			if merged.ID == "" {
				merged.ID = part.ID
			}
			if merged.Type == "" {
				merged.Type = part.Type
			}
			if merged.Function.Name == "" {
				merged.Function.Name = part.Function.Name
			}
			arguments.WriteString(part.Function.Arguments)
		}
		merged.Function.Arguments = arguments.String()
		if s.names[index] == SecretManageToolName {
			sealed, err := sealSecretArguments(s.vault, merged.ID, merged.Function.Arguments)
			if err != nil {
				return nil, err
			}
			merged.Function.Arguments = sealed
		}
		calls = append(calls, merged)
	}
	if len(calls) == 0 {
		return nil, nil
	}
	return &schema.Message{Role: schema.Assistant, ToolCalls: calls}, nil
}

// maskToolEvent masks known secret values in the text of a tool event before
// it reaches clients, durable events and receipts. Model text deltas are the
// model's own words and are not changed.
func maskToolEvent(event harness.RunEvent) harness.RunEvent {
	if event.Kind == "text_delta" || event.Kind == "reasoning_delta" || !secrets.Active() {
		return event
	}
	if len(event.Content) > 0 {
		content := append([]harness.Content(nil), event.Content...)
		for i := range content {
			if content[i].Text != "" {
				content[i].Text = secrets.MaskOutput(content[i].Text)
			}
		}
		event.Content = content
	}
	if len(event.Arguments) > 0 {
		if masked := secrets.MaskOutput(string(event.Arguments)); masked != string(event.Arguments) && json.Valid([]byte(masked)) {
			event.Arguments = json.RawMessage(masked)
		}
	}
	if event.Receipt != nil && event.Receipt.Error != "" {
		receipt := *event.Receipt
		receipt.Error = secrets.Mask(receipt.Error)
		event.Receipt = &receipt
	}
	return event
}

// maskToolResult masks the text parts of an enhanced tool result.
func maskToolResult(result *schema.ToolResult) *schema.ToolResult {
	if result == nil || !secrets.Active() {
		return result
	}
	var out *schema.ToolResult
	for i, part := range result.Parts {
		if part.Type != schema.ToolPartTypeText || part.Text == "" {
			continue
		}
		masked := secrets.MaskOutput(part.Text)
		if masked == part.Text {
			continue
		}
		if out == nil {
			copy := *result
			copy.Parts = append([]schema.ToolOutputPart(nil), result.Parts...)
			out = &copy
		}
		out.Parts[i].Text = masked
	}
	if out == nil {
		return result
	}
	return out
}

// maskStringStream masks a streamed tool result. A value split across
// chunks is still recognized: the masker withholds a short tail until the
// next chunk or the end of the stream.
func maskStringStream(source *schema.StreamReader[string]) *schema.StreamReader[string] {
	if source == nil || !secrets.Active() {
		return source
	}
	masker := secrets.NewStream()
	reader, writer := schema.Pipe[string](1)
	go func() {
		defer source.Close()
		defer writer.Close()
		for {
			chunk, err := source.Recv()
			if err != nil {
				if rest := masker.Flush(); rest != "" && writer.Send(rest, nil) {
					return
				}
				if !errors.Is(err, io.EOF) {
					writer.Send("", err)
				}
				return
			}
			if out := masker.Push(chunk); out != "" && writer.Send(out, nil) {
				return
			}
		}
	}()
	return reader
}

// maskEnhancedStream masks streamed enhanced tool results with a stateful
// StreamMasker so secrets split across adjacent text chunks cannot escape.
func maskEnhancedStream(source *schema.StreamReader[*schema.ToolResult]) *schema.StreamReader[*schema.ToolResult] {
	if source == nil || !secrets.Active() {
		return source
	}
	masker := secrets.NewStream()
	reader, writer := schema.Pipe[*schema.ToolResult](1)
	go func() {
		defer source.Close()
		defer writer.Close()
		for {
			chunk, err := source.Recv()
			if err != nil {
				if rest := masker.Flush(); rest != "" {
					if writer.Send(&schema.ToolResult{Parts: []schema.ToolOutputPart{{Type: schema.ToolPartTypeText, Text: rest}}}, nil) {
						return
					}
				}
				if !errors.Is(err, io.EOF) {
					writer.Send(nil, err)
				}
				return
			}
			if chunk == nil {
				continue
			}
			parts := make([]schema.ToolOutputPart, 0, len(chunk.Parts))
			for _, part := range chunk.Parts {
				if part.Type == schema.ToolPartTypeText {
					if out := masker.Push(part.Text); out != "" {
						part.Text = out
						parts = append(parts, part)
					}
					continue
				}
				if rest := masker.Flush(); rest != "" {
					parts = append(parts, schema.ToolOutputPart{Type: schema.ToolPartTypeText, Text: rest})
				}
				parts = append(parts, part)
			}
			if len(parts) > 0 {
				copy := *chunk
				copy.Parts = parts
				if writer.Send(&copy, nil) {
					return
				}
			}
		}
	}()
	return reader
}
