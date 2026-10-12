package tools

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// Coordinate all native mutations, including overlapping roots and path aliases.
// This does not lock out external editors, shell commands, or MCP servers.
var workspaceMutationMu sync.Mutex

func fileVersion(content string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
}

func readVersionedFile(ctx context.Context, root *os.Root, path string) (string, error) {
	content, err := readRootFile(ctx, root, path)
	if err != nil {
		return "", err
	}
	return marshalString(struct {
		Version string `json:"version"`
		Content string `json:"content"`
	}{fileVersion(content), content})
}

// An omitted version means create-only, never an unconditional overwrite.
// Arguments (including the expected version) remain part of the approval receipt.
func mutateVersionedFile(ctx context.Context, root *os.Root, path, expected string, change func(string, bool) (string, error)) (string, error) {
	workspaceMutationMu.Lock()
	defer workspaceMutationMu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", harness.MarkToolNotExecuted(err)
	}
	content, err := readRootFile(ctx, root, path)
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", harness.MarkToolNotExecuted(err)
	}
	actual := "absent"
	if exists {
		actual = fileVersion(content)
	}
	if expected == "" {
		expected = "absent"
	}
	if actual != expected {
		return "", harness.MarkToolNotExecuted(errors.New("file_version_conflict: file changed or expected_version is missing; read_file again and use its version before editing an existing file; no write performed"))
	}
	next, err := change(content, exists)
	if err != nil {
		return "", harness.MarkToolNotExecuted(err)
	}
	if err = writeRootFile(ctx, root, path, next); err != nil {
		return "", err
	}
	return marshalString(map[string]any{"written": true, "path": path, "version": fileVersion(next)})
}
