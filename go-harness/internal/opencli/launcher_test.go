package opencli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestManagedWindowsShimResolvesNodeWithoutShell(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows shim")
	}
	t.Setenv("OPENCLI_BIN", "")
	base := t.TempDir()
	app := filepath.Join(base, "Program Files", "OpenCLIApp")
	shim := filepath.Join(base, "opencli.cmd")
	pkg := filepath.Join(app, "node_modules", "@jackwener", "opencli")
	if err := os.MkdirAll(filepath.Join(pkg, "dist"), 0700); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{filepath.Join(pkg, "package.json"): `{"name":"@jackwener/opencli","bin":{"opencli":"dist/main.js"}}`, filepath.Join(pkg, "dist", "main.js"): "// fixture", filepath.Join(base, "node.exe"): "fixture", shim: "@echo off\r\nREM OPENCLIAPP_MANAGED_SHIM\r\n\"" + filepath.Join(app, "opencli-app.exe") + "\" __opencli_shim %*\r\n"} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := Resolve(shim)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(plan.Executable) != "node.exe" || len(plan.Prefix) != 1 || !strings.HasSuffix(plan.Prefix[0], "main.js") {
		t.Fatalf("bad plan %+v", plan)
	}
	if _, ok := plan.Environment["MODEL_API_KEY"]; ok {
		t.Fatal("inherited arbitrary host credentials")
	}
	if err := os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{"name":"@jackwener/opencli","bin":{"opencli":"../../evil.js"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(shim); err == nil {
		t.Fatal("npm entry escapes package")
	}
}

func TestOpenCLIEnvironmentOverride(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCLI_BIN", exe)
	t.Setenv("MODEL_API_KEY", "do-not-inherit")
	t.Setenv("OPENCLI_CONFIG_DIR", t.TempDir())
	plan, err := Resolve("missing-configured-path")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Executable != exe || plan.Environment["OPENCLI_CONFIG_DIR"] == "" {
		t.Fatal("host override ignored")
	}
	if _, ok := plan.Environment["MODEL_API_KEY"]; ok {
		t.Fatal("credential leak")
	}
}
