package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestWSLHelperMissingOrWrongChecksumRefusesStartup(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows WSL host")
	}
	cwd := t.TempDir()
	helper := filepath.Join(cwd, "fixture-helper")
	if err := os.WriteFile(helper, []byte("fixture helper"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, digest string }{
		{helper + "-missing", strings.Repeat("0", 64)}, {helper, strings.Repeat("0", 64)}, {helper, ""},
	} {
		cfg := harness.SandboxConfig{Enabled: true, Provider: harness.SandboxWSL2, WSL: harness.WSLSandboxConfig{Distribution: "explicit-unused-distro", HelperPath: tc.path, HelperSHA256: tc.digest}}
		b, err := New(context.Background(), cfg, cwd)
		if b != nil {
			_ = b.Close()
			t.Fatal("invalid helper started a backend")
		}
		if !errors.Is(err, harness.ErrSandboxUnavailable) {
			t.Fatalf("missing/checksum error not actionable: %v", err)
		}
	}
}

func TestWSLHelperChangesRefuseSubsequentCommands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "helper")
	data := []byte("fixture-a")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	hash := sha256.Sum256(data)
	b := &Backend{helper: file, cfg: harness.SandboxConfig{WSL: harness.WSLSandboxConfig{HelperPath: path, HelperSHA256: hex.EncodeToString(hash[:])}}}
	if err := b.checkHelper(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("fixture-b"), 0600); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(b.checkHelper(), harness.ErrSandboxUnavailable) {
		t.Fatal("changed helper retained execution authority")
	}
}
