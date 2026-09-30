package tools

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/cloudwego/eino/components/tool"
	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type migratedPath struct {
	Description string `json:"description,omitempty"`
	Path        string `json:"path" jsonschema:"description=Relative workspace path, absolute path inside this workspace, or /mnt/acp-workspace/..."`
}
type migratedRead struct {
	migratedPath
	StartLine       int    `json:"start_line,omitempty"`
	EndLine         int    `json:"end_line,omitempty"`
	Offset          int    `json:"offset,omitempty"`
	MaxBytes        int    `json:"max_bytes,omitempty"`
	ExpectedVersion string `json:"expected_version,omitempty"`
}
type migratedWrite struct {
	migratedPath
	Content string `json:"content"`
	Append  bool   `json:"append,omitempty"`
}
type migratedReplace struct {
	migratedPath
	OldStr     string `json:"old_str"`
	NewStr     string `json:"new_str"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}
type migratedGlob struct {
	migratedPath
	Pattern     string `json:"pattern"`
	IncludeDirs bool   `json:"include_dirs,omitempty"`
	MaxResults  int    `json:"max_results,omitempty"`
}
type migratedGrep struct {
	migratedPath
	Pattern       string `json:"pattern"`
	Glob          string `json:"glob,omitempty"`
	Literal       bool   `json:"literal,omitempty"`
	CaseSensitive bool   `json:"case_sensitive,omitempty"`
	MaxResults    int    `json:"max_results,omitempty"`
}
type migratedLS struct {
	migratedPath
	MaxEntries int    `json:"max_entries,omitempty"`
	Cursor     string `json:"cursor,omitempty"`
}
type migratedDelete struct {
	migratedPath
	Recursive bool `json:"recursive,omitempty"`
}
type migratedMove struct {
	Description string `json:"description,omitempty"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Overwrite   bool   `json:"overwrite,omitempty"`
}

// Mutations share a run-scoped lock. All operations use the pinned os.Root,
// including recursive deletion; no absolute path is passed to a shell.
func migratedWorkspaceTools(ctx context.Context, req harness.RunRequest, root *os.Root, standard []tool.BaseTool, configs []harness.BuiltinToolConfig) ([]tool.BaseTool, error) {
	var mu sync.Mutex
	path := func(value string) (string, error) {
		p, err := migratedLocalPath(req.Session.CWD, value)
		return p, harness.MarkToolNotExecuted(err)
	}
	add := func(t tool.BaseTool, err error) error {
		if err != nil {
			return err
		}
		info, err := t.Info(ctx)
		if err != nil {
			return err
		}
		for i, existing := range standard {
			old, err := existing.Info(ctx)
			if err != nil {
				return err
			}
			if old.Name == info.Name {
				standard[i] = t
				return nil
			}
		}
		standard = append(standard, t)
		return nil
	}
	for _, c := range configs {
		var err error
		switch c.Name {
		case "ls":
			err = add(inferTool("ls", "List workspace entries up to two levels deep, with bounded pages and a version-checked cursor.", func(ctx context.Context, in migratedLS) (string, error) {
				p, err := path(in.Path)
				if err != nil {
					return "", err
				}
				limit := in.MaxEntries
				if limit == 0 {
					limit = 200
				}
				if limit < 1 || limit > 1000 {
					return "", errors.New("max_entries must be 1..1000")
				}
				entries := []string{}
				truncated := false
				err = walkWorkspace(ctx, root, p, func(name string, d fs.DirEntry) error {
					if name == filepath.ToSlash(p) {
						return nil
					}
					rel, _ := filepath.Rel(p, filepath.FromSlash(name))
					depth := strings.Count(filepath.ToSlash(rel), "/")
					if depth >= 2 {
						if d.IsDir() {
							return fs.SkipDir
						}
						return nil
					}
					entry := name
					if d.IsDir() {
						entry += "/"
					}
					entries = append(entries, entry)
					return nil
				}, &truncated)
				if err != nil {
					return "", err
				}
				body, _ := json.Marshal(entries)
				version := fmt.Sprintf("%x", sha256.Sum256(body))
				offset := 0
				if in.Cursor != "" {
					decoded, e := base64.RawURLEncoding.DecodeString(in.Cursor)
					if e != nil {
						return "", errors.New("invalid ls cursor")
					}
					prev, index, ok := strings.Cut(string(decoded), ":")
					offset, e = strconv.Atoi(index)
					if !ok || e != nil || prev != version || offset < 0 || offset >= len(entries) {
						return "", errors.New("directory changed or cursor invalid; restart ls")
					}
				}
				end := min(offset+limit, len(entries))
				next := ""
				if end < len(entries) {
					next = base64.RawURLEncoding.EncodeToString([]byte(version + ":" + strconv.Itoa(end)))
				}
				return marshalString(map[string]any{"entries": entries[offset:end], "version": version, "next_cursor": next, "truncated": truncated || next != ""})
			}))
		case "read_file":
			err = add(inferTool("read_file", "Read a UTF-8 workspace file with line ranges or version-checked byte continuation. Returns content, version and next_offset. At most 1 MiB per page; files up to 16 MiB.", func(ctx context.Context, in migratedRead) (string, error) {
				p, err := path(in.Path)
				if err != nil {
					return "", err
				}
				return migratedReadFile(ctx, root, p, in)
			}))
		case "write_file":
			if req.Session.Mode == "plan" {
				continue
			}
			err = add(inferTool("write_file", "Atomically write or append UTF-8 text to a workspace file, at most 512 KiB total.", func(ctx context.Context, in migratedWrite) (string, error) {
				mu.Lock()
				defer mu.Unlock()
				p, err := path(in.Path)
				if err != nil {
					return "", err
				}
				content := in.Content
				if in.Append {
					old, e := readRootFile(ctx, root, p)
					if e != nil && !errors.Is(e, os.ErrNotExist) {
						return "", harness.MarkToolNotExecuted(e)
					}
					content = old + content
				}
				return "OK", writeRootFile(ctx, root, p, content)
			}))
		case "str_replace":
			if req.Session.Mode == "plan" {
				continue
			}
			err = add(inferTool("str_replace", "Replace old_str with new_str in a workspace text file. Without replace_all, old_str must appear exactly once.", func(ctx context.Context, in migratedReplace) (string, error) {
				mu.Lock()
				defer mu.Unlock()
				p, err := path(in.Path)
				if err != nil {
					return "", err
				}
				if in.OldStr == "" {
					return "", harness.MarkToolNotExecuted(errors.New("old_str must not be empty"))
				}
				content, err := readRootFile(ctx, root, p)
				if err != nil {
					return "", harness.MarkToolNotExecuted(err)
				}
				count := strings.Count(content, in.OldStr)
				if count == 0 || (!in.ReplaceAll && count != 1) {
					return "", harness.MarkToolNotExecuted(errors.New("old_str must match; without replace_all it must match exactly once"))
				}
				n := 1
				if in.ReplaceAll {
					n = -1
				}
				return "OK", writeRootFile(ctx, root, p, strings.Replace(content, in.OldStr, in.NewStr, n))
			}))
		case "glob":
			err = add(inferTool("glob", "Find workspace paths by glob, including **. Bounded traversal; returns matches and truncation/coverage flags.", func(ctx context.Context, in migratedGlob) (string, error) {
				p, err := path(in.Path)
				if err != nil {
					return "", err
				}
				if !doublestar.ValidatePattern(in.Pattern) || in.Pattern == "" {
					return "", errors.New("invalid glob pattern")
				}
				limit := resultLimit(c.MaxResults, in.MaxResults, 200, 1000)
				matches := []string{}
				truncated := false
				err = walkWorkspace(ctx, root, p, func(name string, d fs.DirEntry) error {
					if d.Type()&os.ModeSymlink != 0 || d.IsDir() && !in.IncludeDirs {
						return nil
					}
					rel, e := filepath.Rel(p, filepath.FromSlash(name))
					if e != nil {
						return e
					}
					ok, e := doublestar.Match(in.Pattern, filepath.ToSlash(rel))
					if e != nil {
						return e
					}
					if ok {
						if len(matches) >= limit {
							truncated = true
							return fs.SkipAll
						}
						matches = append(matches, name)
					}
					return nil
				}, &truncated)
				if err != nil {
					return "", err
				}
				return marshalString(map[string]any{"matches": matches, "truncated": truncated})
			}))
		case "grep":
			err = add(inferTool("grep", "Search text with RE2 regex or literal matching; optional glob filter and case sensitivity. Skips symlinks, binary files and files over 512 KiB; bounded traversal.", func(ctx context.Context, in migratedGrep) (string, error) {
				p, err := path(in.Path)
				if err != nil {
					return "", err
				}
				if in.Pattern == "" || len(in.Pattern) > 4096 {
					return "", errors.New("pattern must contain 1..4096 bytes")
				}
				pattern := in.Pattern
				if in.Literal {
					pattern = regexp.QuoteMeta(pattern)
				}
				if !in.CaseSensitive {
					pattern = "(?i)" + pattern
				}
				re, err := regexp.Compile(pattern)
				if err != nil {
					return "", err
				}
				if in.Glob != "" && !doublestar.ValidatePattern(in.Glob) {
					return "", errors.New("invalid glob filter")
				}
				limit := resultLimit(c.MaxResults, in.MaxResults, 100, 500)
				matches := []any{}
				truncated := false
				skipped := 0
				total := 0
				err = walkWorkspace(ctx, root, p, func(name string, d fs.DirEntry) error {
					if d.IsDir() {
						return nil
					}
					if d.Type()&os.ModeSymlink != 0 {
						skipped++
						return nil
					}
					if in.Glob != "" {
						rel, _ := filepath.Rel(p, filepath.FromSlash(name))
						if rel == "." {
							rel = d.Name()
						}
						ok, _ := doublestar.Match(in.Glob, filepath.ToSlash(rel))
						if !ok {
							return nil
						}
					}
					info, e := d.Info()
					if e != nil {
						return e
					}
					if !info.Mode().IsRegular() || info.Size() > maxFileBytes {
						skipped++
						return nil
					}
					total += int(info.Size())
					if total > 16<<20 {
						truncated = true
						return fs.SkipAll
					}
					text, e := readRootFile(ctx, root, name)
					if e != nil {
						if ctx.Err() != nil {
							return ctx.Err()
						}
						skipped++
						return nil
					}
					if strings.ContainsRune(text, 0) {
						skipped++
						return nil
					}
					for n, line := range strings.Split(text, "\n") {
						if re.MatchString(line) {
							if len(matches) >= limit {
								truncated = true
								return fs.SkipAll
							}
							matches = append(matches, map[string]any{"path": name, "line_number": n + 1, "line": truncateRunes(line, 400)})
						}
					}
					return nil
				}, &truncated)
				if err != nil {
					return "", err
				}
				return marshalString(map[string]any{"matches": matches, "truncated": truncated, "skipped_files": skipped})
			}))
		case "delete_path":
			if req.Session.Mode == "plan" {
				continue
			}
			err = add(inferTool("delete_path", "Delete a workspace file/symlink or empty directory. recursive explicitly allows a directory tree. Workspace root cannot be deleted.", func(ctx context.Context, in migratedDelete) (string, error) {
				mu.Lock()
				defer mu.Unlock()
				p, err := path(in.Path)
				if err != nil {
					return "", err
				}
				if p == "." {
					return "", harness.MarkToolNotExecuted(errors.New("cannot delete workspace root"))
				}
				if err := ctx.Err(); err != nil {
					return "", harness.MarkToolNotExecuted(err)
				}
				if _, err = root.Lstat(p); err != nil {
					return "", harness.MarkToolNotExecuted(err)
				}
				if in.Recursive {
					err = root.RemoveAll(p)
				} else {
					err = root.Remove(p)
				}
				return "OK", err
			}))
		case "move_path":
			if req.Session.Mode == "plan" {
				continue
			}
			err = add(inferTool("move_path", "Move/rename within the workspace. Creates destination parents. Existing file/symlink requires overwrite=true; existing directories cannot be overwritten.", func(ctx context.Context, in migratedMove) (string, error) {
				mu.Lock()
				defer mu.Unlock()
				src, err := path(in.Source)
				if err != nil {
					return "", err
				}
				dst, err := path(in.Destination)
				if err != nil {
					return "", err
				}
				if src == "." || dst == "." || src == dst {
					return "", harness.MarkToolNotExecuted(errors.New("cannot move workspace root or move path onto itself"))
				}
				if err := ctx.Err(); err != nil {
					return "", harness.MarkToolNotExecuted(err)
				}
				if _, err = root.Lstat(src); err != nil {
					return "", harness.MarkToolNotExecuted(err)
				}
				info, err := root.Lstat(dst)
				if err == nil {
					if !in.Overwrite || info.IsDir() {
						return "", harness.MarkToolNotExecuted(errors.New("destination exists or is a directory"))
					}
				} else if !errors.Is(err, os.ErrNotExist) {
					return "", harness.MarkToolNotExecuted(err)
				}
				if err = root.MkdirAll(filepath.Dir(dst), 0755); err != nil {
					return "", err
				}
				return "OK", root.Rename(src, dst)
			}))
		}
		if err != nil {
			return nil, err
		}
	}
	return standard, nil
}

func migratedLocalPath(workspace, value string) (string, error) {
	const virtual = "/mnt/acp-workspace"
	if value == virtual {
		value = "."
	} else if strings.HasPrefix(value, virtual+"/") {
		value = strings.TrimPrefix(value, virtual+"/")
	}
	if filepath.IsAbs(value) {
		var err error
		value, err = filepath.Rel(workspace, value)
		if err != nil {
			return "", errors.New("path must stay inside the workspace")
		}
	}
	return localPath(value)
}

func walkWorkspace(ctx context.Context, root *os.Root, path string, visit func(string, fs.DirEntry) error, truncated *bool) error {
	count := 0
	return fs.WalkDir(boundedRootFS{root}, filepath.ToSlash(path), func(name string, d fs.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err != nil {
			return err
		}
		count++
		if count > 2000 {
			*truncated = true
			return fs.SkipAll
		}
		if d.IsDir() && d.Name() == ".git" && name != filepath.ToSlash(path) {
			return fs.SkipDir
		}
		return visit(name, d)
	})
}

func resultLimit(configured, requested, fallback, cap int) int {
	if configured > 0 {
		return configured
	}
	if requested > 0 {
		return min(requested, cap)
	}
	return fallback
}
func marshalString(value any) (string, error) {
	data, err := json.Marshal(value)
	return string(data), err
}
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

func migratedReadFile(ctx context.Context, root *os.Root, path string, in migratedRead) (string, error) {
	if in.Offset < 0 || in.StartLine < 0 || in.EndLine < 0 || in.EndLine > 0 && in.StartLine > in.EndLine {
		return "", errors.New("invalid read range")
	}
	if in.Offset > 0 && (in.ExpectedVersion == "" || in.StartLine > 0) {
		return "", errors.New("continuation requires expected_version and omits start_line")
	}
	limit := in.MaxBytes
	if limit == 0 {
		limit = maxFileBytes
	}
	if limit < 4 || limit > 1<<20 {
		return "", errors.New("max_bytes must be 4..1048576")
	}
	if err := ctx.Err(); err != nil {
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
	if !info.Mode().IsRegular() || info.Size() > 16<<20 {
		return "", errors.New("read_file requires a regular file at most 16 MiB")
	}
	body, err := io.ReadAll(io.LimitReader(f, 16<<20+1))
	if err != nil {
		return "", err
	}
	if len(body) > 16<<20 || !utf8.Valid(body) || strings.ContainsRune(string(body), 0) {
		return "", errors.New("file exceeds limit or is not UTF-8 text")
	}
	version := fmt.Sprintf("%x", sha256.Sum256(body))
	if in.ExpectedVersion != "" && in.ExpectedVersion != version {
		return "", errors.New("file changed; restart read_file")
	}
	start := in.Offset
	end := len(body)
	if in.StartLine > 0 || in.EndLine > 0 {
		line := 1
		for i, b := range body {
			if line < in.StartLine {
				start = i + 1
			}
			if b == '\n' {
				if in.EndLine > 0 && line == in.EndLine {
					end = i + 1
					break
				}
				line++
			}
		}
	}
	if start > end {
		return "", errors.New("offset exceeds file/range length")
	}
	rangeEnd := end
	end = min(end, start+limit)
	if start > 0 && start < len(body) && body[start]&0xc0 == 0x80 {
		return "", errors.New("offset must be a UTF-8 character boundary")
	}
	for end > start && !utf8.Valid(body[start:end]) {
		end--
	}
	if start > 0 && start < len(body) && body[start]&0xc0 == 0x80 {
		return "", errors.New("offset must be a UTF-8 character boundary")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	next := 0
	if end < rangeEnd {
		next = end
	}
	return marshalString(map[string]any{"content": string(body[start:end]), "version": version, "offset": start, "next_offset": next, "truncated": next > 0})
}
