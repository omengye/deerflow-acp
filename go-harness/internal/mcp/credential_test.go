package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestMCPCredentialTargetAndDecodedArguments(t *testing.T) {
	g := &guardTool{endpoint: &endpoint{credentialEndpoint: "https://mcp.example", credentialIdentity: "binding-v1"}, info: []byte(`{"Name":"mcp_sample_login"}`)}
	const args = `{"url":"https://service.example/api","login":"{{credential:username}}","key":"\u007b{credential:key}}"}`
	scope, err := g.CredentialScope(args)
	if err != nil || scope.Service != "https://mcp.example → https://service.example" {
		t.Fatal("wrong target", scope, err)
	}
	p := harness.PermissionRequest{ToolName: "mcp_sample_login", Arguments: json.RawMessage(args)}
	if !harness.ExplicitSecretApproval(p) {
		t.Fatal("unbound MCP credential was accepted as a scoped read")
	}
	scopeForHost := harness.ServiceCredentialScope(scope)
	p.CredentialScope = &scopeForHost
	if harness.ExplicitSecretApproval(p) {
		t.Fatal("scoped MCP credential read requested an extra confirmation")
	}
	changed, err := g.CredentialScope(`{"url":"https://different.example","key":"{{credential:key}}"}`)
	if err != nil || changed == scope {
		t.Fatal("target scope reused")
	}
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	v, err := secrets.Open(context.Background(), db.DB(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	prompts := 0
	ctx := secrets.WithCredentialPrompt(secrets.WithCredentialGrant(secrets.WithVault(context.Background(), v)), func(context.Context, secrets.CredentialScope, secrets.CredentialFields) (secrets.CredentialPair, error) {
		prompts++
		return secrets.CredentialPair{Username: []byte("demo"), Key: []byte("example-key")}, nil
	})
	for i := 0; i < 2; i++ {
		out, mask, err := secrets.CredentialArguments(ctx, scope, args)
		var decoded map[string]string
		if err != nil || json.Unmarshal([]byte(out), &decoded) != nil || decoded["login"] != "demo" || decoded["key"] != "example-key" || mask.Mask("demo") == "demo" || prompts != 1 {
			t.Fatal("MCP dispatch mapping", err)
		}
	}
	for _, bad := range []string{`{"{{credential:key}}":1}`, `{"value":"{{credential:incorrect}}"}`} {
		if _, _, err := secrets.CredentialArguments(ctx, changed, bad); err == nil || prompts != 1 {
			t.Fatal("invalid MCP credential requested input")
		}
	}
}
