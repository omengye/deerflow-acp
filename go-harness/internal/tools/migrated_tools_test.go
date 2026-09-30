package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/opencli"
	"github.com/omengye/deerflow-acp/go-harness/internal/sandbox"
)

func TestMigratedWorkspaceOperationsAndBoundaries(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	configs := []harness.BuiltinToolConfig{}
	for _, name := range []string{"ls", "read_file", "glob", "grep", "write_file", "str_replace", "move_path", "delete_path"} {
		configs = append(configs, harness.BuiltinToolConfig{Name: name})
	}
	items, closeTools, err := ConfiguredWorkspaceFactory(ctx, harness.RunRequest{Session: harness.Session{CWD: cwd}}, configs)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTools()
	named := map[string]tool.InvokableTool{}
	for _, item := range items {
		info, _ := item.Info(ctx)
		named[info.Name] = item.(tool.InvokableTool)
	}
	run := func(name string, args any) string {
		t.Helper()
		data, _ := json.Marshal(args)
		out, err := named[name].InvokableRun(ctx, string(data))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return out
	}
	run("write_file", map[string]any{"path": "nested/notes.txt", "content": "你好\nfirst\nsecond\n", "append": false})
	run("write_file", map[string]any{"path": filepath.Join(cwd, "nested/notes.txt"), "content": "third\n", "append": true})
	run("str_replace", map[string]any{"path": "/mnt/acp-workspace/nested/notes.txt", "old_str": "first", "new_str": "FIRST"})
	var first map[string]any
	json.Unmarshal([]byte(run("read_file", map[string]any{"path": "nested/notes.txt", "max_bytes": 4})), &first)
	if first["content"] != "你" || first["next_offset"] != float64(3) {
		t.Fatalf("UTF8 chunk %+v", first)
	}
	var second map[string]any
	json.Unmarshal([]byte(run("read_file", map[string]any{"path": "nested/notes.txt", "offset": first["next_offset"], "expected_version": first["version"], "max_bytes": 8})), &second)
	if !strings.HasPrefix(second["content"].(string), "好\n") {
		t.Fatal(second)
	}
	lines := run("read_file", map[string]any{"path": "nested/notes.txt", "start_line": 2, "end_line": 3})
	if !strings.Contains(lines, "FIRST") || strings.Contains(lines, "third") {
		t.Fatal(lines)
	}
	if out := run("glob", map[string]any{"path": ".", "pattern": "**/*.txt"}); !strings.Contains(out, "nested/notes.txt") {
		t.Fatal(out)
	}
	if out := run("grep", map[string]any{"path": ".", "pattern": "^first$", "glob": "**/*.txt"}); !strings.Contains(out, "FIRST") || !strings.Contains(out, `"line_number":2`) {
		t.Fatal(out)
	}
	run("move_path", map[string]any{"source": "nested/notes.txt", "destination": "moved/note.txt"})
	for _, test := range []struct {
		name string
		args any
	}{
		{"read_file", map[string]any{"path": "moved/note.txt", "expected_version": "stale"}},
		{"write_file", map[string]any{"path": "../outside.txt", "content": "escape"}},
		{"read_file", map[string]any{"path": filepath.Join(t.TempDir(), "secret")}},
		{"move_path", map[string]any{"source": ".", "destination": "destroy"}},
		{"delete_path", map[string]any{"path": cwd, "recursive": true}},
	} {
		data, _ := json.Marshal(test.args)
		if _, err := named[test.name].InvokableRun(ctx, string(data)); err == nil {
			t.Fatalf("accepted %s %+v", test.name, test.args)
		}
	}
	run("write_file", map[string]any{"path": "moved/existing.txt", "content": "existing"})
	if _, err := named["move_path"].InvokableRun(ctx, `{"source":"moved/note.txt","destination":"moved/existing.txt"}`); err == nil {
		t.Fatal("implicit overwrite")
	}
	run("delete_path", map[string]any{"path": "moved", "recursive": true})
	if _, err := os.Stat(filepath.Join(cwd, "moved")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	readOnly, cleanup, err := ConfiguredWorkspaceFactory(ctx, harness.RunRequest{Session: harness.Session{CWD: cwd, Mode: "plan"}}, configs)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	for _, item := range readOnly {
		info, _ := item.Info(ctx)
		if strings.Contains(info.Name, "write") || info.Name == "str_replace" || info.Name == "move_path" || info.Name == "delete_path" {
			t.Fatal("mutation offered in plan")
		}
	}
}

func TestMigratedWorkspaceSymlinkMutationBoundary(t *testing.T) {
	cwd, outside := t.TempDir(), t.TempDir()
	sentinel := filepath.Join(outside, "keep.txt")
	os.WriteFile(sentinel, []byte("keep"), 0600)
	if err := os.Symlink(outside, filepath.Join(cwd, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	items, cleanup, err := ConfiguredWorkspaceFactory(context.Background(), harness.RunRequest{Session: harness.Session{CWD: cwd}}, []harness.BuiltinToolConfig{{Name: "delete_path"}})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	for _, item := range items {
		info, _ := item.Info(context.Background())
		if info.Name == "delete_path" {
			if _, err := item.(tool.InvokableTool).InvokableRun(context.Background(), `{"path":"link/keep.txt","recursive":true}`); err == nil {
				t.Fatal("escaped symlink parent")
			}
			if _, err := item.(tool.InvokableTool).InvokableRun(context.Background(), `{"path":"link","recursive":true}`); err != nil {
				t.Fatal(err)
			}
		}
	}
	if body, err := os.ReadFile(sentinel); err != nil || string(body) != "keep" {
		t.Fatal("deleted outside sentinel")
	}
}

func TestWorkspacePreMutationRejectionsCarryEvidence(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "existing.txt"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	items, cleanup, err := ConfiguredWorkspaceFactory(context.Background(), harness.RunRequest{Session: harness.Session{CWD: cwd}}, []harness.BuiltinToolConfig{{Name: "write_file"}, {Name: "str_replace"}, {Name: "move_path"}, {Name: "delete_path"}})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	named := map[string]tool.InvokableTool{}
	for _, item := range items {
		info, _ := item.Info(context.Background())
		named[info.Name] = item.(tool.InvokableTool)
	}
	for _, test := range []struct {
		name string
		args any
	}{
		{"write_file", map[string]any{"path": "new/file.txt", "content": strings.Repeat("x", maxFileBytes+1)}},
		{"write_file", map[string]any{"path": "../outside.txt", "content": "escape"}},
		{"str_replace", map[string]any{"path": "existing.txt", "old_str": "absent", "new_str": "changed"}},
		{"move_path", map[string]any{"source": "existing.txt", "destination": "existing.txt"}},
		{"delete_path", map[string]any{"path": ".", "recursive": true}},
	} {
		encoded, _ := json.Marshal(test.args)
		_, err := named[test.name].InvokableRun(context.Background(), string(encoded))
		var rejected *harness.ToolNotExecutedError
		if !errors.As(err, &rejected) {
			t.Fatalf("pre-mutation %s lacks evidence: %v", test.name, err)
		}
	}
	if body, err := os.ReadFile(filepath.Join(cwd, "existing.txt")); err != nil || string(body) != "keep" {
		t.Fatalf("rejected operation changed file: %q %v", body, err)
	}
	if _, err := os.Stat(filepath.Join(cwd, "new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oversized write created a parent directory: %v", err)
	}
	// Once a mutation is attempted, do not claim that the operation was never
	// executed merely because the OS rejects it.
	_, err = named["delete_path"].InvokableRun(context.Background(), `{"path":".","recursive":false}`)
	var rejected *harness.ToolNotExecutedError
	if !errors.As(err, &rejected) {
		t.Fatal("workspace-root check must remain pre-execution")
	}
	if err := os.Mkdir(filepath.Join(cwd, "nonempty"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "nonempty", "keep.txt"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = named["delete_path"].InvokableRun(context.Background(), `{"path":"nonempty"}`)
	if err == nil || errors.As(err, &rejected) {
		t.Fatalf("attempted removal falsely certified not executed: %v", err)
	}
}

func TestNativeWebToolsUseProxyBoundsAndPreserveURLs(t *testing.T) {
	var seenSearch, seenImages, seenPage bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host == "api.search.brave.com" {
			seenSearch = true
			if r.Header.Get("X-Subscription-Token") != "fixture-key" || r.URL.Query().Get("count") != "1" || r.URL.Query().Get("freshness") != "pw" {
				t.Error("Brave config not consumed")
			}
			fmt.Fprint(w, `{"web":{"results":[{"title":"one","url":"https://example.test/a","description":"first"},{"title":"two","url":"https://example.test/b"}]}}`)
			return
		}
		if r.URL.Host == "duckduckgo.com" {
			if r.URL.Path == "/i.js" {
				seenImages = true
				if r.URL.Query().Get("vqd") != "3-123-456" || r.URL.Query().Get("f") != "size:Large,layout:Wide" {
					t.Error("image filters")
				}
				fmt.Fprint(w, `{"results":[{"title":"image","image":"https://images.test/full.jpg","thumbnail":"https://images.test/thumb.jpg"}]}`)
			} else {
				fmt.Fprint(w, `<script>vqd='3-123-456';</script>`)
			}
			return
		}
		seenPage = true
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><head><title>Example</title></head><body><nav>REMOVE_NAV</nav><article><h2>Heading</h2><p>Hello world. <a href="/reference">Read reference</a></p><pre>a\nb</pre><script>REMOVE_SCRIPT</script></article></body></html>`)
	}))
	defer proxy.Close()
	for _, c := range []harness.BuiltinToolConfig{{Name: "web_search", APIKey: "fixture-key", MaxResults: 1, HTTPSProxy: proxy.URL}, {Name: "image_search", HTTPSProxy: proxy.URL}, {Name: "web_fetch", HTTPSProxy: proxy.URL, MaxOutputChars: 256}} {
		item, cleanup, err := WebTools(c)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		info, _ := item.Info(context.Background())
		if info.Name != c.Name {
			t.Fatal("wrong schema name")
		}
		s, err := newWebService(c)
		if err != nil {
			t.Fatal(err)
		}
		defer s.transport.CloseIdleConnections()
		s.braveBase = "http://api.search.brave.com/res/v1/web/search"
		s.duckBase = "http://duckduckgo.com"
		var out string
		switch c.Name {
		case "web_search":
			out, err = s.search(context.Background(), webSearchInput{Query: "test", TimeRange: "week"})
		case "image_search":
			out, err = s.images(context.Background(), imageSearchInput{Query: "test", Size: "Large", Layout: "Wide"})
		case "web_fetch":
			out, err = s.fetch(context.Background(), webFetchInput{URL: "http://example.test/page"})
		}
		if err != nil {
			t.Fatal(err)
		}
		switch c.Name {
		case "web_search":
			if strings.Contains(out, "two") || !strings.Contains(out, "https://example.test/a") {
				t.Fatal(out)
			}
		case "image_search":
			if !strings.Contains(out, "full.jpg") || !strings.Contains(out, "thumb.jpg") {
				t.Fatal(out)
			}
		case "web_fetch":
			if strings.Contains(out, "REMOVE") || !strings.Contains(out, "Heading") || !strings.Contains(out, "Hello world") || !strings.Contains(out, "[Read reference](http://example.test/reference)") {
				t.Fatal(out)
			}
		}
	}
	if !seenSearch || !seenImages || !seenPage {
		t.Fatal("tools bypassed explicit proxy")
	}
}

func TestBingImageFallbackKeepsOriginalImageURL(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host == "duckduckgo.com" {
			w.WriteHeader(403)
			return
		}
		if r.URL.Host != "www.bing.com" || r.URL.Query().Get("qft") != "+filterui:imagesize-large+filterui:aspect-wide" {
			t.Error("fallback query/filter lost")
		}
		fmt.Fprint(w, `<html><a class="iusc" m="{&quot;murl&quot;:&quot;https://images.test/full.jpg&quot;,&quot;turl&quot;:&quot;https://images.test/thumb.jpg&quot;,&quot;t&quot;:&quot;Example&quot;}"></a></html>`)
	}))
	defer proxy.Close()
	s, err := newWebService(harness.BuiltinToolConfig{Name: "image_search", HTTPSProxy: proxy.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer s.transport.CloseIdleConnections()
	s.duckBase = "http://duckduckgo.com"
	s.bingBase = "http://www.bing.com/images/search"
	out, err := s.images(context.Background(), imageSearchInput{Query: "test", Size: "Large", Layout: "Wide"})
	if err != nil || !strings.Contains(out, "full.jpg") || !strings.Contains(out, "thumb.jpg") || !strings.Contains(out, `"provider":"bing"`) {
		t.Fatalf("fallback result=%s err=%v", out, err)
	}
}

func TestNativeWebStatusCancellationAndCredentialRedirect(t *testing.T) {
	var leaked bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("X-Subscription-Token") != ""
		fmt.Fprint(w, `{"web":{"results":[]}}`)
	}))
	defer other.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, other.URL, 302)
		case "/wait":
			<-r.Context().Done()
		case "/huge":
			fmt.Fprint(w, strings.Repeat("x", 2<<20+1))
		default:
			w.WriteHeader(429)
		}
	}))
	defer origin.Close()
	s, err := newWebService(harness.BuiltinToolConfig{Name: "web_search", APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.transport.CloseIdleConnections()
	s.braveBase = origin.URL + "/redirect"
	if _, err := s.search(context.Background(), webSearchInput{Query: "test"}); err != nil {
		t.Fatal(err)
	}
	if leaked {
		t.Fatal("search key leaked across origins")
	}
	for _, path := range []string{"/status", "/huge"} {
		if _, _, _, err := s.get(context.Background(), origin.URL+path, nil); err == nil {
			t.Fatal("unbounded or failed response accepted")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, _, err := s.get(ctx, origin.URL+"/wait", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if _, _, _, err := s.get(context.Background(), "file:///secret", nil); err == nil {
		t.Fatal("non-web URL accepted")
	}
}

type captureOpenCLIBackend struct {
	commandBackendFixture
	request harness.CommandRequest
}

func (b *captureOpenCLIBackend) Start(ctx context.Context, r harness.CommandRequest) (harness.CommandSnapshot, error) {
	b.request = r
	return b.commandBackendFixture.Start(ctx, r)
}
func TestOpenCLIArgumentsAndFailedReceipt(t *testing.T) {
	exit := 7
	b := &captureOpenCLIBackend{commandBackendFixture: commandBackendFixture{snapshot: harness.CommandSnapshot{State: harness.CommandFailed, ExitCode: &exit, TerminationConfirmed: true, Stdout: harness.CommandOutput{Text: "partial", Truncated: true}}}}
	item, err := OpenCLITool(b, opencli.Launcher{Executable: "/fixed", Prefix: []string{"fixed.js"}}, []string{"web"})
	if err != nil {
		t.Fatal(err)
	}
	literal := `a & echo PWNED $(danger) %PATH% "quoted"`
	input, _ := json.Marshal(map[string]any{"site": "web", "command": "fetch", "arguments": []string{literal}})
	out, err := item.InvokableRun(context.Background(), string(input))
	var receipt interface{ ToolResult() string }
	if err == nil || !errors.As(err, &receipt) || receipt.ToolResult() != out || !strings.Contains(out, `"exit_code":7`) || !strings.Contains(out, `"stdout_truncated":true`) {
		t.Fatalf("receipt %s %v", out, err)
	}
	if !reflect.DeepEqual(b.request.Args, []string{"fixed.js", "web", "fetch", literal, "--format", "json"}) || b.request.Executable != "/fixed" || b.request.Script != "" {
		t.Fatal(b.request)
	}
	starts := b.starts
	for _, raw := range []string{`{"site":"github","command":"create"}`, `{"site":"web","command":"--help"}`, `{"site":"web","command":"fetch","executable":"evil"}`, `{"site":"web","command":"fetch"} {}`} {
		if _, err := item.InvokableRun(context.Background(), raw); err == nil {
			t.Fatal("untrusted input accepted")
		}
	}
	if b.starts != starts {
		t.Fatal("rejected input started a process")
	}
}

func TestInstalledOpenCLIThroughGoOwnedBackend(t *testing.T) {
	configured := os.Getenv("DEERFLOW_TEST_OPENCLI")
	if configured == "" {
		t.Skip("set DEERFLOW_TEST_OPENCLI for a local installed-CLI smoke")
	}
	plan, err := opencli.Resolve(configured)
	if err != nil {
		t.Fatal(err)
	}
	backend, err := sandbox.New(context.Background(), harness.SandboxConfig{Enabled: true, Provider: harness.SandboxLocal, AllowedExecutables: []string{plan.Executable}, Environment: plan.Environment, Limits: harness.CommandLimits{Timeout: 30 * time.Second, OutputBytes: 2 << 20}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	result, err := executeCommand(context.Background(), backend, harness.CommandRequest{Executable: plan.Executable, Args: append(plan.Prefix, "list", "--format", "json")})
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid([]byte(result.Stdout.Text)) {
		t.Fatal("installed OpenCLI did not return JSON")
	}
	t.Logf("installed OpenCLI local list succeeded: %d output bytes, no browser command", len(result.Stdout.Text))
}
