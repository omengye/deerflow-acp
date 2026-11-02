// Package runtimepaths keeps portable launchers on the installation's selected
// database. A command-line or inherited environment value cannot override a
// portable binding. Standalone configurations retain their existing defaults.
package runtimepaths

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func samePath(a, b string) bool {
	a, _ = filepath.Abs(a)
	b, _ = filepath.Abs(b)
	if resolved, err := filepath.EvalSymlinks(a); err == nil {
		a = resolved
	}
	if resolved, err := filepath.EvalSymlinks(b); err == nil {
		b = resolved
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// UserData identifies a portable profile from its explicit config first. A
// standalone --config must not accidentally inherit the executable's profile.
func UserData(config, portableRoot, executable string) (string, error) {
	if config != "" {
		abs, err := filepath.Abs(config)
		if err != nil {
			return "", err
		}
		parent := filepath.Dir(abs)
		if strings.EqualFold(filepath.Base(parent), "config") && strings.EqualFold(filepath.Base(filepath.Dir(parent)), "user-data") {
			selected := filepath.Dir(parent)
			if portableRoot != "" && !samePath(selected, filepath.Join(portableRoot, "user-data")) {
				return "", fmt.Errorf("portable root and configuration select different user-data directories")
			}
			return selected, nil
		}
		if portableRoot != "" {
			return "", fmt.Errorf("configuration is outside the selected portable profile")
		}
		return "", nil
	}
	if portableRoot != "" {
		return filepath.Abs(filepath.Join(portableRoot, "user-data"))
	}
	root := filepath.Dir(executable)
	if fileExists(filepath.Join(root, "resources", "default-config.yaml")) || fileExists(filepath.Join(root, "user-data", "config", "config.yaml")) {
		return filepath.Abs(filepath.Join(root, "user-data"))
	}
	return "", nil
}

func profile(config string) (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	return UserData(config, os.Getenv("DEER_FLOW_PORTABLE_ROOT"), executable)
}

// Config discovers only the selected portable config, never a file in cwd.
func Config(requested string) (string, error) {
	if requested != "" {
		return requested, nil
	}
	if configured := os.Getenv("DEER_FLOW_CONFIG_PATH"); configured != "" {
		return configured, nil
	}
	data, err := profile("")
	if err != nil || data == "" {
		return "", err
	}
	config := filepath.Join(data, "config", "config.yaml")
	if !fileExists(config) {
		return "", fmt.Errorf("portable configuration is missing: %s", config)
	}
	return config, nil
}

func Runtime(config string) (string, error) {
	data, err := profile(config)
	if err != nil || data == "" {
		return "", err
	}
	return filepath.Join(data, "runtime", "acp-go"), nil
}

func Resolve(config, requested string) (string, error) {
	data, err := profile(config)
	if err != nil {
		return "", err
	}
	if data != "" {
		return ResolvePortable(data, requested)
	}
	if requested != "" || config == "" {
		return requested, nil
	}
	abs, err := filepath.Abs(config)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(abs), "go-harness-state"), nil
}

func ResolvePortable(userData, requested string) (string, error) {
	selected := filepath.Join(userData, "data", "go-harness")
	path := filepath.Join(userData, "config", "runtime-data.json")
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		if fileExists(filepath.Join(userData, "config", "go-harness-state", "harness.db")) {
			return "", fmt.Errorf("runtime-data.json is missing while a historical database exists; refusing to choose another store")
		}
	} else if err != nil {
		return "", err
	} else {
		defer file.Close()
		raw, err := io.ReadAll(io.LimitReader(file, 8193))
		if err != nil {
			return "", err
		}
		if len(raw) > 8192 {
			return "", fmt.Errorf("runtime-data.json exceeds 8192 bytes")
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		// Match serde's exact, single field contract. Struct decoding alone
		// accepts duplicate/case-insensitive keys, which can select a different
		// store than another launcher reading the same file.
		if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
			return "", fmt.Errorf("runtime-data.json must contain one data_dir field")
		}
		if token, err := decoder.Token(); err != nil || token != "data_dir" {
			return "", fmt.Errorf("runtime-data.json must contain one data_dir field")
		}
		var relative string
		if err := decoder.Decode(&relative); err != nil {
			return "", fmt.Errorf("invalid runtime-data.json data_dir: %w", err)
		}
		if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
			return "", fmt.Errorf("runtime-data.json must contain one data_dir field")
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			return "", fmt.Errorf("invalid trailing content in runtime-data.json")
		}
		if relative == "" || !filepath.IsLocal(relative) || strings.Contains(relative, ":") {
			return "", fmt.Errorf("bound data directory must be relative to user-data")
		}
		for _, part := range strings.FieldsFunc(relative, func(r rune) bool { return r == '/' || r == '\\' }) {
			if part == ".." {
				return "", fmt.Errorf("bound data directory cannot traverse parents")
			}
		}
		selected = filepath.Join(userData, relative)
		if !fileExists(filepath.Join(selected, "harness.db")) {
			return "", fmt.Errorf("bound database is missing: %s; refusing to open another store", selected)
		}
		resolved, err := filepath.EvalSymlinks(selected)
		if err != nil {
			return "", err
		}
		root, err := filepath.EvalSymlinks(userData)
		if err != nil {
			return "", err
		}
		rel, err := filepath.Rel(root, resolved)
		if err != nil || !filepath.IsLocal(rel) {
			return "", fmt.Errorf("bound data directory escapes user-data")
		}
	}
	if requested != "" && !samePath(requested, selected) {
		return "", fmt.Errorf("data directory override conflicts with portable binding: requested %s, selected %s", requested, selected)
	}
	return filepath.Abs(selected)
}
