package client

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func requireSessionPersistenceFailure(t *testing.T, err error) {
	t.Helper()
	var persistence interface{ PersistenceFailure() bool }
	if !errors.As(err, &persistence) || !persistence.PersistenceFailure() {
		t.Fatalf("local session error lost persistence marker: %v", err)
	}
}

func TestSessionStateIOFailuresKeepPersistenceMarker(t *testing.T) {
	dir := t.TempDir()
	_, err := readSessionState(filepath.Join(dir, "missing.json"))
	requireSessionPersistenceFailure(t, err)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing state can no longer create a new remote session")
	}
	// A directory in place of the destination forces the atomic state commit
	// to fail on every supported OS, without depending on host permissions.
	requireSessionPersistenceFailure(t, writeSessionState(dir, []byte(`{}`)))
	large := filepath.Join(dir, "large.json")
	if err := os.WriteFile(large, []byte(strings.Repeat("x", 4097)), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = readSessionState(large)
	requireSessionPersistenceFailure(t, err)
}

func TestInvokeToolCorruptStateStopsBeforeRemoteDispatch(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{`{`, `{"session_id":"","policy":"old"}`} {
		t.Run(state, func(t *testing.T) {
			root := t.TempDir()
			identity := sha256.Sum256([]byte("corrupt-parent"))
			file := filepath.Join(root, "acp-agent-sessions", fmt.Sprintf("%x", identity[:]), "fixture.json")
			if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte(state), 0600); err != nil {
				t.Fatal(err)
			}
			invoke, err := Tool(root, harness.Session{ID: "corrupt-parent"}, map[string]harness.ACPAgentConfig{"fixture": {Command: executable}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = invoke.InvokableRun(context.Background(), `{"agent":"fixture","prompt":"work"}`)
			requireSessionPersistenceFailure(t, err)
			body, err := os.ReadFile(file)
			if err != nil || string(body) != state {
				t.Fatalf("corrupt state was reset: %q %v", body, err)
			}
		})
	}
}
