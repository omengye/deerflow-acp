package tools

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/tool"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
)

const maxFileBytes = 512 << 10

type pathInput struct {
	Path string `json:"path" jsonschema:"description=Path relative to the session workspace"`
}
type readInput struct {
	Path   string `json:"path" jsonschema:"description=Path relative to the session workspace"`
	Offset int    `json:"offset,omitempty" jsonschema:"description=Optional 1-based first line to return; use with limit to page through long or large files"`
	Limit  int    `json:"limit,omitempty" jsonschema:"description=Optional maximum number of lines to return (default 400 when offset is set)"`
}
type writeInput struct {
	Path            string `json:"path"`
	Content         string `json:"content"`
	ExpectedVersion string `json:"expected_version,omitempty" jsonschema:"description=Version from read_file for existing files; absent or omitted means create only"`
}
type editInput struct {
	Path            string `json:"path"`
	OldText         string `json:"old_text"`
	NewText         string `json:"new_text"`
	ExpectedVersion string `json:"expected_version" jsonschema:"description=Version returned by read_file"`
}
type searchInput struct {
	Path string `json:"path"`
	Text string `json:"text"`
}

// WorkspaceFactory pins one directory handle for the entire run. The engine
// must call cleanup after all model and tool work has terminated.
func WorkspaceFactory(ctx context.Context, req harness.RunRequest) ([]tool.BaseTool, func() error, error) {
	return ConfiguredWorkspaceFactory(ctx, req, nil)
}

func ConfiguredWorkspaceFactory(ctx context.Context, req harness.RunRequest, configured []harness.BuiltinToolConfig) ([]tool.BaseTool, func() error, error) {
	cwd, err := session.NormalizeWorkspace(req.Session.CWD)
	if err != nil {
		return nil, nil, err
	}
	if !session.SameWorkspace(cwd, req.Session.CWD) {
		return nil, nil, fmt.Errorf("workspace directory was replaced")
	}
	expected, err := os.Lstat(cwd)
	if err != nil {
		return nil, nil, err
	}
	if !expected.IsDir() || expected.Mode()&os.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("workspace must remain a directory")
	}
	root, err := os.OpenRoot(cwd)
	if err != nil {
		return nil, nil, err
	}
	opened, identityErr := root.Stat(".")
	if identityErr != nil || !os.SameFile(expected, opened) {
		_ = root.Close()
		return nil, nil, fmt.Errorf("workspace identity changed while opening")
	}
	if resolved, checkErr := session.NormalizeWorkspace(cwd); checkErr != nil || !session.SameWorkspace(resolved, cwd) {
		_ = root.Close()
		return nil, nil, fmt.Errorf("workspace directory changed while opening")
	}
	result, err := workspaceTools(ctx, req, root)
	if err == nil && len(configured) > 0 {
		result, err = migratedWorkspaceTools(ctx, req, root, result, configured)
	}
	if err != nil {
		_ = root.Close()
		return nil, nil, err
	}
	return result, root.Close, nil
}

func workspaceTools(ctx context.Context, req harness.RunRequest, root *os.Root) ([]tool.BaseTool, error) {
	read, err := inferTool("read_file", workspaceReadDescription, func(ctx context.Context, in readInput) (string, error) {
		if in.Offset == 0 && in.Limit == 0 {
			return readVersionedFile(ctx, root, in.Path)
		}
		return readVersionedFileRange(ctx, root, in.Path, in.Offset, in.Limit)
	})
	if err != nil {
		return nil, err
	}
	list, err := inferTool("list_directory", "List immediate entries inside a workspace directory.", func(ctx context.Context, in pathInput) ([]string, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path, err := localPath(in.Path)
		if err != nil {
			return nil, err
		}
		f, err := openRead(root, path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		entries, err := f.ReadDir(1001)
		if err != nil && err != io.EOF {
			return nil, err
		}
		out := make([]string, 0, len(entries))
		for i, e := range entries {
			if i == 1000 {
				out = append(out, "[truncated at 1000 entries]")
				break
			}
			name := e.Name()
			if e.IsDir() {
				name += "/"
			}
			out = append(out, name)
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	search, err := inferTool("search_files", "Search literal text in bounded workspace files. Returns at most 100 matches from at most 2000 entries; symlinks are not traversed.", func(ctx context.Context, in searchInput) ([]string, error) {
		if in.Text == "" {
			return nil, fmt.Errorf("text is required")
		}
		path, err := localPath(in.Path)
		if err != nil {
			return nil, err
		}
		var out []string
		count := 0
		err = fs.WalkDir(boundedRootFS{root}, filepath.ToSlash(path), func(p string, d fs.DirEntry, walkErr error) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if walkErr != nil {
				return walkErr
			}
			count++
			if count > 2000 || len(out) >= 100 {
				return fs.SkipAll
			}
			if d.IsDir() {
				if d.Name() == ".git" {
					return fs.SkipDir
				}
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() || info.Size() > maxFileBytes {
				return nil
			}
			text, err := readRootFile(ctx, root, p)
			if err != nil {
				return err
			}
			if strings.ContainsRune(text, 0) {
				return nil
			}
			// Mask before matching and truncating so search cannot probe secrets
			// byte-by-byte or split a secret across the 400-byte line cap.
			text = secrets.Mask(text)
			for n, line := range strings.Split(text, "\n") {
				if strings.Contains(line, in.Text) {
					if len(line) > 400 {
						line = line[:400]
					}
					out = append(out, fmt.Sprintf("%s:%d:%s", p, n+1, line))
					if len(out) >= 100 {
						return fs.SkipAll
					}
				}
			}
			return nil
		})
		return out, err
	})
	if err != nil {
		return nil, err
	}
	result := []tool.BaseTool{read, list, search}
	if req.Session.Mode == "plan" {
		return result, nil
	}
	write, err := inferTool("write_file", "Write UTF-8 text inside the workspace; maximum 512 KiB. Existing files require expected_version from read_file. Omitted version or absent means create-only. Conflicts require a fresh read.", func(ctx context.Context, in writeInput) (string, error) {
		return mutateVersionedFile(ctx, root, in.Path, in.ExpectedVersion, func(original string, _ bool) (string, error) {
			return restoreSecretMarkersInFile(ctx, in.Content, original)
		})
	})
	if err != nil {
		return nil, err
	}
	edit, err := inferTool("edit_file", "Replace exactly one occurrence of old_text in a workspace text file. Requires expected_version from read_file; conflicts require a fresh read.", func(ctx context.Context, in editInput) (string, error) {
		if in.OldText == "" {
			return "", harness.MarkToolNotExecuted(fmt.Errorf("old_text must not be empty"))
		}
		return mutateVersionedFile(ctx, root, in.Path, in.ExpectedVersion, func(original string, exists bool) (string, error) {
			oldText, err := restoreSecretMarkersInFile(ctx, in.OldText, original)
			if err != nil {
				return "", err
			}
			newText, err := restoreSecretMarkersInFile(ctx, in.NewText, original)
			if err != nil {
				return "", err
			}
			if !exists || strings.Count(original, oldText) != 1 {
				return "", fmt.Errorf("old_text must match exactly once in an existing file")
			}
			return strings.Replace(original, oldText, newText, 1), nil
		})
	})
	if err != nil {
		return nil, err
	}
	return append(result, write, edit), nil
}

type boundedRootFS struct{ root *os.Root }

func (f boundedRootFS) Open(name string) (fs.File, error) { return openRead(f.root, name) }
func (f boundedRootFS) ReadDir(name string) ([]fs.DirEntry, error) {
	dir, err := openRead(f.root, name)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(2001)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(entries) > 2000 {
		return nil, fmt.Errorf("directory exceeds search entry budget")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}

func localPath(path string) (string, error) {
	if path == "" {
		path = "."
	}
	path = filepath.Clean(path)
	if !filepath.IsLocal(path) {
		return "", fmt.Errorf("path must stay inside the workspace")
	}
	return path, nil
}
func readFile(ctx context.Context, cwd, path string) (string, error) {
	root, err := os.OpenRoot(cwd)
	if err != nil {
		return "", err
	}
	defer root.Close()
	return readRootFile(ctx, root, path)
}
func readRootFile(ctx context.Context, root *os.Root, path string) (string, error) {
	return readRootFileLimit(ctx, root, path, maxFileBytes)
}

func readRootFileLimit(ctx context.Context, root *os.Root, path string, limit int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	path, err := localPath(path)
	if err != nil {
		return "", err
	}
	f, err := openRead(root, path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return "", err
	}
	if len(data) > limit {
		if limit == maxFileBytes {
			return "", fmt.Errorf("file exceeds 512 KiB; read it in line ranges with offset and limit")
		}
		return "", fmt.Errorf("file exceeds %d MiB", limit>>20)
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("file is not valid UTF-8 text")
	}
	return string(data), ctx.Err()
}
func writeFile(ctx context.Context, cwd, path, content string) error {
	root, err := os.OpenRoot(cwd)
	if err != nil {
		return err
	}
	defer root.Close()
	return writeRootFile(ctx, root, path, content)
}
func writeRootFile(ctx context.Context, root *os.Root, path, content string) error {
	if err := ctx.Err(); err != nil {
		return harness.MarkToolNotExecuted(err)
	}
	if len(content) > maxFileBytes {
		return harness.MarkToolNotExecuted(fmt.Errorf("content exceeds 512 KiB"))
	}
	if !utf8.ValidString(content) {
		return harness.MarkToolNotExecuted(fmt.Errorf("content is not valid UTF-8 text"))
	}
	path, err := localPath(path)
	if err != nil {
		return harness.MarkToolNotExecuted(err)
	}
	mode := os.FileMode(0600)
	if info, statErr := root.Stat(path); statErr == nil {
		if !info.Mode().IsRegular() {
			return harness.MarkToolNotExecuted(fmt.Errorf("target is not a regular file"))
		}
		mode = info.Mode().Perm()
	} else if !os.IsNotExist(statErr) {
		return harness.MarkToolNotExecuted(statErr)
	}
	if err = root.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	// Atomic replacement avoids leaving a truncated file on interrupted writes.
	tmp := filepath.Join(filepath.Dir(path), ".deerflow-"+rand.Text())
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	_, writeErr := io.WriteString(f, content)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return root.Rename(tmp, path)
}
