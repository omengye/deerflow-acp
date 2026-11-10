package eino

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	chatapi "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstream"
	openaisdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/responses"
)

// Responses uses the same Eino agent/tool loop, with the official SDK owning
// HTTP and SSE decoding. All retries remain at the tracked model boundary.
type responsesModel struct {
	client openaisdk.Client
	name   string
	origin string
	tools  []map[string]any
}

type responsesTuning struct {
	effort      string
	extraFields map[string]any
}

func withResponsesTuning(effort string, extraFields map[string]any) model.Option {
	return model.WrapImplSpecificOptFn(func(o *responsesTuning) {
		o.effort, o.extraFields = effort, extraFields
	})
}

func newResponsesModel(_ context.Context, cfg Config, name string, client *http.Client) (model.ToolCallingChatModel, error) {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	opts := []option.RequestOption{option.WithAPIKey(cfg.APIKey), option.WithMaxRetries(0), option.WithBaseURL(strings.TrimRight(baseURL, "/") + "/")}
	if client != nil {
		opts = append(opts, option.WithHTTPClient(client))
	}
	return &responsesModel{client: openaisdk.NewClient(opts...), name: name, origin: fmt.Sprintf("%x", sha256.Sum256([]byte(strings.TrimRight(baseURL, "/"))))}, nil
}

func (m *responsesModel) GetType() string          { return "OpenAIResponses" }
func (m *responsesModel) IsCallbacksEnabled() bool { return false }

func (m *responsesModel) WithTools(tools []*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	converted, err := responsesTools(tools)
	if err != nil {
		return nil, err
	}
	copy := *m
	copy.tools = converted
	return &copy, nil
}

func responsesTools(tools []*schema.ToolInfo) ([]map[string]any, error) {
	result := make([]map[string]any, 0, len(tools))
	seen := map[string]bool{}
	for _, tool := range tools {
		if tool == nil || tool.Name == "" || seen[tool.Name] {
			return nil, fmt.Errorf("%w: invalid Responses function definition", harness.ErrInvalidInput)
		}
		seen[tool.Name] = true
		var parameters any = map[string]any{"type": "object", "properties": map[string]any{}}
		if tool.ParamsOneOf != nil {
			value, err := tool.ToJSONSchema()
			if err != nil {
				return nil, err
			}
			// Snapshot schemas so WithTools never retains mutable caller objects.
			encoded, err := json.Marshal(value)
			if err != nil {
				return nil, err
			}
			if err = json.Unmarshal(encoded, &parameters); err != nil {
				return nil, err
			}
		}
		// Preserve existing optional parameters; Responses otherwise defaults
		// function schemas to strict mode, which changes their required fields.
		result = append(result, map[string]any{"type": "function", "name": tool.Name, "description": tool.Desc, "parameters": parameters, "strict": false})
	}
	return result, nil
}

func (m *responsesModel) request(input []*schema.Message, opts ...model.Option) (responses.ResponseNewParams, error) {
	common := model.GetCommonOptions(nil, opts...)
	tuning := model.GetImplSpecificOptions(&responsesTuning{}, opts...)
	invalid := func(message string) (responses.ResponseNewParams, error) {
		return responses.ResponseNewParams{}, fmt.Errorf("%w: %s", harness.ErrInvalidInput, message)
	}
	if len(common.Stop) > 0 {
		return invalid("Responses does not support stop sequences")
	}
	if len(tuning.extraFields) > 0 {
		return invalid("Responses thinking must use reasoning_effort")
	}
	if len(common.DeferredTools) > 0 || common.ToolSearchTool != nil || common.AgenticToolChoice != nil {
		return invalid("Responses chat adapter does not support deferred or agentic tools")
	}
	items, err := responsesInputForOrigin(input, m.origin)
	if err != nil {
		return responses.ResponseNewParams{}, err
	}
	name := m.name
	if common.Model != nil {
		name = *common.Model
	}
	body := map[string]any{"model": name, "input": items, "store": false, "include": []string{"reasoning.encrypted_content"}}
	if common.MaxTokens != nil {
		if *common.MaxTokens <= 0 {
			return invalid("Responses output allowance must be positive")
		}
		body["max_output_tokens"] = *common.MaxTokens
	}
	if tuning.effort != "" {
		body["reasoning"] = map[string]any{"effort": tuning.effort}
	}
	if common.Temperature != nil {
		body["temperature"] = *common.Temperature
	}
	if common.TopP != nil {
		body["top_p"] = *common.TopP
	}
	tools := m.tools
	if common.Tools != nil {
		tools, err = responsesTools(common.Tools)
		if err != nil {
			return responses.ResponseNewParams{}, err
		}
	}
	if len(common.AllowedToolNames) > 0 {
		allowed := map[string]bool{}
		for _, name := range common.AllowedToolNames {
			allowed[name] = true
		}
		var selected []map[string]any
		for _, tool := range tools {
			if allowed[tool["name"].(string)] {
				selected = append(selected, tool)
				delete(allowed, tool["name"].(string))
			}
		}
		if len(allowed) > 0 {
			return invalid("Responses allowed tool is not bound")
		}
		tools = selected
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	if common.ToolChoice != nil {
		switch *common.ToolChoice {
		case schema.ToolChoiceAllowed:
			body["tool_choice"] = "auto"
		case schema.ToolChoiceForbidden:
			body["tool_choice"] = "none"
		case schema.ToolChoiceForced:
			if len(tools) == 0 {
				return invalid("Responses cannot force an empty tool list")
			}
			body["tool_choice"] = "required"
		default:
			return invalid("unsupported Responses tool choice")
		}
	}
	return param.Override[responses.ResponseNewParams](body), nil
}

func (m *responsesModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	return modelstream.Generate(ctx, m, input, opts...)
}

func (m *responsesModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	request, err := m.request(input, opts...)
	if err != nil {
		return nil, err
	}
	work, cancel := context.WithCancel(ctx)
	stream := m.requestStream(work, request)
	if err = stream.Err(); err != nil {
		cancel()
		_ = stream.Close()
		return nil, responsesError(err)
	}
	reader, writer := schema.Pipe[*schema.Message](1)
	type received struct {
		event responses.ResponseStreamEventUnion
		err   error
	}
	events := make(chan received, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(events)
		defer stream.Close()
		for stream.Next() {
			select {
			case events <- received{event: stream.Current()}:
			case <-work.Done():
				return
			}
		}
		if err := stream.Err(); err != nil {
			select {
			case events <- received{err: err}:
			case <-work.Done():
			}
		}
	}()
	go func() {
		defer writer.Close()
		defer func() { cancel(); <-done }()
		// Eino has no reader-close callback. A filtered pulse detects a reader
		// closed during an idle SSE connection, then cancels and joins I/O.
		pulse := time.NewTicker(250 * time.Millisecond)
		defer pulse.Stop()
		var text strings.Builder
		terminal := false
		fail := func(err error) { writer.Send(nil, err) }
		for {
			var event responses.ResponseStreamEventUnion
			select {
			case <-pulse.C:
				if writer.Send(nil, nil) {
					return
				}
				continue
			case <-work.Done():
				fail(work.Err())
				return
			case next, ok := <-events:
				if !ok {
					if work.Err() != nil {
						fail(work.Err())
						return
					}
					if !terminal {
						fail(fmt.Errorf("%w: Responses stream ended without a terminal event", harness.ErrModelResponseInvalid))
					}
					return
				}
				if next.err != nil {
					fail(responsesError(next.err))
					return
				}
				event = next.event
			}
			if terminal {
				fail(fmt.Errorf("%w: Responses event after terminal response", harness.ErrModelResponseInvalid))
				return
			}
			switch event.Type {
			case "response.output_text.delta", "response.refusal.delta":
				text.WriteString(event.Delta)
				if text.Len() > 16<<20 {
					fail(fmt.Errorf("%w: Responses text exceeds limit", harness.ErrModelResponseInvalid))
					return
				}
				if writer.Send(&schema.Message{Role: schema.Assistant, Content: event.Delta}, nil) {
					return
				}
			case "response.reasoning_summary_text.delta":
				// Only provider-published summaries are visible; opaque reasoning
				// and reasoning_text events never enter user-facing text.
				if writer.Send(&schema.Message{Role: schema.Assistant, ReasoningContent: event.Delta}, nil) {
					return
				}
			case "response.completed", "response.incomplete":
				terminal = true
				message, err := responsesOutput(event.Response, event.Type == "response.incomplete")
				if err != nil {
					fail(err)
					return
				}
				if !strings.HasPrefix(message.Content, text.String()) {
					fail(fmt.Errorf("%w: Responses text changed at completion", harness.ErrModelResponseInvalid))
					return
				}
				message.Content = strings.TrimPrefix(message.Content, text.String())
				message.Extra[responsesOriginKey] = m.origin
				if writer.Send(message, nil) {
					return
				}
			case "response.failed":
				writer.Send(&schema.Message{Role: schema.Assistant, ResponseMeta: &schema.ResponseMeta{Usage: responsesUsage(event.Response)}}, nil)
				fail(responsesProviderError(string(event.Response.Error.Code), event.Response.Error.Message))
				return
			case "error":
				fail(responsesProviderError(event.Code, event.Message))
				return
			}
		}
	}()
	return schema.StreamReaderWithConvert(reader, func(message *schema.Message) (*schema.Message, error) {
		if message == nil {
			return nil, schema.ErrNoValue
		}
		return message, nil
	}), nil
}

func responsesUsage(response responses.Response) *schema.TokenUsage {
	if !response.JSON.Usage.Valid() {
		return nil
	}
	usage := response.Usage
	return &schema.TokenUsage{PromptTokens: int(usage.InputTokens), CompletionTokens: int(usage.OutputTokens), TotalTokens: int(usage.TotalTokens), PromptTokenDetails: schema.PromptTokenDetails{CachedTokens: int(usage.InputTokensDetails.CachedTokens)}, CompletionTokensDetails: schema.CompletionTokensDetails{ReasoningTokens: int(usage.OutputTokensDetails.ReasoningTokens)}}
}

// Translate SDK errors into the existing typed provider boundary so rate-limit,
// context and request classifiers retain their current behavior. Never retain
// the SDK's HTTP request object (which contains credentials) in this error.
func responsesError(err error) error {
	var api *openaisdk.Error
	if errors.As(err, &api) {
		return &chatapi.APIError{HTTPStatusCode: api.StatusCode, HTTPStatus: http.StatusText(api.StatusCode), Code: api.Code, Type: api.Type, Message: api.Message, Param: &api.Param}
	}
	var event *ssestream.StreamError
	if errors.As(err, &event) {
		var body struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(event.Event.Data, &body) == nil {
			return responsesProviderError(body.Error.Code, body.Error.Message)
		}
		return fmt.Errorf("%w: invalid Responses error event", harness.ErrModelResponseInvalid)
	}
	return err
}

func responsesProviderError(code, message string) error {
	status := http.StatusInternalServerError
	switch code {
	case "rate_limit_exceeded":
		status = http.StatusTooManyRequests
	case "context_length_exceeded", "invalid_request_error":
		status = http.StatusBadRequest
	}
	return &chatapi.APIError{HTTPStatusCode: status, HTTPStatus: http.StatusText(status), Code: code, Message: message}
}
