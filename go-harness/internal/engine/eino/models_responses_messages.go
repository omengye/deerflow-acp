package eino

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/openai/openai-go/v3/responses"
)

const responsesOutputKey = "deerflow_responses_output"
const responsesTextKey = "deerflow_responses_text"
const responsesOriginKey = "deerflow_responses_origin"

type responsesItem struct {
	Type         string `json:"type"`
	ID           string `json:"id,omitempty"`
	CallID       string `json:"call_id,omitempty"`
	Name         string `json:"name,omitempty"`
	Arguments    string `json:"arguments,omitempty"`
	Status       string `json:"status,omitempty"`
	Result       string `json:"result,omitempty"`
	OutputFormat string `json:"output_format,omitempty"`
	Error        *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
	Content []struct {
		Type    string `json:"type"`
		Text    string `json:"text,omitempty"`
		Refusal string `json:"refusal,omitempty"`
	} `json:"content,omitempty"`
}

func responsesInput(messages []*schema.Message) ([]any, error) {
	return responsesInputForOrigin(messages, "")
}

func responsesInputForOrigin(messages []*schema.Message, origin string) ([]any, error) {
	items := make([]any, 0, len(messages))
	for _, message := range messages {
		if message == nil {
			return nil, fmt.Errorf("%w: nil Responses message", harness.ErrInvalidInput)
		}
		if message.Role == schema.Assistant {
			images := responsesAssistantImages(message)
			if saved, ok := message.Extra[responsesOutputKey].(string); ok && saved != "" && (origin == "" || message.Extra[responsesOriginKey] == origin) {
				var raw []json.RawMessage
				if len(saved) > 16<<20 || json.Unmarshal([]byte(saved), &raw) != nil {
					return nil, fmt.Errorf("%w: invalid Responses history", harness.ErrModelResponseInvalid)
				}
				// Output continuation may consolidate several visible answers.
				// Preserve opaque reasoning, but regenerate the edited message.
				unchanged := message.Extra[responsesTextKey] == message.Content
				calls := map[string]schema.ToolCall{}
				for _, call := range message.ToolCalls {
					if _, duplicate := calls[call.ID]; duplicate || call.ID == "" {
						return nil, fmt.Errorf("%w: invalid Responses history tool identity", harness.ErrModelResponseInvalid)
					}
					calls[call.ID] = call
				}
				messageWritten := false
				imageIndex := 0
				for _, entry := range raw {
					var item responsesItem
					if json.Unmarshal(entry, &item) != nil {
						return nil, fmt.Errorf("%w: invalid Responses history item", harness.ErrModelResponseInvalid)
					}
					switch item.Type {
					case "reasoning":
						items = append(items, entry)
					case "message":
						if unchanged {
							items = append(items, entry)
							messageWritten = true
						} else if !messageWritten && message.Content != "" {
							items = append(items, responsesAssistantText(message.Content))
							messageWritten = true
						}
					case "function_call":
						call, exists := calls[item.CallID]
						if !exists {
							continue
						} // Host reduction removed this call.
						delete(calls, item.CallID)
						if call.Function.Name == item.Name && call.Function.Arguments == item.Arguments {
							items = append(items, entry)
						} else {
							var edited map[string]any
							if err := json.Unmarshal(entry, &edited); err != nil {
								return nil, err
							}
							edited["name"], edited["arguments"] = call.Function.Name, call.Function.Arguments
							items = append(items, edited)
						}
					case "image_generation_call":
						// Requests use store:false. Even with result bytes present,
						// replaying this provider ID causes an upstream lookup/404.
						// Restore the session asset as explicit image context at
						// its original position, including on the same endpoint.
						if imageIndex < len(images) {
							if item.ID == "" || item.Status != "completed" {
								return nil, fmt.Errorf("%w: invalid Responses image history", harness.ErrModelResponseInvalid)
							}
							var err error
							items, err = responsesAppendImageContext(items, images[imageIndex:imageIndex+1])
							if err != nil {
								return nil, err
							}
							imageIndex++
						}
					default:
						return nil, fmt.Errorf("%w: unsupported Responses history item", harness.ErrModelResponseInvalid)
					}
				}
				if !messageWritten && message.Content != "" {
					items = append(items, responsesAssistantText(message.Content))
				}
				for _, call := range message.ToolCalls {
					if _, exists := calls[call.ID]; exists {
						items = append(items, responsesFunctionCall(call))
					}
				}
				var err error
				items, err = responsesAppendImageContext(items, images[imageIndex:])
				if err != nil {
					return nil, err
				}
				continue
			}
			if message.Content != "" {
				items = append(items, responsesAssistantText(message.Content))
			}
			for _, call := range message.ToolCalls {
				items = append(items, responsesFunctionCall(call))
			}
			var err error
			items, err = responsesAppendImageContext(items, images)
			if err != nil {
				return nil, err
			}
			continue
		}
		content, err := responsesInputContent(message)
		if err != nil {
			return nil, err
		}
		switch message.Role {
		case schema.System, schema.User:
			items = append(items, map[string]any{"type": "message", "role": string(message.Role), "content": content})
		case schema.Tool:
			if message.ToolCallID == "" {
				return nil, fmt.Errorf("%w: missing Responses tool call id", harness.ErrInvalidInput)
			}
			var output any = message.Content
			if len(message.UserInputMultiContent) > 0 || len(message.MultiContent) > 0 {
				output = content
			}
			items = append(items, map[string]any{"type": "function_call_output", "call_id": message.ToolCallID, "output": output})
		default:
			return nil, fmt.Errorf("%w: unsupported Responses message role", harness.ErrInvalidInput)
		}
	}
	return items, nil
}

func responsesAssistantText(text string) map[string]any {
	return map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}}
}
func responsesFunctionCall(call schema.ToolCall) map[string]any {
	return map[string]any{"type": "function_call", "call_id": call.ID, "name": call.Function.Name, "arguments": call.Function.Arguments}
}

func responsesInputContent(message *schema.Message) ([]any, error) {
	result := []any{}
	parts := message.UserInputMultiContent
	if len(parts) == 0 && len(message.MultiContent) > 0 {
		for _, part := range message.MultiContent {
			switch part.Type {
			case schema.ChatMessagePartTypeText:
				parts = append(parts, schema.MessageInputPart{Type: part.Type, Text: part.Text})
			case schema.ChatMessagePartTypeImageURL:
				if part.ImageURL == nil {
					return nil, fmt.Errorf("%w: missing Responses image", harness.ErrInvalidInput)
				}
				parts = append(parts, schema.MessageInputPart{Type: part.Type, Image: &schema.MessageInputImage{MessagePartCommon: schema.MessagePartCommon{URL: &part.ImageURL.URL}, Detail: schema.ImageURLDetail(part.ImageURL.Detail)}})
			default:
				return nil, fmt.Errorf("%w: unsupported Responses media", harness.ErrInvalidInput)
			}
		}
	}
	if len(parts) == 0 {
		return []any{map[string]any{"type": "input_text", "text": message.Content}}, nil
	}
	for _, part := range parts {
		switch part.Type {
		case schema.ChatMessagePartTypeText:
			result = append(result, map[string]any{"type": "input_text", "text": part.Text})
		case schema.ChatMessagePartTypeImageURL:
			if part.Image == nil {
				return nil, fmt.Errorf("%w: missing Responses image", harness.ErrInvalidInput)
			}
			image := part.Image
			var address string
			if image.URL != nil {
				address = *image.URL
			} else if image.Base64Data != nil && image.MIMEType != "" {
				address = "data:" + image.MIMEType + ";base64," + *image.Base64Data
			}
			if address == "" {
				return nil, fmt.Errorf("%w: empty Responses image", harness.ErrInvalidInput)
			}
			detail := string(image.Detail)
			if detail == "" {
				detail = "auto"
			}
			result = append(result, map[string]any{"type": "input_image", "image_url": address, "detail": detail})
		default:
			return nil, fmt.Errorf("%w: unsupported Responses media", harness.ErrInvalidInput)
		}
	}
	return result, nil
}

func responsesOutput(response responses.Response, incomplete bool) (*schema.Message, error) {
	message := &schema.Message{Role: schema.Assistant, ResponseMeta: &schema.ResponseMeta{FinishReason: "stop", Usage: responsesUsage(response)}}
	if incomplete {
		if response.IncompleteDetails.Reason != "max_output_tokens" {
			return nil, fmt.Errorf("%w: Responses response incomplete", harness.ErrModelResponseInvalid)
		}
		message.ResponseMeta.FinishReason = "length"
	}
	var raw []json.RawMessage
	var text strings.Builder
	seen := map[string]bool{}
	seenImages := map[string]bool{}
	imageLimit := modelImageLimit{}
	for index, output := range response.Output {
		encoded := json.RawMessage(output.RawJSON())
		var item responsesItem
		if json.Unmarshal(encoded, &item) != nil {
			return nil, fmt.Errorf("%w: invalid Responses output item", harness.ErrModelResponseInvalid)
		}
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				switch part.Type {
				case "output_text":
					text.WriteString(part.Text)
				case "refusal":
					text.WriteString(part.Refusal)
				default:
					return nil, fmt.Errorf("%w: unsupported Responses output content", harness.ErrModelResponseInvalid)
				}
			}
		case "function_call":
			if !incomplete && (item.CallID == "" || item.Name == "" || !json.Valid([]byte(item.Arguments)) || seen[item.CallID]) {
				return nil, fmt.Errorf("%w: invalid Responses function call", harness.ErrModelResponseInvalid)
			}
			seen[item.CallID] = true
			message.ToolCalls = append(message.ToolCalls, schema.ToolCall{Index: &index, ID: item.CallID, Type: "function", Function: schema.FunctionCall{Name: item.Name, Arguments: item.Arguments}})
		case "reasoning": // Kept opaque for the next model call, never shown.
		case "image_generation_call":
			if item.Status == "failed" {
				code, detail := "image_generation_failed", "provider did not complete the image generation call"
				if item.Error != nil {
					code, detail = item.Error.Code, item.Error.Message
				}
				return nil, responsesImageFailure(code, detail)
			}
			if incomplete && (item.Status == "in_progress" || item.Status == "generating") {
				continue // Partial images are not durable generated output.
			}
			if item.ID == "" || seenImages[item.ID] || item.Status != "completed" || item.Result == "" {
				return nil, fmt.Errorf("%w: Responses image generation did not return a completed image", harness.ErrModelResponseInvalid)
			}
			seenImages[item.ID] = true
			part, err := responsesGeneratedImage(item, &imageLimit)
			if err != nil {
				return nil, err
			}
			message.AssistantGenMultiContent = append(message.AssistantGenMultiContent, part)
			// Never persist provider base64 in Extra alongside the asset. Retain
			// only identity/order so the next request can restore image context.
			encoded, _ = json.Marshal(map[string]any{"type": item.Type, "id": item.ID, "status": item.Status})
		default:
			return nil, fmt.Errorf("%w: unsupported Responses output item", harness.ErrModelResponseInvalid)
		}
		raw = append(raw, encoded)
	}
	if !incomplete && len(message.ToolCalls) > 0 {
		message.ResponseMeta.FinishReason = "tool_calls"
	}
	message.Content = text.String()
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	if len(encoded) > 16<<20 {
		return nil, fmt.Errorf("%w: Responses output exceeds limit", harness.ErrModelResponseInvalid)
	}
	message.Extra = map[string]any{responsesOutputKey: string(encoded), responsesTextKey: message.Content}
	return message, nil
}
