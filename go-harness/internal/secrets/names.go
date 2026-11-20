// Package secrets implements the harness secret vault: encrypted named values
// that agents manage through host tools and use by reference in commands and
// skills. Plaintext never needs to enter model context, receipts or history;
// known values are masked as [secret:NAME] markers wherever tool output flows.
package secrets

import (
	"errors"
	"fmt"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

const (
	MaxNameBytes        = 128
	MaxStringBytes      = 64 << 10
	MaxFileBytes        = 1 << 20
	MaxDescriptionBytes = 512
	MaxRevealBytes      = 6 << 10
	MaxSecrets          = 512
	RetainedVersions    = 5
	// Soft-deleted secrets stay restorable (and masked) for this many days.
	DeletedRetentionDays = 30
	// Shorter values are not masked as substrings: they would corrupt
	// unrelated output and give no meaningful protection.
	MinMaskBytes = 8
	// OutEnv names the per-command write-back directory.
	OutEnv = harness.SecretOutEnv
)

var (
	ErrNotFound    = errors.New("secret not found")
	ErrInvalid     = errors.New("invalid secret request")
	ErrUnavailable = errors.New("secret value is unavailable: the vault key changed (for example after a Windows user or machine change); set it again")
	ErrClosed      = errors.New("secret vault is closed")
)

type Kind string

const (
	KindString Kind = "string"
	KindFile   Kind = "file"
)

// ValidName reports whether name is an uppercase environment-compatible
// identifier. Uppercase-only names avoid case-insensitive collisions on
// Windows and can be used directly as environment variable names.
func ValidName(name string) bool {
	if name == "" || len(name) > MaxNameBytes {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == '_' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9' {
			continue
		}
		return false
	}
	return true
}

func validateName(name string) error {
	if !ValidName(name) {
		return fmt.Errorf("%w: name must match [A-Z_][A-Z0-9_]* and be at most %d bytes", ErrInvalid, MaxNameBytes)
	}
	return nil
}

// reservedEnv are host-owned variables a secret mapping must never replace.
var reservedEnv = []string{
	"PATH", "PATHEXT", "SYSTEMROOT", "WINDIR", "COMSPEC", "SYSTEMDRIVE",
	"APPDATA", "LOCALAPPDATA", "PROGRAMDATA", "PSMODULEPATH",
	"TEMP", "TMP", "TMPDIR", "HOME", "USERPROFILE", "XDG_CONFIG_HOME",
	"LANG", "LC_ALL", "SHELL", "PWD", "BASH_ENV", "ENV", "ZDOTDIR",
	"LD_PRELOAD", "LD_LIBRARY_PATH", "DYLD_INSERT_LIBRARIES",
	"PYTHONPATH", "PYTHONHOME", "NODE_OPTIONS",
	"WSLENV", "SSH_AUTH_SOCK", "SSL_CERT_FILE", "SSL_CERT_DIR",
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
	"GIT_SSH_COMMAND", "GIT_EXEC_PATH",
}

// ValidEnvName validates a model-selected injection variable name. Names are
// compared case-insensitively against reserved host variables because
// Windows environments are case-insensitive.
func ValidEnvName(name string) error {
	if name == "" || len(name) > MaxNameBytes {
		return fmt.Errorf("%w: environment variable name is empty or too long", ErrInvalid)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || i > 0 && c >= '0' && c <= '9') {
			// An invalid identifier may be a value put in the wrong field: never echo it.
			return fmt.Errorf("%w: an environment variable name must match [A-Za-z_][A-Za-z0-9_]* (map environment variable -> secret name, never a value)", ErrInvalid)
		}
	}
	upper := strings.ToUpper(name)
	for _, prefix := range []string{"DEERFLOW_", "DOCKER_", "WSL_"} {
		if strings.HasPrefix(upper, prefix) {
			return fmt.Errorf("%w: environment variable %q uses the reserved %s prefix", ErrInvalid, name, prefix)
		}
	}
	for _, reserved := range reservedEnv {
		if upper == reserved {
			return fmt.Errorf("%w: environment variable %q is reserved by the host", ErrInvalid, name)
		}
	}
	return nil
}

// ValidateBindings checks an ENV->SECRET mapping supplied to a command tool.
func ValidateBindings(bindings map[string]string) error {
	if len(bindings) > 32 {
		return fmt.Errorf("%w: at most 32 secrets may be injected into one command", ErrInvalid)
	}
	seen := make(map[string]string, len(bindings))
	for env, name := range bindings {
		if err := ValidEnvName(env); err != nil {
			return err
		}
		if err := validateName(name); err != nil {
			return fmt.Errorf("%w; secrets maps an environment variable to a secret name, never to a value", err)
		}
		upper := strings.ToUpper(env)
		if other, ok := seen[upper]; ok {
			return fmt.Errorf("%w: environment variables %q and %q differ only by case", ErrInvalid, other, env)
		}
		seen[upper] = env
	}
	return nil
}
