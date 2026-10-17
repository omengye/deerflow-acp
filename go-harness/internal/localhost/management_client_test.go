package localhost

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagementClientAuthenticatedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	_, ep := startHost(t, Config{RuntimeDir: dir, Manage: func(_ context.Context, r ManagementRequest) (any, error) {
		return map[string]string{"operation": r.Operation}, nil
	}})
	data, err := ManagementCall(context.Background(), dir, json.RawMessage(`{"operation":"schedule.list"}`))
	if err != nil || !strings.Contains(string(data), `"ok":true`) || strings.Contains(string(data), ep.Token) {
		t.Fatal(string(data), err)
	}
	ep.Host = "example.com"
	raw, _ := json.Marshal(ep)
	if err = os.WriteFile(filepath.Join(dir, EndpointFilename), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = ManagementCall(context.Background(), dir, json.RawMessage(`{"operation":"schedule.list"}`)); err == nil {
		t.Fatal("accepted remote endpoint")
	}
}
