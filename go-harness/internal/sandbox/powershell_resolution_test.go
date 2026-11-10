package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestPowerShellExplicitMissingPathCannotFallBack(t *testing.T) {
	_, err := resolvePowerShell(filepath.Join(t.TempDir(), "missing-pwsh.exe"))
	if !errors.Is(err, harness.ErrSandboxUnavailable) {
		t.Fatal("missing explicit runtime was silently replaced", err)
	}
}

func TestPowerShellDiscoveryFallsBackToSystemExecutable(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows system PowerShell fallback")
	}
	t.Setenv("PATH", t.TempDir())
	got, err := resolvePowerShell("")
	want, wantErr := absoluteExecutable(filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe"))
	if wantErr != nil || err != nil || got != want {
		t.Fatal("system executable fallback unavailable", got, err, wantErr)
	}
}
