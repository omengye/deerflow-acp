package eino

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type toolMiddleware struct {
	adk.BaseChatModelAgentMiddleware
	sink        *eventSink
	permissions harness.PermissionHandler
	protected   map[string]bool
	io          *runIO
	budget      *runBudget
}

func (m *toolMiddleware) start(ctx context.Context, tc *adk.ToolContext, args string) error {
	arguments := json.RawMessage(args)
	if !json.Valid(arguments) {
		return errors.New("tool arguments are not valid JSON")
	}
	if err := m.sink.emit(ctx, harness.RunEvent{Kind: "tool_start", ToolCallID: tc.CallID, ToolName: tc.Name, Status: "pending", Arguments: append(json.RawMessage(nil), arguments...)}); err != nil {
		return err
	}
	if err := m.budget.tool(); err != nil {
		return err
	}
	if m.protected[tc.Name] {
		if m.permissions == nil {
			return harness.ErrPermissionDenied
		}
		decision, err := m.permissions(ctx, harness.PermissionRequest{
			ID: m.sink.request.RunID + "/" + tc.CallID, SessionID: m.sink.request.Session.ID,
			RunID: m.sink.request.RunID, ToolCallID: tc.CallID, ToolName: tc.Name, Arguments: append(json.RawMessage(nil), arguments...),
		})
		if err != nil {
			return err
		}
		switch decision {
		case harness.AllowOnce, harness.AllowAlways:
		case harness.PermissionCancelled:
			return context.Canceled
		default:
			return harness.ErrPermissionDenied
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return m.sink.emit(ctx, harness.RunEvent{Kind: "tool_update", ToolCallID: tc.CallID, ToolName: tc.Name, Status: "in_progress"})
}

func (m *toolMiddleware) finish(ctx context.Context, tc *adk.ToolContext, content []harness.Content, err error) error {
	status := "completed"
	if err != nil {
		status = "failed"
		content = []harness.Content{{Type: "text", Text: err.Error()}}
	}
	// Terminal cards and their durable events must survive cancellation of the
	// actual tool operation. The run still joins this callback before returning.
	return m.sink.emit(context.WithoutCancel(ctx), harness.RunEvent{Kind: "tool_end", ToolCallID: tc.CallID, ToolName: tc.Name, Status: status, Content: content})
}

func (m *toolMiddleware) WrapInvokableToolCall(_ context.Context, next adk.InvokableToolCallEndpoint, tc *adk.ToolContext) (adk.InvokableToolCallEndpoint, error) {
	return func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
		ctx, finish, err := m.io.begin(ctx)
		if err != nil {
			return "", err
		}
		defer finish()
		if err := m.start(ctx, tc, args); err != nil {
			if emitErr := m.finish(ctx, tc, nil, err); emitErr != nil {
				return "", emitErr
			}
			if errors.Is(err, harness.ErrPermissionDenied) {
				return "Tool permission denied; no operation was performed.", nil
			}
			return "", err
		}
		output, err := next(ctx, args, opts...)
		if emitErr := m.finish(ctx, tc, []harness.Content{{Type: "text", Text: output}}, err); emitErr != nil {
			return "", emitErr
		}
		return output, err
	}, nil
}

func (m *toolMiddleware) WrapStreamableToolCall(_ context.Context, next adk.StreamableToolCallEndpoint, tc *adk.ToolContext) (adk.StreamableToolCallEndpoint, error) {
	return func(ctx context.Context, args string, opts ...tool.Option) (*schema.StreamReader[string], error) {
		ctx, finish, err := m.io.begin(ctx)
		if err != nil {
			return nil, err
		}
		handedOff := false
		defer func() {
			if !handedOff {
				finish()
			}
		}()
		if err := m.start(ctx, tc, args); err != nil {
			if emitErr := m.finish(ctx, tc, nil, err); emitErr != nil {
				return nil, emitErr
			}
			if errors.Is(err, harness.ErrPermissionDenied) {
				return schema.StreamReaderFromArray([]string{"Tool permission denied; no operation was performed."}), nil
			}
			return nil, err
		}
		stream, err := next(ctx, args, opts...)
		if err != nil {
			_ = m.finish(ctx, tc, nil, err)
			return nil, err
		}
		handedOff = true
		return relayStream(stream, finish, func(chunk string) error {
			if err := m.sink.emit(ctx, harness.RunEvent{Kind: "tool_update", ToolCallID: tc.CallID, ToolName: tc.Name, Status: "in_progress", Content: []harness.Content{{Type: "text", Text: chunk}}}); err != nil {
				return err
			}
			return nil
		}, m.io.recordError), nil
	}, nil
}

func enhancedContent(output *schema.ToolResult) []harness.Content {
	if output == nil {
		return nil
	}
	var contents []harness.Content
	for _, part := range output.Parts {
		if part.Type == schema.ToolPartTypeText {
			contents = append(contents, harness.Content{Type: "text", Text: part.Text})
		}
		if part.Type == schema.ToolPartTypeImage && part.Image != nil {
			c := harness.Content{Type: "image", MimeType: part.Image.MIMEType}
			if part.Image.Base64Data != nil {
				c.Data = *part.Image.Base64Data
			}
			if part.Image.URL != nil {
				c.URI = *part.Image.URL
			}
			contents = append(contents, c)
		}
		if part.Type == schema.ToolPartTypeFile && part.File != nil && part.File.URL != nil {
			contents = append(contents, harness.Content{Type: "resource_link", URI: *part.File.URL, MimeType: part.File.MIMEType})
		}
	}
	return contents
}

func (m *toolMiddleware) WrapEnhancedInvokableToolCall(_ context.Context, next adk.EnhancedInvokableToolCallEndpoint, tc *adk.ToolContext) (adk.EnhancedInvokableToolCallEndpoint, error) {
	return func(ctx context.Context, args *schema.ToolArgument, opts ...tool.Option) (*schema.ToolResult, error) {
		ctx, finish, err := m.io.begin(ctx)
		if err != nil {
			return nil, err
		}
		defer finish()
		if args == nil {
			return nil, errors.New("nil tool arguments")
		}
		if err := m.start(ctx, tc, args.Text); err != nil {
			_ = m.finish(ctx, tc, nil, err)
			return nil, err
		}
		output, err := next(ctx, args, opts...)
		if emitErr := m.finish(ctx, tc, enhancedContent(output), err); emitErr != nil {
			return nil, emitErr
		}
		return output, err
	}, nil
}

func (m *toolMiddleware) WrapEnhancedStreamableToolCall(_ context.Context, next adk.EnhancedStreamableToolCallEndpoint, tc *adk.ToolContext) (adk.EnhancedStreamableToolCallEndpoint, error) {
	return func(ctx context.Context, args *schema.ToolArgument, opts ...tool.Option) (*schema.StreamReader[*schema.ToolResult], error) {
		ctx, finish, err := m.io.begin(ctx)
		if err != nil {
			return nil, err
		}
		handedOff := false
		defer func() {
			if !handedOff {
				finish()
			}
		}()
		if args == nil {
			return nil, errors.New("nil tool arguments")
		}
		if err := m.start(ctx, tc, args.Text); err != nil {
			_ = m.finish(ctx, tc, nil, err)
			return nil, err
		}
		stream, err := next(ctx, args, opts...)
		if err != nil {
			_ = m.finish(ctx, tc, nil, err)
			return nil, err
		}
		handedOff = true
		return relayStream(stream, finish, func(chunk *schema.ToolResult) error {
			if err := m.sink.emit(ctx, harness.RunEvent{Kind: "tool_update", ToolCallID: tc.CallID, ToolName: tc.Name, Status: "in_progress", Content: enhancedContent(chunk)}); err != nil {
				return err
			}
			return nil
		}, m.io.recordError), nil
	}, nil
}
