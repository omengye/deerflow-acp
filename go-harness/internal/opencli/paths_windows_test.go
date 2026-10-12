package opencli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/internal/testutil"
)

func TestManagedShimShortPackagePath(t *testing.T) {
	t.Setenv("OPENCLI_BIN", "")
	base := testutil.CanonicalTempDir(t)
	app := filepath.Join(base, "Long OpenCLI Application Directory")
	pkg := filepath.Join(app, "node_modules", "@jackwener", "opencli")
	if err := os.MkdirAll(filepath.Join(pkg, "dist"), 0700); err != nil {
		t.Fatal(err)
	}
	shortApp := testutil.ShortPath(t, app)
	shim := filepath.Join(base, "opencli.cmd")
	script := filepath.Join(pkg, "dist", "main.js")
	for path, body := range map[string]string{
		filepath.Join(pkg, "package.json"): `{"name":"@jackwener/opencli","bin":"dist/main.js"}`,
		script:                             "// fixture", filepath.Join(base, "node.exe"): "fixture",
		shim: "@echo off\r\nREM OPENCLIAPP_MANAGED_SHIM\r\n\"" + filepath.Join(shortApp, "opencli-app.exe") + "\" __opencli_shim %*\r\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := Resolve(shim)
	if err != nil || len(plan.Prefix) != 1 || plan.Prefix[0] != script {
		t.Fatalf("short package path: %+v %v", plan, err)
	}
	if err := os.Remove(script); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside.js")
	if err := os.WriteFile(outside, []byte("// must not execute"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, script); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Resolve(shim); err == nil {
		t.Fatal("short package path allowed a symlink escape")
	}
}
