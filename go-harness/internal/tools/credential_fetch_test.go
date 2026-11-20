package tools

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
)

func TestDesktopCredentialFetchPinsOriginAndRedactsBasic(t *testing.T) {
	const user = "demo"
	const key = "fixture-service-password"
	wrong := 0
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { wrong++; io.WriteString(w, "ok") }))
	defer other.Close()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, other.URL, 302)
			return
		}
		u, k, ok := r.BasicAuth()
		if !ok || u != user || k != key {
			t.Error("incorrect saved credential")
		}
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, u+" "+k+" "+r.Header.Get("Authorization"))
	}))
	defer server.Close()
	v := toolTestVault(t)
	prompts := 0
	ctx := secrets.WithCredentialPrompt(secrets.WithCredentialGrant(secrets.WithVault(context.Background(), v)), func(_ context.Context, scope secrets.CredentialScope, _ secrets.CredentialFields) (secrets.CredentialPair, error) {
		prompts++
		if scope.Service != server.URL {
			return secrets.CredentialPair{}, secrets.ErrCredentialCancelled
		}
		return secrets.CredentialPair{Username: []byte(user), Key: []byte(key)}, nil
	})
	f, err := newWebService(harness.BuiltinToolConfig{Name: "web_fetch"})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/first", "/second"} {
		out, err := f.fetch(ctx, webFetchInput{URL: server.URL + path, Credential: "basic"})
		if err != nil || strings.Contains(out, user) || strings.Contains(out, key) || strings.Contains(out, base64.StdEncoding.EncodeToString([]byte(user+":"+key))) {
			t.Fatal("reflected credential in result or failed request", err)
		}
	}
	if _, err = f.fetch(ctx, webFetchInput{URL: server.URL + "/redirect", Credential: "basic"}); err == nil {
		t.Fatal("followed credential redirect")
	}
	if _, err = f.fetch(ctx, webFetchInput{URL: other.URL, Credential: "basic"}); err == nil || wrong != 0 || prompts != 2 || calls != 3 {
		t.Fatal("origin isolation failed", err)
	}
}
