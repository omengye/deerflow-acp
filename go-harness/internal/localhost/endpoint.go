// Package localhost implements the authenticated DFACP/1 loopback transport used
// by the Rust Bridge. It deliberately exposes no HTTP server.
package localhost

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

const EndpointFilename = "endpoint.json"

// Endpoint is wire compatible with bridge/src/main.rs and daemon_endpoint.py.
// Token is a local bearer credential; callers must never log this structure.
type Endpoint struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Token      string `json:"token"`
	PID        int    `json:"pid"`
	BuildID    string `json:"build_id"`
	ConfigPath string `json:"config_path"`
}

// DefaultRuntimeDir intentionally differs from the Python runtime directory.
// Existing Bridge invocations must opt in using --daemon and --runtime-dir.
func DefaultRuntimeDir() (string, error) {
	if dir := os.Getenv("DEERFLOW_GO_RUNTIME_DIR"); dir != "" {
		return dir, nil
	}
	if runtime.GOOS == "windows" {
		if dir := os.Getenv("LOCALAPPDATA"); dir != "" {
			return filepath.Join(dir, "DeerFlow", "go-acp"), nil
		}
	} else if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "deerflow-go-acp"), nil
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "deerflow-go", "acp"), nil
}

func prepareRuntimeDir(dir string) (string, error) {
	if dir == "" {
		var err error
		dir, err = DefaultRuntimeDir()
		if err != nil {
			return "", err
		}
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	if err = securePath(dir, true); err != nil {
		return "", fmt.Errorf("protect runtime directory: %w", err)
	}
	return dir, nil
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func publishEndpoint(dir string, ep Endpoint) (err error) {
	b, err := json.Marshal(ep)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".endpoint-*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() { _ = f.Close(); _ = os.Remove(name) }()
	if err = securePath(name, false); err != nil {
		return err
	}
	if _, err = f.Write(append(b, '\n')); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(name, filepath.Join(dir, EndpointFilename))
}

// removeOwnedEndpoint never removes an endpoint published by a successor.
// The runtime lock remains held for the entire read/compare/remove operation.
func removeOwnedEndpoint(dir string, owned Endpoint) error {
	name := filepath.Join(dir, EndpointFilename)
	b, err := os.ReadFile(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var current Endpoint
	if err = json.Unmarshal(b, &current); err != nil {
		return fmt.Errorf("endpoint changed during shutdown; preserving it")
	}
	if current.PID != owned.PID || current.Token != owned.Token {
		return nil
	}
	return os.Remove(name)
}
