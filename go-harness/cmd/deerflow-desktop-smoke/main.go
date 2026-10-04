// Command deerflow-desktop-smoke validates a copied Windows Waku/Go package
// over its actual WebSocket and ACP transports. It never opens the desktop UI,
// uses remote providers, or reads/modifies the supplied package's user data.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type smoke struct {
	packagePath, cache, root, logs, workspace, config, runtime, executable string
	env, bridgeArgs                                                        []string
	processes                                                              []*jsonProcess
	waku                                                                   *wakuClient
	fake, alt                                                              *fakeModel
	report                                                                 obj
	mu                                                                     sync.Mutex
	legacy                                                                 bool
}

func below(path, root string) bool {
	rel, e := filepath.Rel(root, path)
	return e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
func read(path string) string { b, e := os.ReadFile(path); must(e); return string(b) }
func exists(path string) bool { _, e := os.Stat(path); return e == nil }
func (s *smoke) log(check string, detail obj) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := clone(detail)
	entry["time"] = time.Now().Format(time.RFC3339)
	entry["check"] = check
	s.report["checks"] = append(s.report["checks"].([]any), entry)
	f, e := os.OpenFile(filepath.Join(s.logs, "checks.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	must(e)
	_, e = f.Write(append(jsonBytes(entry), '\n'))
	f.Close()
	must(e)
	fmt.Println(string(jsonBytes(entry)))
}
func copyFile(source, destination string) {
	info, e := os.Lstat(source)
	must(e)
	need(info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0, "not an immutable regular file: %s", source)
	must(os.MkdirAll(filepath.Dir(destination), 0700))
	src, e := os.Open(source)
	must(e)
	defer src.Close()
	dst, e := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	must(e)
	_, e = io.Copy(dst, src)
	closeErr := dst.Close()
	must(e)
	must(closeErr)
}
func copyTree(source, destination string) {
	must(filepath.WalkDir(source, func(path string, entry os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if entry.Type()&os.ModeSymlink != 0 || entry.Type()&os.ModeIrregular != 0 {
			return fmt.Errorf("package reparse/special entry: %s", path)
		}
		rel, e := filepath.Rel(source, path)
		if e != nil {
			return e
		}
		dest := filepath.Join(destination, rel)
		if entry.IsDir() {
			return os.MkdirAll(dest, 0700)
		}
		copyFile(path, dest)
		return nil
	}))
}
func (s *smoke) environment() []string {
	keep := map[string]bool{}
	for _, key := range []string{"SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT", "NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE", "PROGRAMFILES", "PROGRAMFILES(X86)", "PROGRAMDATA", "OS"} {
		keep[key] = true
	}
	values := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if keep[strings.ToUpper(k)] {
			values[strings.ToUpper(k)] = v
		}
	}
	profile := filepath.Join(s.root, "smoke-profile")
	for _, name := range []string{"home", "temp", "roaming", "local", "cache", "config"} {
		must(os.MkdirAll(filepath.Join(profile, name), 0700))
	}
	for k, v := range map[string]string{"PATH": s.root + string(os.PathListSeparator) + filepath.Join(values["SYSTEMROOT"], "System32"), "HOME": filepath.Join(profile, "home"), "USERPROFILE": filepath.Join(profile, "home"), "APPDATA": filepath.Join(profile, "roaming"), "LOCALAPPDATA": filepath.Join(profile, "local"), "XDG_CONFIG_HOME": filepath.Join(profile, "config"), "XDG_CACHE_HOME": filepath.Join(profile, "cache"), "TEMP": filepath.Join(profile, "temp"), "TMP": filepath.Join(profile, "temp"), "DEER_FLOW_PORTABLE_ROOT": s.root, "DEER_FLOW_CONFIG_PATH": s.config, "DEER_FLOW_ACP_RUNTIME_DIR": s.runtime, "WAKU_APP_EXECUTABLE": filepath.Join(s.root, "deerflow-desktop.exe"), "DEERFLOW_GO_DATA_DIR": filepath.Join(s.root, "user-data", "data", "go-harness"), "NO_PROXY": "127.0.0.1,localhost"} {
		values[k] = v
	}
	var out []string
	for k, v := range values {
		out = append(out, k+"="+v)
	}
	return out
}
func (s *smoke) process(args []string, env []string, name string) *jsonProcess {
	p := startProcess(args, env, s.root, s.logs, name)
	s.processes = append(s.processes, p)
	return p
}
func (s *smoke) startWaku(token, name string) *jsonProcess {
	p := s.process([]string{filepath.Join(s.root, "waku-daemon.exe"), "--bind", "127.0.0.1:0", "--parent-pid", fmt.Sprint(os.Getpid())}, append(append([]string(nil), s.env...), "WAKU_DAEMON_TOKEN="+token), name)
	ready := p.receive(30 * time.Second)
	need(ready["pid"] == float64(p.cmd.Process.Pid), "readiness PID mismatch")
	s.waku = connectWaku(ready, token, s.log)
	return p
}
func (s *smoke) acp(name string) *acpClient {
	c := &acpClient{process: s.process(s.bridgeArgs, s.env, name)}
	c.initialize()
	return c
}
func (s *smoke) settled() obj {
	deadline := time.Now().Add(150 * time.Second)
	for time.Now().Before(deadline) {
		state := s.waku.deerflow("status")
		need(str(state["apply_error"]) == "", "service apply failed: %v", state["apply_error"])
		if state["applying"] != true && state["running"] == true {
			return state
		}
		time.Sleep(500 * time.Millisecond)
	}
	panic(fmt.Errorf("service did not settle"))
}
func (s *smoke) prepare() {
	need(runtime.GOOS == "windows", "this smoke command targets the Windows portable package")
	for _, name := range []string{"resources/default-config.yaml", "waku-daemon.exe", "deerflow-acp.exe", "deerflow-desktop.exe", "deerflow-acpd.exe", "deerflow-config-go.exe"} {
		need(exists(filepath.Join(s.packagePath, name)), "incomplete package: %s", name)
	}
	must(filepath.WalkDir(s.packagePath, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if path == s.packagePath {
			return nil
		}
		if d.IsDir() && d.Name() == "user-data" {
			return filepath.SkipDir
		}
		name := strings.ToLower(d.Name())
		ext := strings.ToLower(filepath.Ext(name))
		if name == "runtime" || name == ".venv" || name == "site-packages" || name == "python.exe" || name == "pythonw.exe" || ext == ".py" || ext == ".pyw" || ext == ".pyc" || ext == ".pyo" {
			return fmt.Errorf("package contains Python component: %s", path)
		}
		return nil
	}))
	copyTree(filepath.Join(s.packagePath, "resources"), filepath.Join(s.root, "resources"))
	for _, name := range []string{"waku-daemon.exe", "deerflow-acp.exe", "deerflow-desktop.exe", "deerflow-acpd.exe", "deerflow-config-go.exe"} {
		copyFile(filepath.Join(s.packagePath, name), filepath.Join(s.root, name))
	}
	// The fixture executable is separate from the product package and invoked
	// explicitly, so the package itself does not gain a test-only dependency.
	self, e := os.Executable()
	must(e)
	s.executable = filepath.Join(s.root, "smoke-mcp-fixture.exe")
	copyFile(self, s.executable)
	s.env = s.environment()
	s.bridgeArgs = []string{filepath.Join(s.root, "deerflow-acp.exe"), "--config", s.config, "--daemon", filepath.Join(s.root, "deerflow-acpd.exe"), "--runtime-dir", s.runtime}
	must(os.MkdirAll(s.workspace, 0700))
	s.log("Copied immutable package allowlist", obj{})
}
func (s *smoke) configure() obj {
	w := s.waku
	snapshot := w.deerflow("snapshot")
	need(below(str(object(snapshot["paths"])["config"]), s.root), "snapshot config escaped isolated package")
	need(len(array(snapshot["models"])) > 0, "default snapshot has no model")
	need(w.deerflow("status")["acp_backend"] == "go", "wrong ACP backend")
	raw := read(s.config)
	need(strings.Contains(raw, "  accept_client_mcp_servers: false") && strings.Contains(raw, "  client_mcp_allowed_commands: []"), "default config lacks host MCP policy")
	raw = strings.Replace(raw, "  accept_client_mcp_servers: false", "  accept_client_mcp_servers: true", 1)
	if s.legacy {
		raw = strings.Replace(raw, "  client_mcp_allowed_commands: []", "", 1)
	} else {
		raw = strings.Replace(raw, "  client_mcp_allowed_commands: []", "  client_mcp_allowed_commands: ["+string(jsonBytes(s.executable))+"]", 1)
	}
	must(os.WriteFile(s.config, []byte(raw), 0600))
	document := clone(w.deerflow("snapshot"))
	for _, v := range array(document["models"]) {
		m := object(v)
		m["api_key"] = ""
		m["clear_api_key"] = true
		m["base_url"] = "http://127.0.0.1:9/v1"
	}
	object(document["memory"])["enabled"] = true
	object(document["skill_evolution"])["enabled"] = false
	document["skills_enabled"] = true
	object(document["subagents"])["enabled"] = false
	need(w.deerflow("validate", document)["valid"] == true, "no-key validation failed")
	saved := w.deerflow("save", document)
	for _, m := range array(saved["models"]) {
		need(object(m)["api_key_configured"] != true, "no-key save retained a credential")
	}
	need(w.deerflow("status")["running"] != true, "fresh isolated service unexpectedly running")
	invalid := clone(saved)
	object(array(invalid["models"])[0])["name"] = ""
	need(safe(func() { w.deerflow("save-and-apply", invalid) }) != nil, "invalid save accepted")
	need(w.deerflow("status")["running"] != true, "invalid save started runtime")
	need(w.deerflow("snapshot")["config_revision"] == saved["config_revision"], "invalid save changed config")
	s.log("Invalid save-and-apply preserves config and stopped runtime", obj{})
	s.fake = newFakeModel(filepath.Join(s.logs, "model-requests.jsonl"), "smoke-local-placeholder")
	s.alt = newFakeModel(filepath.Join(s.logs, "alt-model-requests.jsonl"), "smoke-alt-secret")
	document = clone(saved)
	models := []any{}
	for _, spec := range []struct{ name, display, key, url string }{{"smoke-local", "Local smoke model", "smoke-local-placeholder", s.fake.server.URL}, {"smoke-alt", "Alternate local model", "smoke-alt-secret", s.alt.server.URL}} {
		models = append(models, obj{"original_name": "", "name": spec.name, "display_name": spec.display, "description": "Isolated local smoke fixture", "provider": "openai", "model": spec.name, "api_key": spec.key, "clear_api_key": false, "base_url": spec.url + "/v1", "supports_vision": false, "context_window": 0})
	}
	document["models"] = models
	document["default_model"] = "smoke-local"
	object(document["runtime"])["model_name"] = "smoke-local"
	return w.deerflow("save", document)
}
func (s *smoke) cleanup() {
	failures := []any{}
	record := func(name string, fn func()) {
		if e := safe(fn); e != nil {
			failures = append(failures, name+": "+e.Error())
		}
	}
	if s.waku != nil {
		record("Waku shutdown", func() { s.waku.deerflow("cancel-apply"); s.waku.ws.send(obj{"type": "shutdown"}) })
		s.waku.ws.conn.Close()
	}
	for i := len(s.processes) - 1; i >= 0; i-- {
		p := s.processes[i]
		record(p.cmd.Path, func() { p.stop() })
	}
	if len(s.bridgeArgs) > 0 {
		record("Go daemon stop", func() {
			cmd := exec.Command(s.bridgeArgs[0], append(s.bridgeArgs[1:], "--stop-daemon")...)
			cmd.Dir = s.root
			cmd.Env = s.env
			hideWindow(cmd)
			done := make(chan error, 1)
			var output strings.Builder
			cmd.Stdout = &output
			cmd.Stderr = &output
			must(cmd.Start())
			go func() { done <- cmd.Wait() }()
			select {
			case e := <-done:
				must(os.WriteFile(filepath.Join(s.logs, "cleanup-bridge.log"), []byte(output.String()), 0600))
				if e != nil && exists(filepath.Join(s.runtime, "endpoint.json")) {
					must(e)
				}
			case <-time.After(40 * time.Second):
				cmd.Process.Kill()
				<-done
				panic(fmt.Errorf("stop timed out"))
			}
		})
	}
	if s.fake != nil {
		s.fake.server.Close()
	}
	if s.alt != nil {
		s.alt.server.Close()
	}
	s.report["cleanup_errors"] = failures
	if len(failures) > 0 {
		s.report["ok"] = false
	}
}
func main() {
	var packagePath, cache string
	var fixture, legacy bool
	flag.StringVar(&packagePath, "package", "", "fresh extracted Windows Go desktop package")
	flag.StringVar(&cache, "cache", ".build-cache/desktop-smoke", "isolated output root; must not be inside the supplied package")
	flag.BoolVar(&fixture, "mcp-fixture", false, "run the isolated stdio MCP fixture")
	flag.BoolVar(&legacy, "legacy-empty-mcp-policy", false, "verify legacy enabled MCP with no executable allowlist")
	flag.Parse()
	if fixture {
		if e := serveMCP(os.Stdin, os.Stdout); e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
		return
	}
	if packagePath == "" {
		flag.Usage()
		os.Exit(2)
	}
	absolute, e := filepath.Abs(packagePath)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(2)
	}
	cache, e = filepath.Abs(cache)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(2)
	}
	root := filepath.Join(cache, time.Now().Format("20060102-150405")+"-"+uuid(), "package")
	if !below(root, cache) || below(root, absolute) {
		fmt.Fprintln(os.Stderr, "unsafe smoke output location")
		os.Exit(2)
	}
	s := &smoke{packagePath: absolute, cache: cache, root: root, logs: filepath.Join(filepath.Dir(root), "logs"), workspace: filepath.Join(root, "smoke-workspace"), config: filepath.Join(root, "user-data", "config", "config.yaml"), runtime: filepath.Join(root, "user-data", "runtime", "acp-go"), legacy: legacy, report: obj{"package": absolute, "smoke_root": root, "ok": false, "checks": []any{}, "cleanup_errors": []any{}}}
	if e = os.MkdirAll(s.logs, 0700); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	fmt.Println("Isolated smoke logs:", s.logs)
	err := safe(func() { s.prepare(); s.run(); s.report["ok"] = true })
	if err != nil {
		s.report["error"] = err.Error()
		os.WriteFile(filepath.Join(s.logs, "failure.txt"), []byte(err.Error()), 0600)
	}
	s.cleanup()
	e = os.WriteFile(filepath.Join(s.logs, "report.json"), jsonBytes(s.report), 0600)
	fmt.Println(string(jsonBytes(obj{"ok": s.report["ok"], "logs": s.logs, "error": s.report["error"], "cleanup_errors": s.report["cleanup_errors"]})))
	if e != nil || s.report["ok"] != true {
		os.Exit(1)
	}
}
