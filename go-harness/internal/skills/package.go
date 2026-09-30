package skills

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"gopkg.in/yaml.v3"
)

type metadata struct {
	Name          string                 `json:"name"`
	Description   string                 `json:"description"`
	License       string                 `json:"license,omitempty"`
	Compatibility string                 `json:"compatibility,omitempty"`
	Metadata      map[string]string      `json:"metadata,omitempty"`
	Findings      []harness.SkillFinding `json:"findings,omitempty"`
}
type skillPackage struct {
	meta     metadata
	files    map[string][]byte
	manifest []harness.SkillFileInfo
	bytes    int64
}

func validName(s string) bool {
	if len(s) < 1 || len(s) > 64 || s[0] == '-' || s[len(s)-1] == '-' || strings.Contains(s, "--") {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func validRelative(s string) bool {
	if s == "" || len(s) > 1024 || !utf8.ValidString(s) || strings.ContainsAny(s, "\\:\x00") || strings.HasPrefix(s, "/") || path.Clean(s) != s {
		return false
	}
	for _, part := range strings.Split(s, "/") {
		if part == "" || part == "." || part == ".." || len(part) > 255 || strings.TrimRight(part, ". ") != part || strings.IndexFunc(part, unicode.IsControl) >= 0 {
			return false
		}
		stem := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
		if stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" || len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '1' && stem[3] <= '9' {
			return false
		}
	}
	return true
}

func (r *Registry) readPackage(ctx context.Context, s *source, dir string) (skillPackage, error) {
	p := skillPackage{files: make(map[string][]byte)}
	if dir != "." && !validRelative(dir) {
		return p, invalid("installation directory must be a safe relative path")
	}
	if dir != "." {
		var prefix string
		for _, part := range strings.Split(dir, "/") {
			prefix = path.Join(prefix, part)
			i, err := s.root.Lstat(prefix)
			if err != nil || !i.IsDir() || i.Mode()&os.ModeSymlink != 0 {
				return p, denied("installation directory contains a link or invalid component")
			}
		}
	}
	expected, err := s.root.Lstat(dir)
	if err != nil || !expected.IsDir() {
		return p, invalid("installation directory does not exist")
	}
	root, err := s.root.OpenRoot(dir)
	if err != nil {
		return p, denied("cannot open installation directory")
	}
	defer root.Close()
	actual, err := root.Stat(".")
	if err != nil || !os.SameFile(expected, actual) {
		return p, denied("installation directory changed during open")
	}
	entries := 0
	folded := make(map[string]bool)
	err = walkRoot(root, func(name string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return denied("cannot enumerate installation files")
		}
		if name == "." {
			return nil
		}
		entries++
		if entries > r.limits.MaxFilesPerSkill*4 || strings.Count(name, "/") > 15 {
			return invalid("directory entry or depth limit exceeded")
		}
		if !validRelative(name) {
			return invalid("installation contains an invalid path")
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return denied("skill links are not allowed")
		}
		key := strings.ToLower(name)
		if folded[key] {
			return invalid("case-colliding file paths")
		}
		folded[key] = true
		if entry.IsDir() {
			if strings.EqualFold(entry.Name(), ".git") || strings.EqualFold(entry.Name(), ".svn") {
				return invalid("version-control metadata is not a skill file")
			}
			return nil
		}
		if len(p.files) >= r.limits.MaxFilesPerSkill {
			return invalid("file count limit exceeded")
		}
		before, err := root.Lstat(name)
		if err != nil || !before.Mode().IsRegular() {
			return denied("skill files must be regular files")
		}
		if before.Size() > r.limits.MaxFileBytes {
			return invalid("individual file byte limit exceeded")
		}
		f, err := root.Open(name)
		if err != nil {
			return denied("cannot read skill file")
		}
		opened, err := f.Stat()
		if err != nil || !os.SameFile(before, opened) {
			_ = f.Close()
			return denied("skill file changed during open")
		}
		data, readErr := io.ReadAll(io.LimitReader(f, r.limits.MaxFileBytes+1))
		after, statErr := f.Stat()
		_ = f.Close()
		if readErr != nil || statErr != nil {
			return denied("cannot read skill file")
		}
		if int64(len(data)) > r.limits.MaxFileBytes {
			return invalid("individual file byte limit exceeded")
		}
		if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || int64(len(data)) != after.Size() {
			return invalid("skill file changed while reading")
		}
		if int64(len(data)) > r.limits.MaxSkillBytes-p.bytes {
			return invalid("skill total byte limit exceeded")
		}
		p.bytes += int64(len(data))
		p.files[name] = data
		p.manifest = append(p.manifest, harness.SkillFileInfo{Path: name, Hash: digest(data), Bytes: int64(len(data))})
		return nil
	})
	if err != nil {
		return p, err
	}
	main, ok := p.files["SKILL.md"]
	if !ok {
		return p, invalid("SKILL.md is required")
	}
	p.meta, _, err = parse(main)
	if err != nil {
		return p, err
	}
	if dir != "." && path.Base(dir) != p.meta.Name {
		return p, invalid("skill name must match its installation directory")
	}
	sort.Slice(p.manifest, func(i, j int) bool { return p.manifest[i].Path < p.manifest[j].Path })
	p.meta.Findings = scan(p.files, p.manifest)
	return p, nil
}

func parse(data []byte) (metadata, string, error) {
	var result metadata
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return result, "", invalid("SKILL.md must be UTF-8 text without NUL")
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return result, "", invalid("SKILL.md must begin with a YAML frontmatter delimiter")
	}
	end := strings.Index(text[4:], "\n---\n")
	var raw, body string
	if end >= 0 {
		raw = text[4 : 4+end]
		body = text[4+end+5:]
	} else if strings.HasSuffix(text, "\n---") {
		raw = text[4 : len(text)-4]
	} else {
		return result, "", invalid("frontmatter closing delimiter is missing")
	}
	if len(raw) > 16<<10 {
		return result, "", invalid("frontmatter exceeds 16 KiB")
	}
	var doc yaml.Node
	decoder := yaml.NewDecoder(strings.NewReader(raw))
	if err := decoder.Decode(&doc); err != nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return result, "", invalid("frontmatter must be a YAML mapping")
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return result, "", invalid("frontmatter must contain one YAML document")
	}
	node := doc.Content[0]
	seen := make(map[string]bool)
	if len(node.Content) > 40 {
		return result, "", invalid("too many frontmatter fields")
	}
	for i := 0; i < len(node.Content); i += 2 {
		key, val := node.Content[i], node.Content[i+1]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || seen[key.Value] {
			return result, "", invalid("frontmatter keys must be unique strings")
		}
		seen[key.Value] = true
		switch key.Value {
		case "model", "agent", "context", "allowed-tools", "tools", "permissions":
			return result, "", invalid("model, fork, agent and permission overrides are unsupported in this profile")
		case "metadata":
			if val.Kind != yaml.MappingNode || len(val.Content) > 64 {
				return result, "", invalid("metadata must be a bounded string mapping")
			}
			result.Metadata = map[string]string{}
			for j := 0; j < len(val.Content); j += 2 {
				k, v := val.Content[j], val.Content[j+1]
				if !yamlString(k, 64) || !yamlString(v, 512) {
					return result, "", invalid("metadata entries must be bounded strings")
				}
				if _, ok := result.Metadata[k.Value]; ok {
					return result, "", invalid("metadata keys must be unique")
				}
				result.Metadata[k.Value] = v.Value
			}
		case "compatibility":
			if val.Kind == yaml.MappingNode {
				if val.Anchor != "" || len(val.Content) == 0 || len(val.Content) > 16 {
					return result, "", invalid("compatibility mapping must contain 1..8 plain string entries")
				}
				parts := make([]string, 0, len(val.Content)/2)
				keys := make(map[string]bool)
				for j := 0; j < len(val.Content); j += 2 {
					k, v := val.Content[j], val.Content[j+1]
					if !yamlString(k, 64) || !yamlString(v, 256) || keys[k.Value] {
						return result, "", invalid("compatibility entries must be unique plain strings")
					}
					keys[k.Value] = true
					parts = append(parts, k.Value+": "+v.Value)
				}
				result.Compatibility = strings.Join(parts, ", ")
				if len(result.Compatibility) > 1024 {
					return result, "", invalid("compatibility exceeds 1024 bytes")
				}
				continue
			}
			if !yamlString(val, 1024) {
				return result, "", invalid("frontmatter values must be bounded plain strings")
			}
			result.Compatibility = val.Value
		case "name", "description", "license":
			if !yamlString(val, 1024) {
				return result, "", invalid("frontmatter values must be bounded plain strings")
			}
			switch key.Value {
			case "name":
				result.Name = val.Value
			case "description":
				result.Description = strings.TrimSpace(val.Value)
			case "license":
				result.License = val.Value
			}
		default:
			return result, "", invalid("unknown frontmatter field")
		}
	}
	if !validName(result.Name) || result.Description == "" {
		return result, "", invalid("name and description are required and must be valid")
	}
	if strings.TrimSpace(body) == "" {
		return result, "", invalid("skill instructions cannot be empty")
	}
	return result, strings.TrimSpace(body), nil
}

func yamlString(n *yaml.Node, max int) bool {
	return n.Kind == yaml.ScalarNode && n.Tag == "!!str" && n.Anchor == "" && len(n.Value) <= max && strings.IndexFunc(n.Value, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) < 0
}

func decodeVersion(meta, manifest, version string, size int64, limits harness.SkillLimits) (metadata, []harness.SkillFileInfo, error) {
	var fm metadata
	var files []harness.SkillFileInfo
	if json.Unmarshal([]byte(meta), &fm) != nil || json.Unmarshal([]byte(manifest), &files) != nil || !validName(fm.Name) || fm.Description == "" || digest([]byte(manifest)) != version {
		return fm, nil, errors.New("skills version metadata is corrupt")
	}
	if len(files) == 0 || len(files) > limits.MaxFilesPerSkill || size < 0 || size > limits.MaxSkillBytes {
		return fm, nil, invalid("pinned version exceeds current limits")
	}
	var total int64
	previous := ""
	hasMain := false
	for _, f := range files {
		if !validRelative(f.Path) || f.Path <= previous || !validHash(f.Hash) || f.Bytes < 0 || f.Bytes > limits.MaxFileBytes || f.Bytes > limits.MaxSkillBytes-total {
			return fm, nil, errors.New("skills file manifest is invalid")
		}
		total += f.Bytes
		previous = f.Path
		hasMain = hasMain || f.Path == "SKILL.md"
	}
	if !hasMain || total != size {
		return fm, nil, errors.New("skills file manifest is inconsistent")
	}
	return fm, files, nil
}

// ReadDir batches bound enumeration memory before the visitor enforces the
// entry/depth limits. fs.WalkDir would eagerly read an entire large directory.
func walkRoot(root *os.Root, visit func(string, os.DirEntry, error) error) error {
	if err := visit(".", nil, nil); err != nil {
		return err
	}
	var walk func(string) error
	walk = func(dir string) error {
		expected, err := root.Lstat(dir)
		if err != nil || !expected.IsDir() || expected.Mode()&os.ModeSymlink != 0 {
			return denied("directory changed or became a link")
		}
		f, err := root.Open(dir)
		if err != nil {
			return visit(dir, nil, err)
		}
		defer f.Close()
		actual, err := f.Stat()
		if err != nil || !os.SameFile(expected, actual) {
			return denied("directory changed during enumeration")
		}
		for {
			entries, readErr := f.ReadDir(64)
			for _, entry := range entries {
				name := path.Join(dir, entry.Name())
				if err := visit(name, entry, nil); err != nil {
					return err
				}
				if entry.IsDir() {
					if err := walk(name); err != nil {
						return err
					}
				}
			}
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			if readErr != nil {
				return visit(dir, nil, readErr)
			}
		}
	}
	return walk(".")
}
func validHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
