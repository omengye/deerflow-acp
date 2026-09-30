package tools

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"go.yaml.in/yaml/v3"
)

// Explicit opt-in integration check; prints counts only, never API keys,
// provider headers or remote response content.
func TestConfiguredLiveReadOnlyWebTools(t *testing.T) {
	path := os.Getenv("DEERFLOW_TEST_TOOL_CONFIG")
	if path == "" {
		t.Skip("set DEERFLOW_TEST_TOOL_CONFIG for a live read-only network smoke")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config struct{ Tools []harness.BuiltinToolConfig }
	if yaml.Unmarshal(body, &config) != nil {
		t.Fatal("invalid config")
	}
	for _, c := range config.Tools {
		if c.Name != "web_search" && c.Name != "web_fetch" && c.Name != "image_search" {
			continue
		}
		t.Run(c.Name, func(t *testing.T) {
			item, cleanup, err := WebTools(c)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			args := `{"query":"CloudWeGo Eino"}`
			if c.Name == "web_fetch" {
				args = `{"url":"https://www.cloudwego.io/docs/eino/"}`
			}
			out, err := item.(tool.InvokableTool).InvokableRun(context.Background(), args)
			if err != nil {
				t.Fatal(err)
			}
			if c.Name == "web_fetch" {
				if len(out) < 50 {
					t.Fatal("empty page")
				}
				t.Logf("readable page returned %d bytes", len(out))
			} else {
				var data struct {
					Total int `json:"total_results"`
				}
				if json.Unmarshal([]byte(out), &data) != nil || data.Total < 1 {
					t.Fatal("no search results")
				}
				t.Logf("returned %d results", data.Total)
			}
		})
	}
}
