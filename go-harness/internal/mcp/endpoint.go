package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	official "github.com/cloudwego/eino-ext/components/tool/mcp/officialmcp"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
)

func init() {
	// guardTool.Info decodes nested JSON in ToolInfo.Extra (including
	// mcp.annotations). Eino checkpoints these values through interface fields.
	gob.Register(map[string]any{})
	gob.Register([]any{})
}

type endpoint struct {
	binding                                *binding
	policy                                 harness.MCPPolicy
	credentialEndpoint, credentialIdentity string
	name                                   string
	client                                 *sdk.ClientSession
	transport                              *trackedTransport
	http                                   *http.Transport
	ctx                                    context.Context
	cancel                                 context.CancelFunc
	secrets                                []string
	closeOnce                              sync.Once
	closed                                 chan struct{}
	closeErr                               error
	initialToolCount                       int
	initialToolBytes                       int
	programKey                             string
	// Desktop discovery is published as one immutable list per connection.
	desktopID    string
	desktopTools atomic.Pointer[[]tool.BaseTool]
	listChanged  chan struct{}
}

// Track the raw connection too: SDK initialization errors must not leak a
// transport even when the SDK returns no ClientSession (e.g. version rejection).
type trackedTransport struct {
	sdk.Transport
	conn sdk.Connection
}

func (t *trackedTransport) Connect(ctx context.Context) (sdk.Connection, error) {
	c, err := t.Transport.Connect(ctx)
	if err == nil {
		t.conn = c
	}
	return c, err
}

func (m *Manager) connect(ctx context.Context, b *binding, cwd string, cfg harness.MCPServer, desktopID string) (*endpoint, error) {
	parent := b.ctx
	if desktopID != "" {
		parent = ctx // this program can retire independently of the binding
	}
	lifetime, cancel := context.WithCancel(parent)
	e := &endpoint{binding: b, policy: m.policy, name: cfg.Name, ctx: lifetime, cancel: cancel, closed: make(chan struct{}), secrets: credentialValues(cfg)}
	var infoBytes []byte
	if cfg.Transport == "stdio" {
		if real, file, err := executable(cfg.Command); err == nil {
			identity := desktopServer{config: cfg, target: real}
			e.programKey = identity.catalogKey(cwd, file)
		}
		infoBytes, _ = json.Marshal(cfg)
		infoBytes = append(infoBytes, []byte("\x00"+cwd)...)
		e.credentialEndpoint = "stdio:" + cfg.Name
	} else {
		e.credentialEndpoint, _ = secrets.ServiceOrigin(cfg.URL)
		u, _ := url.Parse(cfg.URL)
		infoBytes = []byte(cfg.Transport + "\x00" + e.credentialEndpoint + "\x00" + u.EscapedPath())
	}
	e.credentialIdentity = fmt.Sprintf("%x", sha256.Sum256(infoBytes))[:24]
	clear(infoBytes)
	// The initialization context stays alive for SSE/JSON-RPC after the setup
	// deadline is stopped. Cancelling a temporary WithTimeout after Connect would
	// tear down a successfully established SSE stream.
	stopCaller := context.AfterFunc(ctx, cancel)
	defer stopCaller()
	timer := time.AfterFunc(m.policy.ConnectTimeout, cancel)
	defer timer.Stop()
	var transport sdk.Transport
	if cfg.Transport == "stdio" {
		transport = &commandTransport{command: cfg.Command, args: cfg.Args, cwd: cwd, env: processEnv(cfg.Env), grace: max(time.Millisecond, m.policy.CloseTimeout/4)}
	} else {
		u, _ := url.Parse(cfg.URL)
		e.http = &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: m.policy.ConnectTimeout, ExpectContinueTimeout: time.Second}
		e.http.ResponseHeaderTimeout = m.policy.ConnectTimeout
		client := &http.Client{Transport: &credentialTransport{base: e.http, endpoint: u, headers: cfg.Headers, lifetime: lifetime, closeTimeout: max(time.Millisecond, m.policy.CloseTimeout/2)}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("MCP redirects are disabled") }}
		if cfg.Transport == "http" {
			transport = &sdk.StreamableClientTransport{Endpoint: cfg.URL, HTTPClient: client}
		} else {
			transport = &sdk.SSEClientTransport{Endpoint: cfg.URL, HTTPClient: client}
		}
	}
	e.transport = &trackedTransport{Transport: transport}
	options := &sdk.ClientOptions{Capabilities: &sdk.ClientCapabilities{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	e.desktopID = desktopID
	if desktopID != "" {
		e.listChanged = make(chan struct{}, 1)
		options.ToolListChangedHandler = func(context.Context, *sdk.ToolListChangedRequest) {
			select {
			case e.listChanged <- struct{}{}:
			default:
			}
		}
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "deerflow-go", Version: "0.1.0"}, options)
	var err error
	phase := "启动或协议握手"
	e.client, err = client.Connect(lifetime, e.transport, nil)
	if err == nil {
		phase = "读取工具列表"
		var discovered []tool.BaseTool
		discovered, err = e.tools(ctx)
		e.initialToolCount = len(discovered)
		for _, item := range discovered {
			e.initialToolBytes += len(item.(*guardTool).info)
		}
		if err == nil && desktopID != "" {
			e.desktopTools.Store(&discovered)
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return e, ctx.Err()
		}
		if b.ctx.Err() != nil {
			return e, ErrClosed
		}
		return e, &harness.MCPSetupError{Server: cfg.Name, Phase: phase}
	}
	if !timer.Stop() {
		return e, context.DeadlineExceeded
	}
	if !stopCaller() || ctx.Err() != nil {
		return e, ctx.Err()
	}
	if lifetime.Err() != nil {
		return e, ErrClosed
	}
	go func() {
		_ = e.client.Wait()
		e.cancel()
		if desktopID == "" {
			b.cancel()
		}
	}()
	return e, nil
}

type credentialTransport struct {
	base         http.RoundTripper
	endpoint     *url.URL
	headers      map[string]string
	lifetime     context.Context
	closeTimeout time.Duration
}

func (t *credentialTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.EqualFold(req.URL.Host, t.endpoint.Host) || req.URL.Scheme != t.endpoint.Scheme {
		return nil, errors.New("MCP endpoint origin changed")
	}
	// The SDK detaches streamable-HTTP background requests from Connect's
	// context. Tie their response bodies back to this endpoint's lifetime.
	op, cancel := context.WithCancel(req.Context())
	stop := context.AfterFunc(t.lifetime, cancel)
	if req.Method == http.MethodDelete {
		stop()
		cancel()
		// No session was assigned (or the server is stateless), so there is
		// no remote session to delete after a failed initialize.
		if req.Header.Get("Mcp-Session-Id") == "" {
			return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: http.NoBody, Request: req}, nil
		}
		op, cancel = context.WithTimeout(context.WithoutCancel(req.Context()), t.closeTimeout)
		stop = func() bool { return true }
	}
	copy := req.Clone(op)
	copy.Header = req.Header.Clone()
	for k, v := range t.headers {
		copy.Header.Set(k, v)
	}
	resp, err := t.base.RoundTrip(copy)
	if err != nil {
		stop()
		cancel()
		return nil, err
	}
	if req.Method == http.MethodDelete {
		// SDK v1.6.1 ignores the DELETE response body. Close it here.
		_ = resp.Body.Close()
		resp.Body = http.NoBody
		stop()
		cancel()
	} else {
		resp.Body = &lifetimeBody{ReadCloser: resp.Body, done: func() { stop(); cancel() }}
	}
	return resp, nil
}

type lifetimeBody struct {
	io.ReadCloser
	done func()
}

func (b *lifetimeBody) Close() error { err := b.ReadCloser.Close(); b.done(); return err }

func (e *endpoint) close() error {
	e.closeOnce.Do(func() {
		e.cancel()
		go func() {
			var err error
			if e.client != nil {
				err = e.client.Close()
			}
			// Join the owned raw connection explicitly too. Session errors must
			// not be mistaken for proof that a child process has been reaped.
			if e.transport != nil && e.transport.conn != nil {
				err = errors.Join(err, e.transport.conn.Close())
			}
			if e.http != nil {
				e.http.CloseIdleConnections()
			}
			if err != nil {
				e.closeErr = errors.New("MCP transport cleanup failed")
			}
			close(e.closed)
		}()
	})
	timer := time.NewTimer(e.policy.CloseTimeout)
	defer timer.Stop()
	select {
	case <-e.closed:
		return e.closeErr
	case <-timer.C:
		return errors.New("MCP transport cleanup timed out")
	}
}

func (e *endpoint) operation(ctx context.Context) (context.Context, func(), error) {
	if e.ctx.Err() != nil {
		return nil, nil, ErrClosed
	}
	op, cancel := context.WithTimeout(ctx, e.policy.CallTimeout)
	stop := context.AfterFunc(e.ctx, cancel)
	return op, func() { stop(); cancel() }, nil
}

type scopedClient struct {
	endpoint *endpoint
	listed   int
	bytes    int
}

func (c *scopedClient) ListTools(ctx context.Context, params *sdk.ListToolsParams) (*sdk.ListToolsResult, error) {
	op, done, err := c.endpoint.operation(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	result, err := c.endpoint.client.ListTools(op, params)
	if err != nil {
		return nil, safeOperationError(op, err, "tool discovery")
	}
	if result == nil {
		return nil, errors.New("MCP server returned no tool listing")
	}
	c.listed += len(result.Tools)
	if c.listed > c.endpoint.policy.MaxTools {
		return nil, errors.New("MCP tool count exceeds configured limit")
	}
	for _, t := range result.Tools {
		if t == nil || t.Name == "" || len(t.Name) > 256 {
			return nil, errors.New("MCP server returned an invalid tool")
		}
	}
	data, err := json.Marshal(result)
	c.bytes += len(data)
	if err != nil || c.bytes > maxCatalogBytes || containsCredentials(data, c.endpoint.secrets) {
		return nil, errors.New("MCP tool metadata is invalid or contains connection credentials")
	}
	return result, nil
}
func (c *scopedClient) CallTool(ctx context.Context, params *sdk.CallToolParams) (*sdk.CallToolResult, error) {
	op, done, err := c.endpoint.operation(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	// Never transparently retry a call whose external side effect is unknown.
	result, err := c.endpoint.client.CallTool(op, params)
	if err != nil {
		return nil, safeOperationError(op, err, "tool call")
	}
	// Inspect the original result before formatting/truncation can expose only
	// a prefix of a credential echoed by the server.
	if mask := secrets.CallCredentialOutput(ctx); mask != nil {
		for _, item := range result.Content {
			if text, ok := item.(*sdk.TextContent); ok {
				text.Text = mask.Mask(text.Text)
			}
		}
	}
	data, err := json.Marshal(result)
	if err != nil || containsCredentials(data, c.endpoint.secrets) {
		return nil, errors.New("MCP result is invalid or contains connection credentials")
	}
	return result, nil
}
func safeOperationError(ctx context.Context, err error, operation string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return fmt.Errorf("MCP %s failed", operation)
}

func (e *endpoint) tools(ctx context.Context) ([]tool.BaseTool, error) {
	errorAsError := false // A normal IsError response is terminal tool evidence.
	rawNames := make(map[string]string)
	result, err := official.GetTools(ctx, &official.Config{
		Cli: &scopedClient{endpoint: e}, ServerName: e.name, ListToolsMode: official.ListToolsAllPages, MaxToolPages: e.policy.MaxToolPages,
		MetadataMode: official.MetadataBasic, DescriptionPolicy: &official.DescriptionPolicy{MaxChars: 4096},
		ResultPolicy:            &official.ResultPolicy{MaxChars: e.policy.MaxResultChars, IncludeStructuredContent: true, ErrorAsError: &errorAsError},
		ToolCallResultHandlerV2: captureToolImages,
		ToolNameMapper: func(_ context.Context, in official.ToolNameMapperInput) (official.ToolNameMapperOutput, error) {
			name := toolName(in.ServerName, in.Tool.Name)
			if _, exists := rawNames[name]; exists {
				return official.ToolNameMapperOutput{}, errors.New("duplicate MCP tool")
			}
			rawNames[name] = in.Tool.Name
			return official.ToolNameMapperOutput{ExposedName: name}, nil
		},
	})
	if err != nil {
		return nil, safeOperationError(ctx, err, "tool discovery")
	}
	wrapped := make([]tool.BaseTool, 0, len(result))
	for _, raw := range result {
		info, err := raw.Info(ctx)
		if err != nil {
			return nil, errors.New("MCP tool metadata is invalid")
		}
		data, err := canonicalToolInfo(info)
		if err != nil {
			return nil, errors.New("MCP tool schema is invalid")
		}
		// Never expose connection credentials accidentally echoed by a server's
		// descriptions, schemas or metadata. Reject rather than change argument schemas.
		if containsCredentials(data, e.secrets) {
			return nil, errors.New("MCP tool metadata contains connection credentials")
		}
		invokable, ok := raw.(tool.InvokableTool)
		if !ok {
			return nil, errors.New("MCP tool is not invokable")
		}
		wrapped = append(wrapped, &guardTool{endpoint: e, raw: invokable, info: data, original: rawNames[info.Name]})
	}
	return wrapped, nil
}

// Eino schemas contain ordered maps populated from unordered MCP objects.
// Normalize object keys before storing metadata or hashing execution identity;
// preserve array order and exact JSON numbers so actual changes still differ.
func canonicalToolInfo(info *schema.ToolInfo) ([]byte, error) {
	data, err := json.Marshal(info)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func toolName(server, name string) string {
	h := sha256.Sum256([]byte(server + "\x00" + name))
	slug := func(s string) string {
		var b strings.Builder
		for _, c := range s {
			if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' {
				b.WriteRune(c)
			} else {
				b.WriteByte('_')
			}
			if b.Len() >= 18 {
				break
			}
		}
		return b.String()
	}
	return "mcp_" + slug(server) + "_" + slug(name) + "_" + hex.EncodeToString(h[:8])
}

type guardTool struct {
	endpoint *endpoint
	original string
	raw      tool.InvokableTool
	info     []byte
}

type toolImageCaptureKey struct{}

type toolImageCapture struct {
	images []*sdk.ImageContent
	failed bool
}

type toolResultError struct {
	cause  error
	result string
}

func (e *toolResultError) Error() string      { return e.cause.Error() }
func (e *toolResultError) Unwrap() error      { return e.cause }
func (e *toolResultError) ToolResult() string { return e.result }

// The official adapter marshals MCP content into a JSON string. Strip image
// data before that string is constructed; the enhanced Eino wrapper receives
// the original image blocks through this per-call context instead.
func captureToolImages(ctx context.Context, _ official.ToolCallInfo, result *sdk.CallToolResult) (*sdk.CallToolResult, error) {
	if capture, ok := ctx.Value(toolImageCaptureKey{}).(*toolImageCapture); ok {
		capture.failed = result.IsError
	}
	var images []*sdk.ImageContent
	content := make([]sdk.Content, 0, len(result.Content))
	var total int64
	for _, part := range result.Content {
		switch image := part.(type) {
		case *sdk.TextContent, *sdk.ResourceLink:
			content = append(content, part)
		case *sdk.ImageContent:
			total += int64(len(image.Data))
			if len(images) >= harness.MaxInputImagesPerTurn || int64(len(image.Data)) > harness.MaxInputImageBytes || total > harness.MaxInputImageTotalBytes {
				return nil, fmt.Errorf("%w: MCP image result exceeds media limits", harness.ErrInvalidInput)
			}
			images = append(images, image)
			content = append(content, &sdk.TextContent{Text: "[image content omitted from JSON text result]"})
		default:
			return nil, fmt.Errorf("%w: unsupported MCP binary content", harness.ErrInvalidInput)
		}
	}
	if len(images) == 0 {
		return result, nil
	}
	if capture, ok := ctx.Value(toolImageCaptureKey{}).(*toolImageCapture); ok {
		capture.images = images
	}
	copyResult := *result
	copyResult.Content = content
	return &copyResult, nil
}

// enhancedGuardTool preserves the official MCP adapter's discovery, argument
// mapping, call policy and text formatting while exposing actual image blocks
// through Eino's structured tool result interface.
type enhancedGuardTool struct{ *guardTool }

type enhancedResultError struct {
	cause  error
	result *schema.ToolResult
}

func (e *enhancedResultError) Error() string                          { return e.cause.Error() }
func (e *enhancedResultError) Unwrap() error                          { return e.cause }
func (e *enhancedResultError) EnhancedToolResult() *schema.ToolResult { return e.result }

func (t *enhancedGuardTool) InvokableRun(ctx context.Context, args *schema.ToolArgument, opts ...tool.Option) (*schema.ToolResult, error) {
	if args == nil {
		return nil, harness.MarkToolNotExecuted(fmt.Errorf("%w: missing MCP arguments", harness.ErrInvalidInput))
	}
	capture := &toolImageCapture{}
	textResult, err := t.guardTool.InvokableRun(context.WithValue(ctx, toolImageCaptureKey{}, capture), args.Text, opts...)
	var terminal *harness.ToolExecutionFailedError
	if err != nil && !errors.As(err, &terminal) {
		return nil, err
	}
	result := &schema.ToolResult{Parts: []schema.ToolOutputPart{{Type: schema.ToolPartTypeText, Text: textResult}}}
	for _, image := range capture.images {
		data := base64.StdEncoding.EncodeToString(image.Data)
		result.Parts = append(result.Parts, schema.ToolOutputPart{Type: schema.ToolPartTypeImage, Image: &schema.ToolOutputImage{MessagePartCommon: schema.MessagePartCommon{Base64Data: &data, MIMEType: image.MIMEType}}})
	}
	if err != nil {
		return result, &enhancedResultError{cause: err, result: result}
	}
	return result, nil
}

func (t *guardTool) Info(context.Context) (*schema.ToolInfo, error) {
	var info schema.ToolInfo
	err := json.Unmarshal(t.info, &info)
	return &info, err
}
func (t *guardTool) InvokableRun(ctx context.Context, args string, opts ...tool.Option) (string, error) {
	capture, ok := ctx.Value(toolImageCaptureKey{}).(*toolImageCapture)
	if !ok {
		capture = &toolImageCapture{}
		ctx = context.WithValue(ctx, toolImageCaptureKey{}, capture)
	}
	var output *secrets.CredentialOutput
	if harness.HasCredentialMarker(json.RawMessage(args)) {
		scope, err := t.CredentialScope(args)
		if err != nil {
			return "", harness.MarkToolNotExecuted(err)
		}
		args, output, err = secrets.CredentialArguments(ctx, scope, args)
		ctx = secrets.WithCredentialOutput(ctx, output)
		if err != nil {
			return "", harness.MarkToolNotExecuted(err)
		}
	}
	op, done, err := t.endpoint.operation(ctx)
	if err != nil {
		return "", harness.MarkToolNotExecuted(err)
	}
	defer done()
	result, err := t.raw.InvokableRun(op, args, opts...)
	if err != nil {
		return "", safeOperationError(op, err, "tool call")
	}
	if containsCredentials([]byte(result), t.endpoint.secrets) {
		return "", errors.New("MCP result contains connection credentials")
	}
	result = output.Mask(result)
	if capture.failed {
		return result, &toolResultError{cause: harness.MarkToolExecutionFailed(errors.New("MCP tool reported an application failure")), result: result}
	}
	return result, nil
}

func credentialValues(cfg harness.MCPServer) []string {
	var out []string
	for _, v := range cfg.Env {
		if v != "" {
			out = append(out, v)
		}
	}
	for _, v := range cfg.Headers {
		if v != "" {
			out = append(out, v)
			if strings.HasPrefix(v, "Bearer ") {
				out = append(out, strings.TrimPrefix(v, "Bearer "))
			}
		}
	}
	if u, err := url.Parse(cfg.URL); err == nil {
		for _, values := range u.Query() {
			for _, v := range values {
				if v != "" {
					out = append(out, v)
				}
			}
		}
	}
	return out
}
func containsCredentials(data []byte, secrets []string) bool {
	// Check decoded JSON string values too, so JSON escaping cannot bypass the guard.
	var v any
	if json.Unmarshal(data, &v) != nil {
		v = string(data)
	}
	var contains func(any) bool
	contains = func(v any) bool {
		switch x := v.(type) {
		case string:
			for _, secret := range secrets {
				if x == secret || (len(secret) >= 8 && strings.Contains(x, secret)) {
					return true
				}
			}
		case []any:
			for _, v := range x {
				if contains(v) {
					return true
				}
			}
		case map[string]any:
			for k, v := range x {
				if contains(k) || contains(v) {
					return true
				}
			}
		}
		return false
	}
	return contains(v)
}
