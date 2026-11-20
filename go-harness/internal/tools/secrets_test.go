package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
	"github.com/omengye/deerflow-acp/go-harness/internal/testutil"
)

func notExecuted(err error) bool {
	var marker *harness.ToolNotExecutedError
	return errors.As(err, &marker)
}

func toolTestVault(t *testing.T) *secrets.Vault {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	v, err := secrets.Open(context.Background(), store.DB(), filepath.Join(dir, "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	if _, err = v.Set(context.Background(), secrets.SetRequest{Name: "TOKEN", Kind: secrets.KindString, Value: []byte("tool-token-value-123")}); err != nil {
		t.Fatal(err)
	}
	return v
}

// secretBackend records the injected material and plays a command that
// writes a refreshed secret into its write-back directory.
type secretBackend struct {
	commandBackendFixture
	secrets  *harness.CommandSecrets
	material string
}

func (b *secretBackend) Start(ctx context.Context, r harness.CommandRequest) (harness.CommandSnapshot, error) {
	b.secrets = r.Secrets
	if r.Secrets != nil && r.Secrets.OutDir != "" {
		b.material = filepath.Dir(r.Secrets.OutDir)
		_ = os.WriteFile(filepath.Join(r.Secrets.OutDir, "TOKEN"), []byte("tool-token-rotated-456\n"), 0o600)
	}
	return b.commandBackendFixture.Start(ctx, r)
}

func TestCommandToolInjectsSecretsAndReportsWriteBack(t *testing.T) {
	v := toolTestVault(t)
	exit := 0
	backend := &secretBackend{commandBackendFixture: commandBackendFixture{snapshot: harness.CommandSnapshot{ID: "owned-command", State: harness.CommandCompleted, ExitCode: &exit, TerminationConfirmed: true}}}
	command, err := CommandTool(backend, harness.SandboxLocal, WithSecretVault(v))
	if err != nil {
		t.Fatal(err)
	}
	info, _ := command.Info(context.Background())
	schema, _ := info.ParamsOneOf.ToJSONSchema()
	encoded, _ := json.Marshal(schema)
	if !strings.Contains(string(encoded), `"secrets"`) || !strings.Contains(string(encoded), `"secret_out"`) {
		t.Fatalf("schema lacks secret parameters: %s", encoded)
	}
	out, err := command.InvokableRun(context.Background(), `{"executable":"/allowed","secrets":{"API_TOKEN":"TOKEN"},"secret_out":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if backend.secrets == nil || backend.secrets.Env["API_TOKEN"] != "tool-token-value-123" {
		t.Fatalf("secret not injected: %+v", backend.secrets)
	}
	if _, err := os.Stat(backend.material); !os.IsNotExist(err) {
		t.Fatalf("secret material survived the command: %v", err)
	}
	var result struct {
		Secrets SecretReport `json:"secrets"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Secrets.Used) != 1 || result.Secrets.Used[0].Name != "TOKEN" || len(result.Secrets.Updated) != 1 || result.Secrets.Updated[0].Status != "updated" || result.Secrets.Updated[0].Version != 2 {
		t.Fatalf("secret report = %s", out)
	}
	if strings.Contains(out, "tool-token") {
		t.Fatalf("secret value in tool output: %s", out)
	}
	resolved, err := v.Resolve(context.Background(), []string{"TOKEN"})
	if err != nil || string(resolved["TOKEN"].Value) != "tool-token-rotated-456" {
		t.Fatalf("write-back value = %q, %v", resolved["TOKEN"].Value, err)
	}
}

func TestCommandToolRejectsUnavailableSecretsBeforeStart(t *testing.T) {
	v := toolTestVault(t)
	backend := &secretBackend{}
	plain, err := CommandTool(backend, harness.SandboxLocal)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := plain.Info(context.Background())
	schema, _ := info.ParamsOneOf.ToJSONSchema()
	if encoded, _ := json.Marshal(schema); strings.Contains(string(encoded), `"secrets"`) {
		t.Fatalf("schema without vault advertises secrets: %s", encoded)
	}
	if _, err := plain.InvokableRun(context.Background(), `{"executable":"/allowed","secrets":{"API_TOKEN":"TOKEN"}}`); !notExecuted(err) {
		t.Fatalf("secrets without vault = %v", err)
	}
	withVault, err := CommandTool(backend, harness.SandboxLocal, WithSecretVault(v))
	if err != nil {
		t.Fatal(err)
	}
	_, err = withVault.InvokableRun(context.Background(), `{"executable":"/allowed","secrets":{"API_TOKEN":"MISSING"}}`)
	if !notExecuted(err) || !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("missing secret = %v", err)
	}
	bash, err := BashTool(backend, harness.SandboxLocal, WithSecretVault(v))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bash.InvokableRun(context.Background(), `{"command":"true","secrets":{"PATH":"TOKEN"}}`); !notExecuted(err) {
		t.Fatalf("reserved variable = %v", err)
	}
	if backend.starts != 0 {
		t.Fatalf("started %d commands with invalid secrets", backend.starts)
	}
}

// Output shows a stored value as its marker, so a model that edits such a
// file writes the marker back. The write must preserve only credentials that
// already exist in the file and never materialize unrelated secrets.
func TestWorkspaceWritesRestoreSecretMarkers(t *testing.T) {
	v := toolTestVault(t)
	ctx := secrets.WithVault(context.Background(), v)
	const value = "tool-token-value-123"
	const otherValue = "other-unrelated-secret-999"
	if _, err := v.Set(ctx, secrets.SetRequest{Name: "OTHER_SECRET", Kind: secrets.KindString, Value: []byte(otherValue), Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	for _, migrated := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "configured"}[migrated], func(t *testing.T) {
			cwd := testutil.CanonicalTempDir(t)
			named := versionTools(t, cwd, migrated)
			call := func(name string, args map[string]any) (string, error) {
				t.Helper()
				b, err := json.Marshal(args)
				if err != nil {
					t.Fatal(err)
				}
				return named[name].InvokableRun(ctx, string(b))
			}
			read := func(path string) (content, version string) {
				t.Helper()
				data, err := os.ReadFile(filepath.Join(cwd, path))
				if err != nil {
					t.Fatal(err)
				}
				out, err := call("read_file", map[string]any{"path": path})
				var result struct{ Version string }
				if err != nil || json.Unmarshal([]byte(out), &result) != nil {
					t.Fatal(out, err)
				}
				return string(data), result.Version
			}
			// Creating a new file must NEVER materialize vault secrets from markers.
			if _, err := call("write_file", map[string]any{"path": "new.env", "content": "TOKEN=[secret:TOKEN]\n"}); err != nil {
				t.Fatal(err)
			}
			if content, _ := read("new.env"); content != "TOKEN=[secret:TOKEN]\n" {
				t.Fatalf("new file materialized a vault secret: %q", content)
			}
			// Pre-populate .env with TOKEN's actual value (e.g. written by user/command).
			if err := os.WriteFile(filepath.Join(cwd, ".env"), []byte("TOKEN="+value+"\nMODE=dev\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			content, version := read(".env")
			if content != "TOKEN="+value+"\nMODE=dev\n" {
				t.Fatalf("initial .env = %q", content)
			}
			edit, oldKey, newKey := "edit_file", "old_text", "new_text"
			if migrated {
				edit, oldKey, newKey = "str_replace", "old_str", "new_str"
			}
			// Editing .env preserves TOKEN (already in file) but refuses to materialize OTHER_SECRET.
			if _, err := call(edit, map[string]any{"path": ".env", oldKey: "TOKEN=[secret:TOKEN]\nMODE=dev", newKey: "TOKEN=[secret:TOKEN]\nMODE=prod\nEXFIL=[secret:OTHER_SECRET]", "expected_version": version}); err != nil {
				t.Fatal(err)
			}
			if content, version = read(".env"); content != "TOKEN="+value+"\nMODE=prod\nEXFIL=[secret:OTHER_SECRET]\n" {
				t.Fatalf("%s stored %q", edit, content)
			}
			if migrated {
				if _, err := call("write_file", map[string]any{"path": ".env", "content": "BACKUP=[secret:TOKEN]\n", "append": true, "expected_version": version}); err != nil {
					t.Fatal(err)
				}
				if content, _ = read(".env"); content != "TOKEN="+value+"\nMODE=prod\nEXFIL=[secret:OTHER_SECRET]\nBACKUP="+value+"\n" {
					t.Fatalf("append stored %q", content)
				}
				// Sliding 4-byte reads across .env must never leak any plaintext bytes of TOKEN.
				for off := 0; off < len(content); off += 4 {
					args := map[string]any{"path": ".env", "max_bytes": 4}
					if off > 0 {
						args["offset"] = off
						args["expected_version"] = fileVersion(content)
					}
					out, err := call("read_file", args)
					if err != nil {
						t.Fatalf("read_file offset=%d: %v", off, err)
					}
					var chunk struct{ Content string }
					if err := json.Unmarshal([]byte(out), &chunk); err != nil {
						t.Fatal(err)
					}
					for i := 0; i+4 <= len(value); i++ {
						if strings.Contains(chunk.Content, value[i:i+4]) {
							t.Fatalf("4-byte read at offset %d leaked secret slice %q in %q", off, value[i:i+4], chunk.Content)
						}
					}
				}
			}
			// A cut-off value cannot be restored, so nothing is written.
			_, err := call("write_file", map[string]any{"path": "partial", "content": "TOKEN=[secret:TOKEN:partial]"})
			if !notExecuted(err) {
				t.Fatalf("partial marker: %v", err)
			}
			if _, statErr := os.Stat(filepath.Join(cwd, "partial")); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("partial marker was written: %v", statErr)
			}
			// Markers of unknown secrets are ordinary text.
			if _, err := call("write_file", map[string]any{"path": "doc.md", "content": "Set [secret:UNKNOWN_TOKEN] first."}); err != nil {
				t.Fatal(err)
			}
			if content, _ = read("doc.md"); content != "Set [secret:UNKNOWN_TOKEN] first." {
				t.Fatalf("unknown marker stored as %q", content)
			}
		})
	}
}

func TestSubagentDisallowedCannotInjectSecrets(t *testing.T) {
	ctx := secrets.Disallow(context.Background())
	v := toolTestVault(t)
	_, _, err := ExecuteWithSecrets(ctx, &commandBackendFixture{}, harness.CommandRequest{Script: "echo hi"}, SecretRequest{
		Vault:    v,
		Bindings: map[string]string{"TOKEN": "TOKEN"},
		Actor:    "subagent",
	})
	if !notExecuted(err) || !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatalf("disallowed context executed secrets: %v", err)
	}
}

func TestWebFetchWithSecretPlaceholderBlocksDeepSeek(t *testing.T) {
	ctx := secrets.WithPlaceholderUsed(context.Background())
	service, err := newWebService(harness.BuiltinToolConfig{Name: "web_fetch"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.fetch(ctx, webFetchInput{URL: "https://example.test/?k=secret", Provider: "deepseek"}); !notExecuted(err) {
		t.Fatalf("web_fetch allowed DeepSeek relay for secret URL: %v", err)
	}
}

func TestWebFetchWithSecretPlaceholderBlocksCrossOriginRedirectAndMasksBeforeTruncate(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("exfil-target"))
	}))
	defer target.Close()

	token := strings.Repeat("abcd", 8)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, target.URL+"/collect?q="+r.URL.RawQuery, http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		// Place the reflected secret right across MaxOutputChars (30) to test mask-before-truncate.
		_, _ = w.Write([]byte(strings.Repeat("a", 24) + token + strings.Repeat("b", 40)))
	}))
	defer origin.Close()

	v := toolTestVault(t)
	if _, err := v.Set(context.Background(), secrets.SetRequest{Name: "FETCH_TOKEN", Kind: secrets.KindString, Value: []byte(token), Actor: "test"}); err != nil {
		t.Fatal(err)
	}

	service, err := newWebService(harness.BuiltinToolConfig{Name: "web_fetch", MaxOutputChars: 30})
	if err != nil {
		t.Fatal(err)
	}
	service.client.Transport = &http.Transport{}

	secretCtx := secrets.WithPlaceholderUsed(context.Background())
	if _, err := service.fetch(secretCtx, webFetchInput{URL: origin.URL + "/redirect?k=" + token, Provider: "local"}); !errors.Is(err, errSecretRedirectBlocked) {
		t.Fatalf("cross-origin redirect with secret placeholder: %v; want %v", err, errSecretRedirectBlocked)
	}
	if hits := targetHits.Load(); hits != 0 {
		t.Fatalf("cross-origin redirect target received %d hits; want 0", hits)
	}

	// Ordinary non-secret fetches still follow redirects normally.
	if out, err := service.fetch(context.Background(), webFetchInput{URL: origin.URL + "/redirect?k=plain", Provider: "local"}); err != nil || !strings.Contains(out, "exfil-target") {
		t.Fatalf("ordinary redirect failed: out=%q err=%v", out, err)
	}

	// Reflected secret crossing MaxOutputChars must be masked BEFORE truncation.
	out, err := service.fetch(secretCtx, webFetchInput{URL: origin.URL + "/reflect", Provider: "local"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+4 <= len(token); i++ {
		if strings.Contains(out, token[i:i+4]) {
			t.Fatalf("truncated web_fetch output leaked secret slice %q in %q", token[i:i+4], out)
		}
	}
}
