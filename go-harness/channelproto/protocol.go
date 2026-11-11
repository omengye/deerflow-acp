// Package channelproto defines the bounded stdio protocol for trusted channel plugins.
// Plugins transport messages; only the host admits and executes agent work.
package channelproto

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
)

const Version = 1
const MaxFrame = 4 << 20

type Field struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Secret   bool   `json:"secret,omitempty"`
	Required bool   `json:"required,omitempty"`
	Login    bool   `json:"login,omitempty"`
}
type Descriptor struct {
	ID                   string   `json:"id"`
	Name                 string   `json:"name"`
	Version              string   `json:"version"`
	Protocol             int      `json:"protocol"`
	Login                []string `json:"login"`
	Messages             []string `json:"messages"`
	Fields               []Field  `json:"fields"`
	RequiresReplyContext bool     `json:"requires_reply_context,omitempty"`
	// IndependentPoll permits polling in a second process using explicit auth
	// and the host's committed cursor, without process-local login state.
	IndependentPoll bool `json:"independent_poll,omitempty"`
	// IndependentFeedback advertises stateless progress/typing methods that the
	// host may call in a separate, disposable process. Optional feedback must
	// never share the final answer's transport or delivery state.
	IndependentFeedback bool `json:"independent_feedback,omitempty"`
}
type Auth struct {
	Account string          `json:"account"`
	Owner   string          `json:"owner"`
	Secret  json.RawMessage `json:"secret"`
}
type Login struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	QR        string `json:"qr,omitempty"`
	ExpiresIn int    `json:"expires_in,omitempty"`
	Auth      *Auth  `json:"auth,omitempty"`
}
type Message struct {
	ID           string `json:"id"`
	Conversation string `json:"conversation"`
	Sender       string `json:"sender"`
	Context      string `json:"context"`
	Text         string `json:"text"`
	QuotedText   string `json:"quoted_text,omitempty"`
	Unsupported  bool   `json:"unsupported,omitempty"`
	Group        bool   `json:"group,omitempty"`
}
type Batch struct {
	Messages []Message `json:"messages"`
	Cursor   string    `json:"cursor"`
}
type Poll struct {
	Auth    Auth              `json:"auth"`
	Cursor  string            `json:"cursor"`
	Options map[string]string `json:"options,omitempty"`
}
type Send struct {
	Auth    Auth              `json:"auth"`
	Message Message           `json:"message"`
	Key     string            `json:"key"`
	Text    string            `json:"text"`
	Options map[string]string `json:"options,omitempty"`
}

// ToolProgress contains host-authored labels and lifecycle state only. Raw
// arguments, tool output, errors and model-authored descriptions do not belong here.
type ToolProgress struct {
	RunID      string `json:"run_id"`
	ToolCallID string `json:"tool_call_id"`
	ToolName   string `json:"tool_name"`
	Phase      string `json:"phase"` // start or end
	Status     string `json:"status,omitempty"`
}

type Progress struct {
	Auth    Auth         `json:"auth"`
	Message Message      `json:"message"`
	Key     string       `json:"key"`
	Tool    ToolProgress `json:"tool"`
}

// The host keeps the opaque typing ticket in memory for one execution only.
// Supplying it explicitly lets a fresh feedback process cancel typing as well.
type Typing struct {
	Auth    Auth    `json:"auth"`
	Message Message `json:"message"`
	Ticket  string  `json:"ticket,omitempty"`
	Active  bool    `json:"active"`
}

type TypingResult struct {
	Sent   bool   `json:"sent"`
	Ticket string `json:"ticket,omitempty"`
}
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      uint64          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      uint64          `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

var ErrAuthExpired = errors.New("channel authentication expired")

type Handler interface {
	Handle(context.Context, string, json.RawMessage) (any, error)
}

// Serve never writes diagnostic output or provider error bodies to stdout.
func Serve(ctx context.Context, in io.Reader, out io.Writer, handler Handler) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4096), MaxFrame)
	encoder := json.NewEncoder(out)
	for scanner.Scan() {
		var request Request
		if json.Unmarshal(scanner.Bytes(), &request) != nil || request.JSONRPC != "2.0" || request.ID == 0 {
			return errors.New("invalid channel request")
		}
		value, err := handler.Handle(ctx, request.Method, request.Params)
		response := Response{JSONRPC: "2.0", ID: request.ID}
		if err != nil {
			response.Error = &Error{Code: -32000, Message: "channel operation failed"}
			if errors.Is(err, ErrAuthExpired) {
				response.Error.Code = -32001
				response.Error.Message = "authentication expired"
			}
		} else {
			response.Result, err = json.Marshal(value)
			if err != nil || len(response.Result) > MaxFrame-256 {
				return errors.New("channel result exceeds protocol bounds")
			}
		}
		if err = encoder.Encode(response); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return scanner.Err()
}

func Decode(raw json.RawMessage, value any) error {
	if len(raw) > MaxFrame {
		return errors.New("oversized channel request")
	}
	return json.Unmarshal(raw, value)
}
