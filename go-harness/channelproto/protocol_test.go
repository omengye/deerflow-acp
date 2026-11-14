package channelproto

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type handler func(context.Context, string, json.RawMessage) (any, error)

func (h handler) Handle(c context.Context, m string, r json.RawMessage) (any, error) {
	return h(c, m, r)
}
func TestProtocolRejectsMalformedFramesAndRedactsErrors(t *testing.T) {
	for _, raw := range []string{"garbage\n", `{"jsonrpc":"1.0","id":1}` + "\n", `{"jsonrpc":"2.0","id":0}` + "\n", strings.Repeat("x", MaxFrame+1)} {
		if Serve(context.Background(), strings.NewReader(raw), &bytes.Buffer{}, handler(func(context.Context, string, json.RawMessage) (any, error) {
			t.Fatal("invalid request dispatched")
			return nil, nil
		})) == nil {
			t.Fatal("bad frame accepted")
		}
	}
	var out bytes.Buffer
	err := Serve(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"login"}`+"\n"), &out, handler(func(context.Context, string, json.RawMessage) (any, error) {
		return nil, errors.New("provider leaked secret")
	}))
	if err != nil || strings.Contains(out.String(), "secret") {
		t.Fatal("error leaked")
	}
}

func TestImageFailureWireCodeAndAuthPrecedence(t *testing.T) {
	for _, tc := range []struct {
		cause error
		code  int
	}{
		{errors.Join(ErrImageNotSent, errors.New("private snapshot or CDN URL")), -32002},
		{errors.Join(ErrImageNotSent, ErrAuthExpired), -32001},
		{errors.New("send acknowledgement lost"), -32000},
	} {
		var out bytes.Buffer
		err := Serve(t.Context(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"send"}`+"\n"), &out, handler(func(context.Context, string, json.RawMessage) (any, error) { return nil, tc.cause }))
		var response Response
		if err != nil || json.Unmarshal(out.Bytes(), &response) != nil || response.Error == nil || response.Error.Code != tc.code || strings.Contains(out.String(), "private") {
			t.Fatalf("wrong failure classification: %s, %v", out.String(), err)
		}
	}
}
