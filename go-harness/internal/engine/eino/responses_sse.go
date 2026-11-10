package eino

import (
	"bytes"
	"context"
	"net/http"

	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/responses"
)

// The pinned SDK dispatches blank lines/comment heartbeats with empty Data,
// then attempts json.Unmarshal on them. Filter these non-payload frames before
// its typed decoder. Nonempty malformed JSON, transport errors and terminal
// response validation remain unchanged. Never register a process-wide decoder.
type responsesSSEDecoder struct{ ssestream.Decoder }

func (d *responsesSSEDecoder) Next() bool {
	for d.Decoder.Next() {
		if len(bytes.TrimSpace(d.Decoder.Event().Data)) != 0 {
			return true
		}
	}
	return false
}

func (m *responsesModel) requestStream(ctx context.Context, request responses.ResponseNewParams) *ssestream.Stream[responses.ResponseStreamEventUnion] {
	var raw *http.Response
	// This is the same public SDK request path used by Responses.NewStreaming:
	// keep its authentication, proxy, HTTP error handling and disabled retries.
	err := m.client.Post(ctx, "responses", request, &raw, option.WithJSONSet("stream", true))
	decoder := ssestream.NewDecoder(raw)
	if decoder != nil {
		decoder = &responsesSSEDecoder{Decoder: decoder}
	}
	return ssestream.NewStream[responses.ResponseStreamEventUnion](decoder, err)
}
