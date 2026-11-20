package tools

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
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

const maxRangedFileBytes = 8 << 20
const defaultReadLines = 400
const maxReadRangeBytes = 256 << 10

// readVersionedFileRange returns a line range. The version still identifies the
// complete file, so a ranged read can authorize a later write or edit.
func readVersionedFileRange(ctx context.Context, root *os.Root, path string, offset, limit int) (string, error) {
	if offset < 0 || limit < 0 {
		return "", errors.New("offset and limit must not be negative")
	}
	if offset == 0 {
		offset = 1
	}
	if limit == 0 {
		limit = defaultReadLines
	}
	content, err := readRootFileLimit(ctx, root, path, maxRangedFileBytes)
	if err != nil {
		return "", err
	}
	lines := strings.SplitAfter(content, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	result := struct {
		Version    string `json:"version"`
		StartLine  int    `json:"startLine"`
		EndLine    int    `json:"endLine"`
		TotalLines int    `json:"totalLines"`
		Truncated  bool   `json:"truncated,omitempty"`
		Content    string `json:"content"`
	}{Version: fileVersion(content), StartLine: offset, EndLine: offset - 1, TotalLines: len(lines)}
	rangeStart, rangeEnd := 0, 0
	for i := 0; i < offset-1 && i < len(lines); i++ {
		rangeStart += len(lines[i])
	}
	rangeEnd = rangeStart
	for i := offset - 1; i >= 0 && i < len(lines) && i < offset-1+limit; i++ {
		if (rangeEnd-rangeStart)+len(lines[i]) > maxReadRangeBytes {
			result.Truncated = true
			break
		}
		rangeEnd += len(lines[i])
		result.EndLine = i + 1
	}
	result.Content = secrets.MaskRange(content, rangeStart, rangeEnd)
	return marshalString(result)
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
