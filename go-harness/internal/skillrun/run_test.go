package skillrun

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/sandbox"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func sourceFixture(t *testing.T, script string, packages ...string) (*skills.Snapshot, string, string) {
	t.Helper()
	ctx := context.Background()
	ws := t.TempDir()
	source := filepath.Join(t.TempDir(), "sample")
	if err := os.MkdirAll(filepath.Join(source, "scripts"), 0700); err != nil {
		t.Fatal(err)
	}
	for p, v := range map[string]string{"SKILL.md": "---\nname: sample\ndescription: Run a source fixture\n---\nUse skill_run.\n", "scripts/run.py": script, "skill-runtime.json": `{"version":1,"python":{"min_version":"3.8","packages":[],"entrypoints":["scripts/run.py"]}}`} {
		if err := os.WriteFile(filepath.Join(source, p), []byte(v), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if len(packages) > 0 {
		manifest, _ := json.Marshal(skills.RuntimeManifest{Version: 1, Python: skills.PythonRequirements{MinVersion: "3.8", Packages: packages, Entrypoints: []string{"scripts/run.py"}}})
		if err := os.WriteFile(filepath.Join(source, "skill-runtime.json"), manifest, 0600); err != nil {
			t.Fatal(err)
		}
	}
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "skill.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	reg, err := skills.NewRegistry(ctx, harness.SkillsConfig{Sources: []harness.SkillSource{{ID: "test", Root: filepath.Dir(source), Scope: harness.SkillScopeGlobal}}}, db.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close(); _ = db.Close() })
	if err = reg.InstallConfigured(ctx, "test", "sample", true); err != nil {
		t.Fatal(err)
	}
	snap, err := reg.Snapshot(ctx, harness.SkillSelection{Workspace: ws, IncludeGlobal: true})
	if err != nil {
		t.Fatal(err)
	}
	return snap, ws, source
}

func pythonVenv(t *testing.T) (string, string) {
	t.Helper()
	cfg := harness.SandboxConfig{Enabled: true, Provider: harness.SandboxPowerShell, AllowShell: true}
	py, err := Interpreter(cfg)
	if err != nil {
		t.Skip(err)
	}
	venv := filepath.Join(t.TempDir(), "venv")
	if output, err := exec.Command(py, "-I", "-B", "-m", "venv", "--without-pip", venv).CombinedOutput(); err != nil {
		t.Skipf("venv unavailable: %v %s", err, output)
	}
	exe := filepath.Join(venv, "Scripts", "python.exe")
	if _, err = os.Stat(exe); err != nil {
		exe = filepath.Join(venv, "bin", "python")
	}
	siteJSON, err := exec.Command(exe, "-I", "-B", "-c", "import sysconfig,json; print(json.dumps(sysconfig.get_path('purelib')))").Output()
	if err != nil {
		t.Fatal(err)
	}
	var sitePath string
	if json.Unmarshal(siteJSON, &sitePath) != nil || sitePath == "" {
		t.Fatal(string(siteJSON))
	}
	return exe, sitePath
}

func writePythonDistribution(t *testing.T, sitePath, name, version string) {
	t.Helper()
	dir := filepath.Join(sitePath, strings.ReplaceAll(name, "-", "_")+"-"+version+".dist-info")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "METADATA"), []byte("Metadata-Version: 2.1\nName: "+name+"\nVersion: "+version+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestRealPythonCheckNeverExecutesSiteStartupOrImportsDependencies(t *testing.T) {
	exe, sitePath := pythonVenv(t)
	cfg := harness.SandboxConfig{Enabled: true, Provider: harness.SandboxLocal, AllowedExecutables: []string{exe}, Environment: map[string]string{"DEERFLOW_SKILL_PYTHON": exe}, Limits: harness.CommandLimits{Timeout: 15 * time.Second}}
	snap, ws, _ := sourceFixture(t, "raise Exception('check must not execute')", "fixture-no-import")
	marker := filepath.Join(ws, "startup-executed")
	writePythonDistribution(t, sitePath, "fixture-no-import", "1.0")
	if err := os.WriteFile(filepath.Join(sitePath, "fixture_no_import.py"), []byte("raise Exception('must never import')"), 0600); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(marker)
	for _, name := range []string{"danger.pth", "sitecustomize.py", "usercustomize.py"} {
		if err := os.WriteFile(filepath.Join(sitePath, name), []byte("import pathlib; pathlib.Path("+string(encoded)+").write_text('startup')\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	backend, err := sandbox.New(context.Background(), cfg, ws)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	result, err := Run(context.Background(), snap, backend, cfg, ws, Input{Action: "check", Name: "sample"})
	if err != nil || !result.Readiness.Ready || result.Readiness.Packages["fixture-no-import"] != "1.0" {
		t.Fatal(result, err)
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("readiness executed startup", err)
	}
}

func TestRealPythonRunUsesVenvDependencies(t *testing.T) {
	exe, sitePath := pythonVenv(t)
	writePythonDistribution(t, sitePath, "fixture-venv-only", "1.0")
	if err := os.WriteFile(filepath.Join(sitePath, "fixture_venv_only.py"), []byte("VALUE = 'venv-dependency'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := harness.SandboxConfig{Enabled: true, Provider: harness.SandboxLocal, AllowedExecutables: []string{exe}, Environment: map[string]string{"DEERFLOW_SKILL_PYTHON": exe}, Limits: harness.CommandLimits{Timeout: 15 * time.Second}}
	snap, ws, _ := sourceFixture(t, "import fixture_venv_only\nprint(fixture_venv_only.VALUE)\n", "fixture-venv-only")
	backend, err := sandbox.New(context.Background(), cfg, ws)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	r, err := Run(context.Background(), snap, backend, cfg, ws, Input{Action: "run", Name: "sample", Path: "scripts/run.py"})
	if err != nil || !r.Readiness.Ready || !r.ExecutionChecked || r.Command == nil || strings.TrimSpace(r.Command.Stdout.Text) != "venv-dependency" {
		t.Fatalf("virtual environment lost: %+v %v", r, err)
	}
}

func TestPythonProbeSystemSiteIsolationAndVenvPrecedence(t *testing.T) {
	py, err := Interpreter(harness.SandboxConfig{Enabled: true, Provider: harness.SandboxPowerShell, AllowShell: true})
	if err != nil {
		t.Skip(err)
	}
	root := t.TempDir()
	venv, base, platform := filepath.Join(root, "venv"), filepath.Join(root, "base"), filepath.Join(root, "platform")
	for _, prefix := range []string{venv, base, platform} {
		version := "2.0"
		if prefix == venv {
			version = "1.0"
		}
		writePythonDistribution(t, filepath.Join(prefix, "packages"), "fixture-shared", version)
	}
	writePythonDistribution(t, filepath.Join(base, "packages"), "fixture-base-only", "3.0")
	writePythonDistribution(t, filepath.Join(platform, "packages"), "fixture-platform-only", "4.0")
	fixture, _ := json.Marshal([]string{venv, base, platform})
	// Model both pre-3.14 prefix initialization and Debian's venv-dependent
	// path calculation without installing anything in the host interpreter.
	setup := "import sys,json,site,pathlib\nv,b,p=json.loads(sys.argv.pop()); sys.executable=str(pathlib.Path(v)/'bin'/'python'); sys.prefix=sys.base_prefix=b; sys.exec_prefix=sys.base_exec_prefix=p\nsite.getsitepackages=lambda prefixes: [str(pathlib.Path(x)/'packages') for x in prefixes] if sys.prefix!=sys.base_prefix else []\n"
	input := `{"min_version":"3.8","packages":["Fixture_Shared","fixture-base-only","fixture-platform-only"]}`
	for _, tc := range []struct {
		name                 string
		include, initialized bool
	}{
		{"pre-3.14-isolated", false, false},
		{"pre-3.14-include-system", true, false},
		{"3.14-isolated", false, true},
		{"3.14-include-system", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := "include-system-site-packages = false\n"
			if tc.include {
				config = "  INCLUDE-SYSTEM-SITE-PACKAGES = TRUE  \n"
			}
			if err := os.WriteFile(filepath.Join(venv, "pyvenv.cfg"), []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			code := setup
			if tc.initialized {
				code += "sys.prefix=sys.exec_prefix=v\n"
			}
			output, err := exec.Command(py, "-I", "-S", "-B", "-c", code+probe, input, string(fixture)).CombinedOutput()
			var r Readiness
			if err != nil || json.Unmarshal(output, &r) != nil || r.Ready != tc.include || r.Packages["Fixture_Shared"] != "1.0" {
				t.Fatalf("metadata probe: %s %v", output, err)
			}
			if tc.include {
				if r.Packages["fixture-base-only"] != "3.0" || r.Packages["fixture-platform-only"] != "4.0" || len(r.Missing) != 0 {
					t.Fatalf("system packages not discovered: %+v", r)
				}
			} else if len(r.Packages) != 1 || len(r.Missing) != 2 {
				t.Fatalf("isolated venv leaked system packages: %+v", r)
			}
		})
	}
}

func TestRealPythonCancellationJoinsAndCleansStaging(t *testing.T) {
	cfg := harness.SandboxConfig{Enabled: true, Provider: harness.SandboxPowerShell, AllowShell: true}
	py, err := Interpreter(cfg)
	if err != nil {
		t.Skip(err)
	}
	cfg = harness.SandboxConfig{Enabled: true, Provider: harness.SandboxLocal, AllowedExecutables: []string{py}, Environment: map[string]string{"DEERFLOW_SKILL_PYTHON": py}, Limits: harness.CommandLimits{Timeout: 10 * time.Second}}
	snap, ws, _ := sourceFixture(t, "import pathlib,time\npathlib.Path('started').touch()\ntime.sleep(60)\n")
	backend, err := sandbox.New(context.Background(), cfg, ws)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			if _, err := os.Stat(filepath.Join(ws, "started")); err == nil {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	result, err := Run(ctx, snap, backend, cfg, ws, Input{Action: "run", Name: "sample", Path: "scripts/run.py"})
	<-done
	if !errors.Is(err, context.Canceled) || result.Command == nil || !result.Command.TerminationConfirmed || result.RetainedDirectory != "" {
		t.Fatal(result, err)
	}
	entries, _ := os.ReadDir(ws)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".deerflow-skill-run-") {
			t.Fatal("cancelled staging retained")
		}
	}
}

type fakeBackend struct {
	requests                 []harness.CommandRequest
	workspace                string
	missing, uncertain, fail bool
	code                     string
	releases                 int
}

func (b *fakeBackend) Start(_ context.Context, r harness.CommandRequest) (harness.CommandSnapshot, error) {
	b.requests = append(b.requests, r)
	if len(b.requests) == 2 {
		data, err := os.ReadFile(filepath.Join(b.workspace, filepath.FromSlash(r.Args[4])))
		if err != nil {
			return harness.CommandSnapshot{}, err
		}
		b.code = string(data)
	}
	return harness.CommandSnapshot{ID: "command"}, nil
}
func (b *fakeBackend) Wait(context.Context, string) (harness.CommandSnapshot, error) {
	code := 0
	if b.fail && len(b.requests) == 2 {
		code = 7
	}
	r := harness.CommandSnapshot{ID: "command", State: harness.CommandCompleted, ExitCode: &code, TerminationConfirmed: true}
	if len(b.requests) == 1 {
		r.Stdout.Text = `{"ready":true,"code":"ready","python_version":"3.12.0"}`
		if b.missing {
			r.Stdout.Text = `{"ready":false,"code":"dependencies_missing","missing":["fixture"]}`
		}
	}
	if b.uncertain && len(b.requests) == 2 {
		r.State = harness.CommandUncertain
		r.TerminationConfirmed = false
	}
	return r, nil
}
func (b *fakeBackend) Poll(c context.Context, id string) (harness.CommandSnapshot, error) {
	return b.Wait(c, id)
}
func (b *fakeBackend) Cancel(c context.Context, id string) (harness.CommandSnapshot, error) {
	return b.Wait(c, id)
}
func (b *fakeBackend) Release(context.Context, string) error { b.releases++; return nil }
func (b *fakeBackend) Close() error                          { return nil }

func TestPinnedExecutionReadinessAndTermination(t *testing.T) {
	for _, mode := range []string{"check", "run", "missing", "exit", "uncertain", "entry_denied"} {
		t.Run(mode, func(t *testing.T) {
			snap, ws, source := sourceFixture(t, "print('pinned-v1')")
			if err := os.WriteFile(filepath.Join(source, "scripts/run.py"), []byte("raise Exception('mutable-v2')"), 0600); err != nil {
				t.Fatal(err)
			}
			b := &fakeBackend{workspace: ws, missing: mode == "missing", fail: mode == "exit", uncertain: mode == "uncertain"}
			cfg := harness.SandboxConfig{Enabled: true, Provider: harness.SandboxLocal, AllowedExecutables: []string{"/usr/bin/python3"}}
			in := Input{Action: "run", Name: "sample", Path: "scripts/run.py", Args: []string{"a'b", "$(whoami)", "a b", "\"", ""}}
			if mode == "check" {
				in = Input{Action: "check", Name: "sample"}
			}
			if mode == "entry_denied" {
				in.Path = "scripts/missing.py"
			}
			result, err := Run(context.Background(), snap, b, cfg, ws, in)
			wantError := mode == "missing" || mode == "exit" || mode == "uncertain" || mode == "entry_denied"
			if (err != nil) != wantError {
				t.Fatalf("%+v %v", result, err)
			}
			if mode == "entry_denied" {
				if len(b.requests) != 0 {
					t.Fatal("denied entry ran")
				}
				return
			}
			if mode == "check" || mode == "missing" {
				if len(b.requests) != 1 || result.ExecutionChecked {
					t.Fatal("probe executed skill")
				}
			} else {
				if len(b.requests) != 2 || b.code != "print('pinned-v1')" {
					t.Fatal("did not run pinned source", b.code)
				}
				for i, a := range in.Args {
					if b.requests[1].Args[5+i] != a {
						t.Fatal("argv changed")
					}
				}
			}
			entries, _ := os.ReadDir(ws)
			if mode == "uncertain" {
				if !errors.Is(err, harness.ErrCommandUncertain) || result.RetainedDirectory == "" || len(entries) != 1 || b.releases != 1 {
					t.Fatal("uncertain ownership lost", result)
				}
			} else if len(entries) != 0 {
				t.Fatal("staging leaked")
			}
			if result.ExecutionChecked != (mode == "run") {
				t.Fatal("invalid execution evidence", result)
			}
		})
	}
}

func TestInputAndInterpreterPolicy(t *testing.T) {
	for _, in := range []Input{{Action: "install", Name: "sample"}, {Action: "check", Name: "sample", Args: []string{"-c"}}, {Action: "run", Name: "sample", Path: "../outside.py"}, {Action: "run", Name: "sample", Path: "C:/outside.py"}, {Action: "run", Name: "sample", Path: "run.py", Args: []string{"a\x00b"}}} {
		if ValidateInput(in) == nil {
			t.Fatal("invalid input accepted", in)
		}
	}
	for _, cfg := range []harness.SandboxConfig{{}, {Enabled: true, Provider: harness.SandboxPowerShell}, {Enabled: true, Provider: harness.SandboxLocal, Environment: map[string]string{"DEERFLOW_SKILL_PYTHON": "/outside/python"}, AllowedExecutables: []string{"/bin/python3"}}} {
		if _, err := Interpreter(cfg); err == nil {
			t.Fatal("policy bypass", cfg)
		}
	}
	r := command(harness.SandboxConfig{Provider: harness.SandboxPowerShell}, `C:\Program Files\Python\python.exe`, []string{"a'b", "$(whoami)", ""}, time.Second)
	if !strings.Contains(r.Script, `'a''b' '$(whoami)' ''`) || r.Executable != "" {
		t.Fatal("unsafe quoting", r)
	}
}

func TestRealPythonPinnedSourceAndExit(t *testing.T) {
	for _, provider := range []harness.SandboxProvider{harness.SandboxLocal, harness.SandboxPowerShell} {
		t.Run(string(provider), func(t *testing.T) {
			cfg := harness.SandboxConfig{Enabled: true, Provider: harness.SandboxPowerShell, AllowShell: true, Limits: harness.CommandLimits{Timeout: 10 * time.Second}}
			py, err := Interpreter(cfg)
			if err != nil {
				t.Skip("no authorized local Python:", err)
			}
			cfg.Provider = provider
			cfg.AllowShell = provider == harness.SandboxPowerShell
			cfg.AllowedExecutables = []string{py}
			cfg.Environment = map[string]string{"DEERFLOW_SKILL_PYTHON": py}
			snap, ws, source := sourceFixture(t, "import sys,json,pathlib\npathlib.Path('ran.json').write_text(json.dumps(sys.argv[1:]))\nprint('pinned-v1')\n")
			if err = os.WriteFile(filepath.Join(source, "scripts/run.py"), []byte("raise Exception('v2')"), 0600); err != nil {
				t.Fatal(err)
			}
			backend, err := sandbox.New(context.Background(), cfg, ws)
			if err != nil {
				t.Fatal(err)
			}
			defer backend.Close()
			if r, err := Run(context.Background(), snap, backend, cfg, ws, Input{Action: "check", Name: "sample"}); err != nil || !r.Readiness.Ready {
				t.Fatal(r, err)
			}
			if _, err = os.Stat(filepath.Join(ws, "ran.json")); !os.IsNotExist(err) {
				t.Fatal("check ran script", err)
			}
			args := []string{"a'b", `$(whoami)`, "a b", `"`, ""}
			r, err := Run(context.Background(), snap, backend, cfg, ws, Input{Action: "run", Name: "sample", Path: "scripts/run.py", Args: args})
			if err != nil || !r.ExecutionChecked || !strings.Contains(r.Command.Stdout.Text, "pinned-v1") {
				t.Fatal(r, err)
			}
			data, err := os.ReadFile(filepath.Join(ws, "ran.json"))
			if err != nil {
				t.Fatal(err)
			}
			want, _ := json.Marshal(args)
			var got []string
			if json.Unmarshal(data, &got) != nil {
				t.Fatal(string(data))
			}
			normalized, _ := json.Marshal(got)
			if string(want) != string(normalized) {
				t.Fatal("argv corrupted", string(data))
			}
		})
	}
}
