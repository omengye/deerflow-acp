package desktopconfig

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/gofrs/flock"
	"go.yaml.in/yaml/v3"
)

const redacted = "__DEERFLOW_REDACTED__"

type Options struct{ Config, UserData, Resources string }

type service struct {
	config, userData, resources string
}

func Run(options Options, operation string, input json.RawMessage) (any, error) {
	for _, name := range []string{options.Config, options.UserData, options.Resources} {
		if name == "" {
			return nil, errors.New("config, user-data and resources are required")
		}
	}
	config, err := filepath.Abs(options.Config)
	if err != nil {
		return nil, err
	}
	userData, err := filepath.Abs(options.UserData)
	if err != nil {
		return nil, err
	}
	resources, err := filepath.Abs(options.Resources)
	if err != nil {
		return nil, err
	}
	s := service{config: config, userData: userData, resources: resources}
	if err := s.init(); err != nil {
		return nil, err
	}
	switch operation {
	case "init":
		return map[string]any{"initialized": true, "paths": map[string]any{"config": config, "user_data": userData}}, nil
	case "snapshot":
		return s.snapshot()
	case "bridge-policy":
		data, err := readYAML(config)
		if err != nil {
			return nil, err
		}
		return bridgePolicy(data)
	case "validate", "save":
		var document map[string]any
		if err := decodeRequest(input, &document); err != nil {
			return nil, err
		}
		return s.save(document, operation == "validate")
	case "test-model":
		var request map[string]any
		if err := decodeRequest(input, &request); err != nil {
			return nil, err
		}
		return s.testModel(request)
	default:
		return nil, fmt.Errorf("unsupported configuration operation %q", operation)
	}
}

func decodeRequest(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		return errors.New("request JSON is required")
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return errors.New("invalid request JSON")
	}
	return nil
}

func (s service) init() error {
	for _, part := range []string{"config", "data", "skills", "logs", "backups", filepath.Join("runtime", "acp-go")} {
		if err := os.MkdirAll(filepath.Join(s.userData, part), 0700); err != nil {
			return err
		}
	}
	if _, err := os.Stat(s.config); errors.Is(err, os.ErrNotExist) {
		data, err := readYAML(filepath.Join(s.resources, "default-config.yaml"))
		if err != nil {
			return fmt.Errorf("read default config: %w", err)
		}
		api := object(data, "api")
		api["data_dir"] = "../data"
		api["deerflow_home"] = "../data/deerflow"
		api["extensions_config_path"] = "./extensions_config.json"
		skills := object(data, "skills")
		skills["path"] = "../skills"
		skills["extensions_file"] = "./extensions_config.json"
		if err := writeYAMLAtomic(s.config, data); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	data, err := readYAML(s.config)
	if err != nil {
		return err
	}
	paths := s.paths(data)
	if _, err := os.Stat(paths.extensions); errors.Is(err, os.ErrNotExist) {
		if err := writeJSONAtomic(paths.extensions, map[string]any{"skills": map[string]any{}}); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if err := os.MkdirAll(paths.skills, 0700); err != nil {
		return err
	}
	if err := copyInitialSkills(filepath.Join(s.resources, "skills"), paths.skills); err != nil {
		return err
	}
	return os.MkdirAll(paths.agents, 0700)
}

type configPaths struct{ extensions, skills, agents, home string }

func (s service) paths(data map[string]any) configPaths {
	api := readObject(data, "api")
	skills := readObject(data, "skills")
	configDir := filepath.Dir(s.config)
	resolve := func(value, fallback string) string {
		if value == "" {
			value = fallback
		}
		if filepath.IsAbs(value) {
			return filepath.Clean(value)
		}
		return filepath.Clean(filepath.Join(configDir, value))
	}
	extensions := str(api, "extensions_config_path", "")
	if extensions == "" {
		extensions = str(skills, "extensions_file", "./extensions_config.json")
	}
	home := resolve(str(api, "deerflow_home", ""), "../data/deerflow")
	return configPaths{extensions: resolve(extensions, "./extensions_config.json"), skills: resolve(str(skills, "path", ""), "../skills"), agents: filepath.Join(home, "agents"), home: home}
}

func copyInitialSkills(source, destination string) error {
	entries, err := os.ReadDir(destination)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return nil
	}
	if _, err := os.Stat(source); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	for _, category := range []string{"public", "custom"} {
		root := filepath.Join(source, category)
		if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("bundled Skill contains a symlink")
			}
			rel, err := filepath.Rel(source, path)
			if err != nil {
				return err
			}
			target := filepath.Join(destination, rel)
			if entry.IsDir() {
				return os.MkdirAll(target, 0700)
			}
			if !entry.Type().IsRegular() {
				return fmt.Errorf("bundled Skill contains an unsupported file")
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, body, 0600)
		}); err != nil {
			return err
		}
	}
	return nil
}

func readYAML(path string) (map[string]any, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, 2<<20+1))
	if err != nil {
		return nil, err
	}
	if len(body) > 2<<20 {
		return nil, errors.New("configuration exceeds 2 MiB")
	}
	var data map[string]any
	if err := yaml.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("invalid configuration YAML: %w", err)
	}
	if data == nil {
		return nil, errors.New("configuration must be a YAML object")
	}
	return data, nil
}

func readJSON(path string) (map[string]any, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(body) > 1<<20 {
		return nil, errors.New("extensions configuration exceeds 1 MiB")
	}
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil || data == nil {
		return nil, errors.New("invalid extensions JSON")
	}
	return data, nil
}

func writeYAMLAtomic(path string, data map[string]any) error {
	body, err := yaml.Marshal(data)
	if err != nil {
		return err
	}
	return writeAtomic(path, body)
}
func writeJSONAtomic(path string, data map[string]any) error {
	body, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(body, '\n'))
}
func writeAtomic(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".deerflow-config-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(body); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func hashFile(path string) (string, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(body)), nil
}

func readObject(parent map[string]any, key string) map[string]any {
	value, _ := parent[key].(map[string]any)
	if value == nil {
		return map[string]any{}
	}
	return value
}
func object(parent map[string]any, key string) map[string]any {
	value, ok := parent[key].(map[string]any)
	if !ok {
		value = map[string]any{}
		parent[key] = value
	}
	return value
}
func str(parent map[string]any, key, fallback string) string {
	if value, ok := parent[key].(string); ok {
		return value
	}
	return fallback
}
func boolValue(parent map[string]any, key string, fallback bool) bool {
	if value, ok := parent[key].(bool); ok {
		return value
	}
	return fallback
}
func number(parent map[string]any, key string, fallback float64) float64 {
	switch value := parent[key].(type) {
	case int:
		return float64(value)
	case float64:
		return value
	}
	return fallback
}
func list(parent map[string]any, key string) []any {
	value, _ := parent[key].([]any)
	if value == nil {
		return []any{}
	}
	return value
}
func cloneMap(value map[string]any) map[string]any {
	out := make(map[string]any, len(value))
	for key, item := range value {
		out[key] = item
	}
	return out
}

var envReference = regexp.MustCompile(`^\$[A-Za-z_][A-Za-z0-9_]*$|^\$\{[^}]+\}$`)

func sensitive(key string) bool {
	key = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(key), "-", "_"))
	switch key {
	case "api_key", "access_key", "secret_key", "private_key", "app_secret", "client_secret", "password", "token", "access_token", "refresh_token", "verification_token", "credential", "credentials":
		return true
	}
	for _, suffix := range []string{"_api_key", "_access_key", "_secret", "_password", "_token"} {
		if strings.HasSuffix(key, suffix) {
			return true
		}
	}
	return false
}
func redact(value any, key string) any {
	if sensitive(key) && value != nil {
		if text, ok := value.(string); !ok || (text != "" && !envReference.MatchString(strings.TrimSpace(text))) {
			return redacted
		}
	}
	switch node := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(node))
		for name, item := range node {
			out[name] = redact(item, name)
		}
		return out
	case []any:
		out := make([]any, len(node))
		for i, item := range node {
			out[i] = redact(item, key)
		}
		return out
	default:
		return value
	}
}
func restore(value, previous any) any {
	if text, ok := value.(string); ok && text == redacted {
		if previous == nil {
			return redacted
		}
		return previous
	}
	switch node := value.(type) {
	case map[string]any:
		old, _ := previous.(map[string]any)
		out := make(map[string]any, len(node))
		for key, item := range node {
			out[key] = restore(item, old[key])
		}
		return out
	case []any:
		old, _ := previous.([]any)
		out := make([]any, len(node))
		byIdentity := make(map[string]any, len(old))
		duplicates := make(map[string]bool)
		for _, item := range old {
			identity := stableArrayIdentity(item)
			if identity == "" {
				continue
			}
			if _, exists := byIdentity[identity]; exists {
				duplicates[identity] = true
			}
			byIdentity[identity] = item
		}
		used := make(map[string]bool, len(node))
		for i, item := range node {
			var prior any
			identity := stableArrayIdentity(item)
			if identity != "" && !duplicates[identity] && !used[identity] {
				prior = byIdentity[identity]
				used[identity] = true
			}
			out[i] = restore(item, prior)
		}
		return out
	default:
		return value
	}
}

// Anonymous arrays cannot safely inherit hidden values by position: reordering
// one entry could attach a credential to another endpoint. Named entries are
// matched to the same name and execution destination before restoration.
func stableArrayIdentity(value any) string {
	item, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	var identity string
	for _, key := range []string{"name", "id", "key"} {
		if name := str(item, key, ""); name != "" {
			identity = key + "\x00" + name
			break
		}
	}
	if identity == "" {
		return ""
	}
	for _, key := range []string{"use", "command", "base_url", "endpoint"} {
		identity += "\x00" + key + "=" + str(item, key, "")
	}
	return identity
}
func hasRedacted(value any) bool {
	switch node := value.(type) {
	case string:
		return node == redacted
	case map[string]any:
		for _, v := range node {
			if hasRedacted(v) {
				return true
			}
		}
	case []any:
		for _, v := range node {
			if hasRedacted(v) {
				return true
			}
		}
	}
	return false
}

func bridgePolicy(data map[string]any) (map[string]any, error) {
	local := readObject(data, "local_acp")
	enabled, ok := local["accept_client_mcp_servers"].(bool)
	if local["accept_client_mcp_servers"] != nil && !ok {
		return nil, errors.New("local_acp.accept_client_mcp_servers must be boolean")
	}
	if !enabled {
		return map[string]any{"enabled": false, "allowed_commands": []string{}}, nil
	}
	items, ok := local["client_mcp_allowed_commands"].([]any)
	if !ok || len(items) < 1 || len(items) > 32 {
		return nil, errors.New("local_acp.client_mcp_allowed_commands must contain 1..32 absolute executables")
	}
	commands := make([]string, 0, len(items))
	seen := make(map[string]bool)
	for _, item := range items {
		path, ok := item.(string)
		if !ok || !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
			return nil, errors.New("MCP commands must be absolute executable paths")
		}
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, errors.New("MCP executable does not exist")
		}
		info, err := os.Stat(real)
		if err != nil || !info.Mode().IsRegular() {
			return nil, errors.New("MCP executable must be an existing file")
		}
		key := real
		if runtime.GOOS == "windows" {
			key = strings.ToLower(real)
		}
		if !seen[key] {
			seen[key] = true
			commands = append(commands, real)
		}
	}
	return map[string]any{"enabled": true, "allowed_commands": commands}, nil
}

func (s service) lock() (*flock.Flock, error) {
	locker := flock.New(filepath.Join(s.userData, ".config-tool.lock"))
	if err := locker.Lock(); err != nil {
		return nil, err
	}
	return locker, nil
}
