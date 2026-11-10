package eino

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstreamtest"
)

func TestModelProbesUseUnifiedProxyForBothProviders(t *testing.T) {
	for _, provider := range []string{"openai", "claude"} {
		t.Run(provider, func(t *testing.T) {
			var calls atomic.Int32
			proxy := httptest.NewServer(modelstreamtest.RequireStreaming(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Host != "model.invalid" {
					t.Errorf("unexpected model destination %s", r.URL.Host)
				}
				if provider == "openai" {
					_ = modelstreamtest.WriteJSON(w, `{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}`)
				} else {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"fixture\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
					fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\n")
					fmt.Fprint(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
				}
			}))
			defer proxy.Close()
			policy := harness.NetworkConfig{Proxy: harness.ProxyConfig{Enabled: true, URL: proxy.URL}}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := ProbeModelWithNetwork(ctx, provider, "fixture", "http://model.invalid", "fixture", policy); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 {
				t.Fatalf("model proxy calls=%d", calls.Load())
			}
		})
	}
}

func TestExecutionContractPinsUnifiedProxyWithoutCredentials(t *testing.T) {
	engine := &Engine{}
	original, err := engine.executionContract(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	engine.config.Network.Proxy.URL = "http://user:secret@proxy.test:8080"
	disabled, err := engine.executionContract(nil, nil)
	if err != nil || original != disabled {
		t.Fatal("disabled retained address changes legacy contract")
	}
	engine.config.Network.Proxy.Enabled = true
	enabled, err := engine.executionContract(nil, nil)
	if err != nil || original == enabled {
		t.Fatal("enabled network policy not pinned")
	}
	engine.config.Network.Proxy.NoProxy = []string{"internal.test"}
	bypass, err := engine.executionContract(nil, nil)
	if err != nil || enabled == bypass {
		t.Fatal("bypass changes not pinned")
	}
}
