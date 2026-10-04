package desktopconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"go.yaml.in/yaml/v3"
)

// Importing old state is explicit data conversion. No old module, script,
// checkpoint, generation job, approval or probation is executed or resumed.
func (s service) legacyImport(operation string, raw json.RawMessage) (any, error) {
	var request struct {
		Directory string `json:"directory"`
		ID        string `json:"id"`
		After     int    `json:"after"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if len(raw) > 16<<10 || decoder.Decode(&request) != nil {
		return nil, errors.New("invalid legacy import request")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, errors.New("invalid legacy import request")
	}
	if operation == "legacy-history-list" {
		if request.Directory != "" || request.ID != "" || request.After != 0 {
			return nil, errors.New("unexpected legacy history list parameters")
		}
		return s.legacyHistoryList()
	}
	if operation == "legacy-history-get" {
		if request.Directory != "" || !legacyArchiveID(request.ID) || request.After < 0 || request.After > 2000 {
			return nil, errors.New("invalid legacy history identity")
		}
		data, err := readLegacyFilePath(filepath.Join(s.userData, "data", "legacy-history", request.ID+".json"), 48<<20)
		if err != nil {
			return nil, err
		}
		var archive legacyArchive
		if json.Unmarshal(data, &archive) != nil || archive.Version != 1 || archive.ID != request.ID {
			return nil, errors.New("invalid legacy history archive")
		}
		if request.After > len(archive.Entries) {
			return nil, errors.New("legacy history cursor is out of range")
		}
		end := min(request.After+20, len(archive.Entries))
		page := []any{}
		for _, entry := range archive.Entries[request.After:end] {
			preview := entry.Text
			if len(entry.Content) > 0 {
				preview = string(entry.Content)
			}
			truncated := len(preview) > 2048
			if truncated {
				preview = preview[:2048]
				for !utf8.ValidString(preview) {
					preview = preview[:len(preview)-1]
				}
			}
			page = append(page, map[string]any{"path": entry.Path, "sha256": entry.SHA256, "size": entry.Size, "omitted": entry.Omitted, "preview": preview, "truncated": truncated})
		}
		return map[string]any{"id": archive.ID, "imported_at": archive.ImportedAt, "read_only": true, "entries": page, "after": request.After, "next": end, "has_more": end < len(archive.Entries), "warnings": archive.Warnings}, nil
	}
	if request.Directory == "" || request.ID != "" || request.After != 0 || !filepath.IsAbs(request.Directory) {
		return nil, errors.New("select an absolute legacy source directory")
	}
	root, err := os.OpenRoot(request.Directory)
	if err != nil {
		return nil, errors.New("cannot open selected legacy directory")
	}
	defer root.Close()
	if operation == "legacy-agent-preview" {
		return s.previewLegacyAgent(root)
	}
	return s.importLegacyHistory(root)
}

func legacyRead(root *os.Root, path string, max int64) ([]byte, error) {
	info, err := root.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > max {
		return nil, errors.New("legacy input must be a bounded regular file")
	}
	file, err := root.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("legacy input changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil || int64(len(data)) > max {
		return nil, errors.New("cannot read bounded legacy input")
	}
	return data, nil
}

func readLegacyFilePath(path string, max int64) ([]byte, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return legacyRead(root, filepath.Base(path), max)
}

func (s service) previewLegacyAgent(root *os.Root) (any, error) {
	data, err := legacyRead(root, "config.yaml", 256<<10)
	if err != nil {
		return nil, errors.New("selected Agent directory must contain config.yaml")
	}
	var old struct {
		Name        string    `yaml:"name"`
		Description string    `yaml:"description"`
		Model       string    `yaml:"model"`
		Tools       *[]string `yaml:"tool_groups"`
		MCP         *[]string `yaml:"mcp_servers"`
		Skills      []string  `yaml:"skills"`
		Memory      *bool     `yaml:"memory_enabled"`
	}
	if !utf8.Valid(data) || yaml.Unmarshal(data, &old) != nil {
		return nil, errors.New("invalid legacy Agent YAML")
	}
	name := old.Name
	if !harness.ValidAgentName(name) || name == "general-purpose" || name == "bash" {
		return nil, errors.New("legacy Agent needs a valid non-reserved role name")
	}
	soul, err := legacyRead(root, "SOUL.md", 65536)
	if errors.Is(err, os.ErrNotExist) {
		soul = nil
	} else if err != nil {
		return nil, errors.New("cannot read bounded SOUL.md")
	}
	if !utf8.Valid(soul) || bytes.ContainsRune(soul, 0) {
		return nil, errors.New("SOUL.md must be UTF-8 text")
	}
	native, err := readYAML(s.config)
	if err != nil {
		return nil, err
	}
	warnings := []string{"旧主智能体将转换为子代理草稿；保存并应用后才会生效。", "旧独立 MCP 服务选择和记忆开关不直接转换为子代理权限。"}
	role := harness.AgentConfig{Description: old.Description, SystemPrompt: string(soul), Model: old.Model, Skills: old.Skills, DelegateTo: []string{}}
	if strings.TrimSpace(role.Description) == "" {
		role.Description = "Imported role " + name
	}
	if strings.TrimSpace(role.SystemPrompt) == "" {
		role.SystemPrompt = "Complete the delegated task using the tools and skills explicitly permitted by the parent."
		warnings = append(warnings, "旧目录没有有效 SOUL.md，已使用通用委派说明，请确认后保存。")
	}
	models := map[string]bool{}
	for _, item := range list(native, "models") {
		if m, ok := item.(map[string]any); ok {
			models[str(m, "name", "")] = true
		}
	}
	if role.Model != "" && role.Model != "inherit" && !models[role.Model] {
		role.Model = "inherit"
		warnings = append(warnings, "原模型未在当前配置登记，草稿改为继承，请重新选择。")
	}
	if old.Tools != nil {
		role.Tools = []string{}
		groups := map[string]bool{}
		for _, g := range *old.Tools {
			groups[g] = false
		}
		seen := map[string]bool{}
		for _, item := range list(native, "tools") {
			t, ok := item.(map[string]any)
			if !ok {
				continue
			}
			group, tool := str(t, "group", ""), str(t, "name", "")
			if _, wanted := groups[group]; wanted && tool != "" && !seen[tool] {
				role.Tools = append(role.Tools, tool)
				seen[tool] = true
				groups[group] = true
			}
		}
		for _, found := range groups {
			if !found {
				warnings = append(warnings, "部分旧工具分组无法映射，只保留当前配置中匹配的 Go 工具；请复核工具列表。")
				break
			}
		}
	}
	if old.MCP != nil {
		// No offline importer can infer the names of live MCP tools. Preserve
		// the restriction by requiring an explicit tool selection in the draft.
		role.Tools = []string{}
		warnings = append(warnings, "旧 Agent 限定了 MCP 服务器；导入草稿的工具暂设为空，请明确选择需要的工具，避免扩大权限。")
	}
	if old.Memory != nil && !*old.Memory {
		role.DisallowedTools = append(role.DisallowedTools, "search_memory")
		warnings = append(warnings, "已禁止角色使用 search_memory；共享记忆注入仍由宿主的记忆设置控制。")
	}
	if err = (harness.SubagentsConfig{CustomAgents: map[string]harness.AgentConfig{name: role}}).Validate(models); err != nil {
		return nil, err
	}
	digest := sha256.New()
	digest.Write(data)
	digest.Write([]byte{0})
	digest.Write(soul)
	return map[string]any{"name": name, "role": role, "warnings": warnings, "source_sha256": hex.EncodeToString(digest.Sum(nil)), "draft_only": true}, nil
}

type legacyEntry struct {
	Path    string          `json:"path"`
	SHA256  string          `json:"sha256"`
	Size    int             `json:"size"`
	Content json.RawMessage `json:"content,omitempty"`
	Text    string          `json:"text,omitempty"`
	Omitted bool            `json:"omitted,omitempty"`
}
type legacyArchive struct {
	Version    int           `json:"version"`
	ID         string        `json:"id"`
	ImportedAt time.Time     `json:"imported_at"`
	ReadOnly   bool          `json:"read_only"`
	Entries    []legacyEntry `json:"entries"`
	Warnings   []string      `json:"warnings"`
}

func legacyArchiveID(id string) bool {
	raw, err := hex.DecodeString(id)
	return err == nil && len(raw) == 32 && id == strings.ToLower(id)
}

func (s service) importLegacyHistory(root *os.Root) (any, error) {
	archive := legacyArchive{Version: 1, ReadOnly: true, ImportedAt: time.Now().UTC(), Entries: []legacyEntry{}, Warnings: []string{"历史提案、发布和观察记录仅供查阅；不会恢复生成、批准、发布、观察期或回滚。", "候选或快照中的可执行/二进制资源仅记录摘要，不写出为可执行文件。"}}
	if _, err := legacyRead(root, "state.json", 4<<20); err != nil {
		return nil, errors.New("select the legacy skill evolution directory containing state.json")
	}
	total := 0
	err := fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == "." {
			return nil
		}
		top := strings.Split(path, "/")[0]
		allowed := top == "state.json" || top == "audit.jsonl" || top == "proposals" || top == "revisions" || top == "signals"
		if !allowed {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("legacy history symlinks are not supported")
		}
		if entry.IsDir() {
			return nil
		}
		if len(archive.Entries) >= 2000 || len(path) > 1024 {
			return errors.New("legacy history exceeds entry limit")
		}
		data, err := legacyRead(root, filepath.FromSlash(path), 4<<20)
		if err != nil {
			return err
		}
		total += len(data)
		if total > 32<<20 {
			return errors.New("legacy history exceeds 32 MiB")
		}
		digest := sha256.Sum256(data)
		item := legacyEntry{Path: path, SHA256: fmt.Sprintf("%x", digest), Size: len(data)}
		ext := strings.ToLower(filepath.Ext(path))
		switch {
		case ext == ".json":
			if !utf8.Valid(data) || !json.Valid(data) {
				return errors.New("invalid legacy history JSON")
			}
			item.Content = append(json.RawMessage(nil), data...)
		case utf8.Valid(data) && (ext == ".md" || ext == ".txt" || ext == ".patch" || ext == ".jsonl"):
			item.Text = string(data)
		default:
			item.Omitted = true
		}
		archive.Entries = append(archive.Entries, item)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(archive.Entries, func(i, j int) bool { return archive.Entries[i].Path < archive.Entries[j].Path })
	digest := sha256.New()
	for _, entry := range archive.Entries {
		fmt.Fprintf(digest, "%s\x00%s\x00", entry.Path, entry.SHA256)
	}
	archive.ID = hex.EncodeToString(digest.Sum(nil))
	dir := filepath.Join(s.userData, "data", "legacy-history")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, archive.ID+".json")
	encoded, err := json.MarshalIndent(archive, "", "  ")
	if err != nil || len(encoded) > 48<<20 {
		return nil, errors.New("encoded legacy history archive exceeds 48 MiB")
	}
	if _, err = os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		if err = writeAtomic(path, append(encoded, '\n')); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	return map[string]any{"id": archive.ID, "entries": len(archive.Entries), "read_only": true, "warnings": archive.Warnings}, nil
}

func (s service) legacyHistoryList() (any, error) {
	dir := filepath.Join(s.userData, "data", "legacy-history")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{"archives": []any{}}, nil
	}
	if err != nil {
		return nil, err
	}
	items := []any{}
	for _, entry := range entries {
		id := strings.TrimSuffix(entry.Name(), ".json")
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(entry.Name(), ".json") || !legacyArchiveID(id) {
			continue
		}
		items = append(items, map[string]any{"id": id, "read_only": true})
		if len(items) == 100 {
			break
		}
	}
	return map[string]any{"archives": items}, nil
}
