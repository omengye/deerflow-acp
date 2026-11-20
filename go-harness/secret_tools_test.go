package deerflow

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/channels"
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
	"github.com/omengye/deerflow-acp/go-harness/internal/testutil"
)

func secretNotExecuted(err error) bool {
	var marker *harness.ToolNotExecutedError
	return errors.As(err, &marker)
}

func randomTestSecret(t *testing.T, prefix string) string {
	t.Helper()
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return prefix + hex.EncodeToString(b)
}

func secretToolFixture(t *testing.T, session harness.Session) (*secrets.Vault, *secretTool, *secretTool) {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	vault, err := secrets.Open(context.Background(), store.DB(), filepath.Join(dir, "secrets"))
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		vault.Close()
		store.Close()
	})
	items, instruction, err := secretTools(vault, harness.RunRequest{Session: session, RunID: "run"}, secretSources{})
	if err != nil || len(items) != 2 || !strings.Contains(instruction, "secret vault") {
		t.Fatalf("secret tools: %d, %v", len(items), err)
	}
	return vault, items[0].(*secretTool), items[1].(*secretTool)
}

func resolveSecret(t *testing.T, vault *secrets.Vault, name string) string {
	t.Helper()
	resolved, err := vault.Resolve(context.Background(), []string{name})
	if err != nil {
		t.Fatal(err)
	}
	return string(resolved[name].Value)
}

func TestSecretManageLifecycleNeverReturnsValues(t *testing.T) {
	ctx := context.Background()
	vault, read, manage := secretToolFixture(t, harness.Session{ID: "s1", CWD: testutil.CanonicalTempDir(t), Mode: "default"})
	first, second := randomTestSecret(t, "first-"), randomTestSecret(t, "second-")
	call := func(tool *secretTool, args map[string]any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(args)
		out, err := tool.InvokableRun(ctx, string(b))
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if strings.Contains(out, first) || strings.Contains(out, second) {
			t.Fatalf("%v returned a value: %s", args["action"], out)
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(out), &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	secret := func(out map[string]any) map[string]any {
		value, _ := out["secret"].(map[string]any)
		return value
	}
	set := map[string]any{"action": "set", "name": "API_TOKEN", "value": first, "description": "CI token"}
	if out := call(manage, set); out["status"] != "stored" || secret(out)["version"] != float64(1) || secret(out)["marker"] != "[secret:API_TOKEN]" || secret(out)["kind"] != "string" {
		t.Fatal(out)
	}
	// A retried call with the same intent adds no version.
	if out := call(manage, set); out["status"] != "unchanged" || secret(out)["version"] != float64(1) {
		t.Fatal(out)
	}
	if out := call(manage, map[string]any{"action": "set", "name": "API_TOKEN", "value": second}); out["status"] != "stored" || secret(out)["version"] != float64(2) || secret(out)["description"] != "CI token" {
		t.Fatal(out)
	}
	if out := call(read, map[string]any{"action": "get", "name": "API_TOKEN", "history": true}); secret(out)["versions"] == nil || !strings.Contains(fmt.Sprint(out["use"]), "secrets={ENV_VAR: NAME}") {
		t.Fatal(out)
	}
	if out := call(manage, map[string]any{"action": "delete", "name": "API_TOKEN"}); out["status"] != "deleted" {
		t.Fatal(out)
	}
	if out := call(read, map[string]any{"action": "list"}); out["count"] != float64(0) {
		t.Fatal(out)
	}
	if out := call(read, map[string]any{"action": "list", "include_deleted": true}); out["count"] != float64(1) {
		t.Fatal(out)
	}
	if out := call(manage, map[string]any{"action": "restore", "name": "API_TOKEN"}); out["status"] != "restored" || resolveSecret(t, vault, "API_TOKEN") != second {
		t.Fatal(out)
	}
	if out := call(manage, map[string]any{"action": "restore", "name": "API_TOKEN", "version": 1}); out["status"] != "restored" || resolveSecret(t, vault, "API_TOKEN") != first {
		t.Fatal(out)
	}

	// reveal is intentionally not exposed to agents; plaintext never returns to the model.
	if _, err := manage.InvokableRun(ctx, `{"action":"reveal","name":"API_TOKEN"}`); !secretNotExecuted(err) || !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatalf("reveal was not rejected as invalid action: %v", err)
	}
}

func TestSecretToolArgumentsAreStrictAndNeverEchoed(t *testing.T) {
	ctx := context.Background()
	vault, read, manage := secretToolFixture(t, harness.Session{ID: "s1", CWD: testutil.CanonicalTempDir(t), Mode: "default"})
	value := randomTestSecret(t, "misplaced-")
	for _, args := range []string{
		// Other spellings of value are sealed by the engine, never read here.
		`{"action":"set","name":"API_TOKEN","Value":"` + value + `"}`,
		`{"action":"set","name":"API_TOKEN","` + value + `":"x"}`,
		`{"action":"set","name":"api token","value":"` + value + `"}`,
		`{"action":"set","name":"API_TOKEN","value":"` + value + `","generate":{}}`,
		`{"action":"set","name":"API_TOKEN","value":"` + value + `"} {"action":"delete"}`,
		`[{"action":"set","name":"API_TOKEN","value":"` + value + `"}]`,
		`{"action":"set","name":"API_TOKEN","value":12345678901}`,
		`{"action":"set","name":"API_TOKEN","value":"` + value + `","delete_source":true}`,
		`{"action":"set","name":"API_TOKEN","value":"` + value + `","file_name":"key.pem"}`,
		`{"action":"set","name":"API_TOKEN","value":"` + value + `","version":2}`,
		`{"action":"delete","name":"API_TOKEN","value":"` + value + `"}`,
		`{"action":"rotate","name":"API_TOKEN","value":"` + value + `"}`,
		`{"malformed_arguments":true}`,
	} {
		_, err := manage.InvokableRun(ctx, args)
		if !secretNotExecuted(err) || strings.Contains(err.Error(), value) {
			t.Fatalf("%s: %v", args, err)
		}
	}
	if _, err := read.InvokableRun(ctx, `{"action":"list","value":"`+value+`"}`); !secretNotExecuted(err) || strings.Contains(err.Error(), value) {
		t.Fatalf("secret_read accepted a value: %v", err)
	}
	if items, err := vault.List(ctx, true); err != nil || len(items) != 0 {
		t.Fatalf("rejected arguments stored %d secrets: %v", len(items), err)
	}
}

func TestSecretManageSealedValueIsBoundToCallAndName(t *testing.T) {
	ctx := context.Background()
	vault, _, manage := secretToolFixture(t, harness.Session{ID: "s1", CWD: testutil.CanonicalTempDir(t), Mode: "default"})
	value := randomTestSecret(t, "sealed-")
	seal := func(callID, name string) string {
		t.Helper()
		sealed, err := vault.Seal(secrets.ValueBinding(callID, name), []byte(strconv.Quote(value)))
		if err != nil {
			t.Fatal(err)
		}
		return sealed
	}
	args := func(name, sealed string) string {
		b, _ := json.Marshal(map[string]any{"action": "set", "name": name, "value_sealed": sealed})
		return string(b)
	}
	// Outside a tool node the call ID is empty, so "" is the matching binding.
	for _, bad := range []string{args("API_TOKEN", seal("another-call", "API_TOKEN")), args("OTHER_TOKEN", seal("", "API_TOKEN")), args("API_TOKEN", "v1.forged.value")} {
		if _, err := manage.InvokableRun(ctx, bad); !secretNotExecuted(err) || !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatalf("%s: %v", bad, err)
		}
	}
	both, _ := json.Marshal(map[string]any{"action": "set", "name": "API_TOKEN", "value": "plain-value-123", "value_sealed": seal("", "API_TOKEN")})
	if _, err := manage.InvokableRun(ctx, string(both)); !secretNotExecuted(err) {
		t.Fatalf("value and value_sealed together: %v", err)
	}
	if _, err := manage.InvokableRun(ctx, args("API_TOKEN", seal("", "API_TOKEN"))); err != nil {
		t.Fatal(err)
	}
	if got := resolveSecret(t, vault, "API_TOKEN"); got != value {
		t.Fatalf("stored %q", got)
	}
}

func TestSecretManageImportsWorkspaceFilesAndGenerates(t *testing.T) {
	ctx := context.Background()
	workspace := testutil.CanonicalTempDir(t)
	vault, _, manage := secretToolFixture(t, harness.Session{ID: "s1", CWD: workspace, Mode: "default"})
	call := func(args map[string]any) (map[string]any, error) {
		t.Helper()
		b, _ := json.Marshal(args)
		out, err := manage.InvokableRun(ctx, string(b))
		if err != nil {
			return nil, err
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(out), &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded, nil
	}
	pem := "-----BEGIN TEST KEY-----\n" + randomTestSecret(t, "") + "\n-----END TEST KEY-----\n"
	token := randomTestSecret(t, "imported-")
	if err := os.MkdirAll(filepath.Join(workspace, "certs"), 0700); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{"certs/key.pem": pem, "token.txt": token + "\n"} {
		if err := os.WriteFile(filepath.Join(workspace, path), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	out, err := call(map[string]any{"action": "set", "name": "SIGNING_KEY", "from_path": "certs/key.pem", "delete_source": true})
	if err != nil {
		t.Fatal(err)
	}
	if secret := out["secret"].(map[string]any); out["status"] != "stored" || secret["kind"] != "file" || secret["file_name"] != "key.pem" || secret["source"] != "file" || out["warning"] != nil {
		t.Fatal(out)
	}
	if _, err := os.Stat(filepath.Join(workspace, "certs", "key.pem")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source file kept: %v", err)
	}
	if resolveSecret(t, vault, "SIGNING_KEY") != pem {
		t.Fatal("file secret changed")
	}
	// An absolute path inside the workspace works; a string drops one newline.
	if out, err = call(map[string]any{"action": "set", "name": "IMPORTED_TOKEN", "from_path": filepath.Join(workspace, "token.txt"), "kind": "string"}); err != nil || out["secret"].(map[string]any)["kind"] != "string" {
		t.Fatal(out, err)
	}
	if resolveSecret(t, vault, "IMPORTED_TOKEN") != token {
		t.Fatal("string secret kept its newline")
	}
	if _, err := os.Stat(filepath.Join(workspace, "token.txt")); err != nil {
		t.Fatalf("source deleted without delete_source: %v", err)
	}

	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "outside.txt")
	if err := os.WriteFile(outside, []byte(randomTestSecret(t, "outside-")), 0600); err != nil {
		t.Fatal(err)
	}
	escapes := []string{"../outside.txt", outside, "certs", "missing.txt", "."}
	if os.Symlink(outside, filepath.Join(workspace, "link.txt")) == nil {
		escapes = append(escapes, "link.txt")
	}
	for _, path := range escapes {
		if _, err := call(map[string]any{"action": "set", "name": "ESCAPED", "from_path": path}); !secretNotExecuted(err) {
			t.Fatalf("from_path %q: %v", path, err)
		}
	}
	if _, err := vault.Get(ctx, "ESCAPED", false); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("escaped path was stored: %v", err)
	}

	generate := map[string]any{"action": "set", "name": "DB_PASSWORD", "generate": map[string]any{"length": 40, "charset": "hex"}}
	if out, err = call(generate); err != nil || out["status"] != "stored" || out["secret"].(map[string]any)["source"] != "generated" {
		t.Fatal(out, err)
	}
	generated := resolveSecret(t, vault, "DB_PASSWORD")
	if len(generated) != 40 || strings.Trim(generated, "0123456789abcdef") != "" {
		t.Fatalf("generated %d bytes outside the hex alphabet", len(generated))
	}
	if out, err = call(generate); err != nil || out["status"] != "stored" || resolveSecret(t, vault, "DB_PASSWORD") == generated {
		t.Fatal("generating again did not create a new value", out, err)
	}
	for _, bad := range []map[string]any{
		{"action": "set", "name": "SHORT", "generate": map[string]any{"length": 7}},
		{"action": "set", "name": "ODD", "generate": map[string]any{"charset": "emoji"}},
		{"action": "set", "name": "KEYFILE", "kind": "file", "generate": map[string]any{}},
	} {
		if _, err := call(bad); !secretNotExecuted(err) {
			t.Fatalf("%v: %v", bad, err)
		}
	}
}

func TestSecretManageIsDeniedInReadOnlySessions(t *testing.T) {
	ctx := context.Background()
	for _, session := range []harness.Session{
		{ID: "plan", Mode: "plan"},
		{ID: "read-only", Mode: "default", ApprovalMode: harness.ApprovalReadOnly},
		{ID: "reject-always", Mode: "default", ApprovalMode: harness.ApprovalRejectAlways},
	} {
		t.Run(session.ID, func(t *testing.T) {
			session.CWD = testutil.CanonicalTempDir(t)
			vault, read, manage := secretToolFixture(t, session)
			if _, err := vault.Set(ctx, secrets.SetRequest{Name: "API_TOKEN", Kind: secrets.KindString, Value: []byte(randomTestSecret(t, "existing-")), Actor: "test"}); err != nil {
				t.Fatal(err)
			}
			for _, args := range []string{
				`{"action":"set","name":"API_TOKEN","value":"replacement-value"}`,
				`{"action":"set","name":"NEW_TOKEN","generate":{}}`,
				`{"action":"delete","name":"API_TOKEN"}`,
				`{"action":"restore","name":"API_TOKEN","version":1}`,
			} {
				if _, err := manage.InvokableRun(ctx, args); !secretNotExecuted(err) || !errors.Is(err, harness.ErrPermissionDenied) {
					t.Fatalf("%s: %v", args, err)
				}
			}
			out, err := read.InvokableRun(ctx, `{"action":"list"}`)
			if err != nil || !strings.Contains(out, `"count":1`) {
				t.Fatalf("secret_read: %s, %v", out, err)
			}
			meta, err := vault.Get(ctx, "API_TOKEN", false)
			if err != nil || meta.Version != 1 || meta.DeletedAt != nil || meta.ExposedAt != nil {
				t.Fatalf("denied call changed the secret: %+v, %v", meta, err)
			}
			if _, err := vault.Get(ctx, "NEW_TOKEN", false); !errors.Is(err, secrets.ErrNotFound) {
				t.Fatalf("denied generate stored a secret: %v", err)
			}
		})
	}
}

// TestSecretCommandFixtureChild is the process the end-to-end commands start.
// It prints the injected values and writes a refreshed token back.
func TestSecretCommandFixtureChild(t *testing.T) {
	if os.Getenv("DEERFLOW_SECRET_COMMAND_FIXTURE") != "1" {
		return
	}
	token := os.Getenv("FIXTURE_TOKEN")
	fmt.Fprintf(os.Stdout, "token=%s other=%s\n", token, os.Getenv("FIXTURE_OTHER"))
	if dir := os.Getenv("DEERFLOW_SECRET_OUT"); dir != "" {
		if err := os.WriteFile(filepath.Join(dir, "REFRESHED_TOKEN"), []byte(refreshedSecret(token)), 0600); err != nil {
			os.Exit(3)
		}
	}
	os.Exit(0)
}

func refreshedSecret(token string) string { return "refreshed-" + strings.ToUpper(token) }

type secretProviderRequest struct {
	Stream   bool `json:"stream"`
	Messages []struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		ToolCallID string          `json:"tool_call_id"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

func (r secretProviderRequest) offers(name string) bool {
	for _, item := range r.Tools {
		if item.Function.Name == name {
			return true
		}
	}
	return false
}

func (r secretProviderRequest) toolResults() map[string]string {
	out := map[string]string{}
	for _, message := range r.Messages {
		if message.Role != "tool" {
			continue
		}
		var text string
		if json.Unmarshal(message.Content, &text) != nil {
			text = string(message.Content)
		}
		out[message.ToolCallID] = text
	}
	return out
}

// latestToolResults returns the results of the agent request that saw the
// most tool results; later requests may belong to post-run maintenance.
func latestToolResults(requests []secretProviderRequest) map[string]string {
	var out map[string]string
	for _, request := range requests {
		if results := request.toolResults(); len(results) >= len(out) {
			out = results
		}
	}
	return out
}

type secretToolStep struct {
	name string
	args map[string]any
}

// secretProvider plays the steps in order: each request answers with the
// next call after the results seen so far, then with a final text.
func secretProvider(t *testing.T, steps []secretToolStep) (*httptest.Server, func() ([][]byte, []secretProviderRequest)) {
	t.Helper()
	var mu sync.Mutex
	var bodies [][]byte
	var requests []secretProviderRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var req secretProviderRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		mu.Lock()
		bodies = append(bodies, body)
		requests = append(requests, req)
		mu.Unlock()
		step := len(req.toolResults())
		if !req.offers("secret_manage") || step >= len(steps) {
			skillToolResponse(w, req.Stream, "", "", "All secrets are in place.")
			return
		}
		args, _ := json.Marshal(steps[step].args)
		skillToolResponse(w, req.Stream, steps[step].name, string(args), "", fmt.Sprintf("call-%d", step))
	}))
	t.Cleanup(server.Close)
	return server, func() ([][]byte, []secretProviderRequest) {
		mu.Lock()
		defer mu.Unlock()
		return append([][]byte(nil), bodies...), append([]secretProviderRequest(nil), requests...)
	}
}

// assertNoSecretBytes fails when any file under dir contains a value.
func assertNoSecretBytes(t *testing.T, dir string, values ...string) {
	t.Helper()
	var scanned int
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		for _, value := range values {
			if bytes.Contains(data, []byte(value)) {
				t.Errorf("%s contains a secret value", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned == 0 {
		t.Fatal("no files were scanned")
	}
}

// The real SDK, tools and OpenAI adapter store a secret from a workspace file
// and from a model argument, inject both into a command that refreshes a third
// one, and write a config file from markers. No value reaches the provider,
// the client events or the data directory.
func TestSecretVaultEndToEndKeepsValuesOutOfModelAndDisk(t *testing.T) {
	ctx := context.Background()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	token, other := randomTestSecret(t, "tok-"), randomTestSecret(t, "other-")
	refreshed := refreshedSecret(token)
	workspace := testutil.CanonicalTempDir(t)
	if err := os.WriteFile(filepath.Join(workspace, "token.txt"), []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Pre-existing config.env holds API_TOKEN so editing it preserves the existing
	// credential via its marker, while creating a brand new file cannot materialize vault values.
	initialEnv := "TOKEN=" + token + "\nMODE=dev\n"
	if err := os.WriteFile(filepath.Join(workspace, "config.env"), []byte(initialEnv), 0600); err != nil {
		t.Fatal(err)
	}
	envHash := fmt.Sprintf("%x", sha256.Sum256([]byte(initialEnv)))
	provider, seen := secretProvider(t, []secretToolStep{
		{"secret_manage", map[string]any{"action": "set", "name": "API_TOKEN", "kind": "string", "from_path": "token.txt", "delete_source": true}},
		{"secret_manage", map[string]any{"action": "set", "name": "OTHER_TOKEN", "value": other, "description": "Other service"}},
		{"execute", map[string]any{"executable": exe, "args": []string{"-test.run=^TestSecretCommandFixtureChild$"}, "secrets": map[string]string{"FIXTURE_TOKEN": "API_TOKEN", "FIXTURE_OTHER": "OTHER_TOKEN"}, "secret_out": true}},
		{"edit_file", map[string]any{"path": "config.env", "old_text": "TOKEN=[secret:API_TOKEN]\nMODE=dev", "new_text": "TOKEN=[secret:API_TOKEN]\nMODE=prod", "expected_version": envHash}},
		{"write_file", map[string]any{"path": "fresh.env", "content": "LEAK=[secret:API_TOKEN]\n"}},
		{"read_file", map[string]any{"path": "config.env"}},
		{"secret_read", map[string]any{"action": "list"}},
	})
	dataDir := t.TempDir()
	c, err := Open(ctx, Config{DataDir: dataDir, Provider: "openai", Model: "fixture", APIKey: "fixture-only", BaseURL: provider.URL + "/v1", PermissionMode: harness.PermissionModeDangerous, DisableSubagents: true,
		Sandbox: harness.SandboxConfig{Enabled: true, Provider: harness.SandboxLocal, AllowedExecutables: []string{exe}, Environment: map[string]string{"DEERFLOW_SECRET_COMMAND_FIXTURE": "1"}, Limits: harness.CommandLimits{Timeout: 30 * time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			c.Close()
		}
	})
	x, err := c.NewSession(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	var events []harness.RunEvent
	var approvals []harness.PermissionRequest
	result, err := c.Run(ctx, x.ID, []harness.Content{{Type: "text", Text: "Store the token from token.txt, use it, and update config.env."}}, func(_ context.Context, event harness.RunEvent) error {
		events = append(events, event)
		return nil
	}, func(_ context.Context, request harness.PermissionRequest) (harness.PermissionDecision, error) {
		approvals = append(approvals, request)
		return harness.AllowOnce, nil
	})
	if err != nil {
		t.Fatal(result, err)
	}

	bodies, requests := seen()
	if len(requests) < 8 {
		t.Fatalf("provider saw %d requests", len(requests))
	}
	for i, body := range bodies {
		for _, value := range []string{token, other, refreshed} {
			if bytes.Contains(body, []byte(value)) {
				t.Fatalf("provider request %d contains a secret value", i)
			}
		}
	}
	results := latestToolResults(requests)
	for id, want := range map[string][]string{
		"call-0": {`"status":"stored"`, `"kind":"string"`, `"source":"file"`},
		"call-1": {`"status":"stored"`, "[secret:OTHER_TOKEN]"},
		"call-2": {"token=[secret:API_TOKEN] other=[secret:OTHER_TOKEN]", `"name":"REFRESHED_TOKEN"`},
		"call-5": {`TOKEN=[secret:API_TOKEN]\nMODE=prod\n`},
		"call-6": {`"count":3`, `"name":"API_TOKEN"`, `"name":"OTHER_TOKEN"`, `"name":"REFRESHED_TOKEN"`},
	} {
		for _, text := range want {
			if !strings.Contains(results[id], text) {
				t.Fatalf("result %s lacks %q: %.400s", id, text, results[id])
			}
		}
	}
	// secret_read needs no approval; secret_manage and execute ask for approval without showing values.
	var commandApproval, manageApproval bool
	for _, request := range approvals {
		if request.ToolName == "secret_read" {
			t.Fatalf("%s asked for approval", request.ToolName)
		}
		if request.ToolName == "secret_manage" {
			manageApproval = true
		}
		if request.ToolName == "execute" {
			commandApproval = strings.Contains(string(request.Arguments), `"FIXTURE_TOKEN":"API_TOKEN"`)
		}
		for _, value := range []string{token, other, refreshed} {
			if strings.Contains(string(request.Arguments), value) {
				t.Fatalf("%s approval shows a value", request.ToolName)
			}
		}
	}
	if !commandApproval || !manageApproval {
		t.Fatalf("commandApproval=%t manageApproval=%t", commandApproval, manageApproval)
	}
	for _, event := range events {
		data, _ := json.Marshal(event)
		for _, value := range []string{token, other, refreshed} {
			if bytes.Contains(data, []byte(value)) {
				t.Fatalf("%s event contains a secret value", event.Kind)
			}
		}
	}

	config, err := os.ReadFile(filepath.Join(workspace, "config.env"))
	if err != nil || string(config) != "TOKEN="+token+"\nMODE=prod\n" {
		t.Fatalf("config.env did not preserve existing token on edit: %q, %v", config, err)
	}
	fresh, err := os.ReadFile(filepath.Join(workspace, "fresh.env"))
	if err != nil || string(fresh) != "LEAK=[secret:API_TOKEN]\n" {
		t.Fatalf("new file materialized a vault secret: %q, %v", fresh, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "token.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("token.txt kept: %v", err)
	}
	if got := resolveSecret(t, c.secrets, "REFRESHED_TOKEN"); got != refreshed {
		t.Fatal("command write-back was not stored")
	}
	closed = true
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	assertNoSecretBytes(t, dataDir, token, other, refreshed)
}

// Channel conversations require approval for secret_manage under global mode,
// allow set under allow_always, and reject it under read_only.
func TestSecretToolsInChannelConversation(t *testing.T) {
	for _, approval := range []string{harness.ApprovalGlobal, harness.ApprovalAllowAlways, harness.ApprovalReadOnly} {
		t.Run(approval, func(t *testing.T) {
			ctx := context.Background()
			token := randomTestSecret(t, "wechat-")
			provider, seen := secretProvider(t, []secretToolStep{
				{"secret_manage", map[string]any{"action": "set", "name": "SERVICE_TOKEN", "value": token}},
				{"secret_read", map[string]any{"action": "get", "name": "SERVICE_TOKEN"}},
			})
			dataDir := t.TempDir()
			c, err := Open(ctx, Config{DataDir: dataDir, Provider: "openai", Model: "fixture", APIKey: "fixture", BaseURL: provider.URL + "/v1", PermissionMode: harness.PermissionModeDangerous, DisableSubagents: true})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			workspace := t.TempDir()
			d := setupChannelCommandFixture(t, c, workspace)
			if approval != harness.ApprovalGlobal {
				if _, err = c.channels.SetPermissions(ctx, d.Instance, d.Revision, approval); err != nil {
					t.Fatal(err)
				}
				d.Revision++
			}
			d.MessageID = "message-secret"
			out, err := c.channelRun(channels.WithDispatch(ctx, d), "", workspace, "Save this token for the service", approval, func(string) error { return nil })
			if err != nil {
				t.Fatal(out, err)
			}
			bodies, requests := seen()
			if len(requests) < 1 || !requests[0].offers("secret_manage") || !requests[0].offers("secret_read") {
				t.Fatalf("channel run was not offered the secret tools: %d requests", len(requests))
			}
			for i, body := range bodies[1:] {
				if bytes.Contains(body, []byte(token)) {
					t.Fatalf("provider request %d contains the value", i+1)
				}
			}
			meta, getErr := c.secrets.Get(ctx, "SERVICE_TOKEN", false)
			if approval == harness.ApprovalGlobal {
				if out.Status != "needs_attention" || !errors.Is(getErr, secrets.ErrNotFound) {
					t.Fatalf("global channel did not pause for secret_manage approval: status=%s err=%v", out.Status, getErr)
				}
				return
			}
			if out.Status != "completed" {
				t.Fatalf("status=%s out=%+v", out.Status, out)
			}
			results := latestToolResults(requests)
			if approval == harness.ApprovalReadOnly {
				if !errors.Is(getErr, secrets.ErrNotFound) || !strings.Contains(results["call-0"], "permission denied") {
					t.Fatalf("read-only channel stored a secret: %+v, %v, %q", meta, getErr, results["call-0"])
				}
				return
			}
			if getErr != nil || resolveSecret(t, c.secrets, "SERVICE_TOKEN") != token || !strings.Contains(results["call-1"], `"marker":"[secret:SERVICE_TOKEN]"`) {
				t.Fatalf("channel secret: %+v, %v, %q", meta, getErr, results["call-1"])
			}
		})
	}
}
