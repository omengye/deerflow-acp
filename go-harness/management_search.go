package deerflow

import (
	"context"

	"github.com/omengye/deerflow-acp/go-harness/internal/localhost"
	"github.com/omengye/deerflow-acp/go-harness/internal/websearch"
)

func (c *Client) managementSearch(ctx context.Context, r localhost.ManagementRequest) (any, error) {
	if c.search == nil {
		return nil, managementInvalid("search services are unavailable")
	}
	switch r.Operation {
	case "search.status":
		if len(r.Fields) != 1 {
			return nil, managementInvalid("search status accepts only operation")
		}
		return c.search.Status()
	case "search.select":
		var in struct {
			Provider string `json:"provider"`
		}
		if err := decodeManagement(r, &in, "provider"); err != nil {
			return nil, err
		}
		return c.search.Select(ctx, in.Provider)
	default:
		var in websearch.Update
		if err := decodeManagement(r, &in, "provider", "expected_revision", "base_url", "api_key", "clear_api_key", "https_proxy", "auth_header", "timeout", "make_default", "fetch_provider"); err != nil {
			return nil, err
		}
		return c.search.Update(ctx, in)
	}
}
