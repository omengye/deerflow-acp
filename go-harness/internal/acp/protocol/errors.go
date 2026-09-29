// Package protocol implements the bounded, bidirectional ACP stdio transport.
// ACP uses newline-delimited UTF-8 JSON-RPC 2.0, not LSP Content-Length framing.
package protocol

import (
	"errors"
	"fmt"
)

const (
	ParseError     = -32700
	InvalidRequest = -32600
	MethodNotFound = -32601
	InvalidParams  = -32602
	InternalError  = -32603
	ServerBusy     = -32000
)

var (
	ErrClosed         = errors.New("ACP connection closed")
	ErrAlreadyServing = errors.New("ACP peer can only be served once")
	ErrFrameTooLarge  = errors.New("ACP frame exceeds configured limit")
	ErrTruncatedFrame = errors.New("ACP connection ended within a frame")
	ErrBackpressure   = errors.New("ACP inbound queue capacity exceeded")
	ErrPendingLimit   = errors.New("ACP pending request limit exceeded")
)

// Error is a JSON-RPC error. Data must be JSON encodable and safe for the client.
// Ordinary Go errors returned by handlers become generic internal errors, so
// filesystem paths, model credentials, and other diagnostic data are not leaked.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("JSON-RPC %d: %s", e.Code, e.Message) }

func asRPCError(err error) *Error {
	var rpc *Error
	if errors.As(err, &rpc) {
		return rpc
	}
	return &Error{Code: InternalError, Message: "Internal error"}
}
