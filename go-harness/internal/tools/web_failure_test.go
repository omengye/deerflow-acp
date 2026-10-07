package tools

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestWebFailuresRetainSanitizedClassification(t *testing.T) {
	service, err := newWebService(harness.BuiltinToolConfig{Name: "web_fetch"})
	if err != nil {
		t.Fatal(err)
	}
	defer service.transport.CloseIdleConnections()
	for _, tc := range []struct {
		status int
		code   string
	}{{403, "permission"}, {429, "rate_limit"}, {503, "network"}} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "private-server-details", tc.status) }))
		_, _, _, err := service.get(context.Background(), server.URL, nil)
		server.Close()
		var typed *harness.ToolFailureError
		if !errors.As(err, &typed) || typed.Code != tc.code {
			t.Fatalf("status=%d err=%v", tc.status, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _, err = service.get(ctx, "http://127.0.0.1:1", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost", err)
	}
}
