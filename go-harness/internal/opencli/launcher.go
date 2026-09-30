// Package opencli resolves host-installed OpenCLI launchers without running a
// shell or loading Python. The model cannot select a program or a working dir.
package opencli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type Launcher struct {
	Executable  string
	Prefix      []string
	Environment map[string]string
}

var managedShim = regexp.MustCompile(`(?m)^"([^"\r\n]+)" __opencli_shim %\*\s*$`)

func Resolve(configured string) (Launcher, error) {
	path := os.Getenv("OPENCLI_BIN")
	if path == "" {
		path = configured
	}
	if path == "" {
		path = "opencli"
	}
	if !filepath.IsAbs(path) {
		var err error
		path, err = exec.LookPath(path)
		if err != nil {
			return Launcher{}, errors.New("OpenCLI is not installed or its executable cannot be found")
		}
	}
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		return Launcher{}, errors.New("configured OpenCLI executable cannot be found")
	}
	plan := Launcher{Executable: path, Environment: environment()}
	if runtime.GOOS == "windows" && (strings.EqualFold(filepath.Ext(path), ".cmd") || strings.EqualFold(filepath.Ext(path), ".bat")) {
		body, err := readSmall(path)
		if err != nil {
			return Launcher{}, err
		}
		packageRoot := filepath.Join(filepath.Dir(path), "node_modules", "@jackwener", "opencli")
		if strings.Contains(string(body), "REM OPENCLIAPP_MANAGED_SHIM") {
			match := managedShim.FindSubmatch(body)
			if len(match) != 2 {
				return Launcher{}, errors.New("unrecognized OpenCLIApp managed shim")
			}
			packageRoot = filepath.Join(filepath.Dir(string(match[1])), "node_modules", "@jackwener", "opencli")
		}
		// Run the npm bin directly with Node, so %, &, quotes and newlines in
		// model arguments stay literal argv, even on Windows.
		manifest, err := readSmall(filepath.Join(packageRoot, "package.json"))
		if err != nil {
			return Launcher{}, errors.New("OpenCLI .cmd requires an installed @jackwener/opencli npm package; configure its executable directly")
		}
		var pkg struct {
			Name string
			Bin  json.RawMessage
		}
		if json.Unmarshal(manifest, &pkg) != nil || pkg.Name != "@jackwener/opencli" {
			return Launcher{}, errors.New("invalid OpenCLI npm package")
		}
		var bin string
		if json.Unmarshal(pkg.Bin, &bin) != nil {
			var bins map[string]string
			if json.Unmarshal(pkg.Bin, &bins) != nil {
				return Launcher{}, errors.New("invalid OpenCLI npm bin")
			}
			bin = bins["opencli"]
		}
		if bin == "" || !filepath.IsLocal(filepath.FromSlash(bin)) {
			return Launcher{}, errors.New("OpenCLI npm bin must stay inside its package")
		}
		script, err := filepath.EvalSymlinks(filepath.Join(packageRoot, filepath.FromSlash(bin)))
		if err != nil {
			return Launcher{}, errors.New("OpenCLI npm entry point is missing")
		}
		rel, err := filepath.Rel(packageRoot, script)
		if err != nil || !filepath.IsLocal(rel) {
			return Launcher{}, errors.New("OpenCLI npm entry point escapes its package")
		}
		node := filepath.Join(filepath.Dir(path), "node.exe")
		if _, err := os.Stat(node); err != nil {
			node, err = exec.LookPath("node.exe")
			if err != nil {
				return Launcher{}, errors.New("OpenCLI requires Node.js >=20; node.exe is not on PATH")
			}
		}
		plan.Executable, err = filepath.EvalSymlinks(node)
		if err != nil {
			return Launcher{}, errors.New("OpenCLI Node executable is missing")
		}
		plan.Prefix = []string{script}
	}
	info, err := os.Stat(plan.Executable)
	if err != nil || !info.Mode().IsRegular() {
		return Launcher{}, fmt.Errorf("OpenCLI executable must be a regular file")
	}
	return plan, nil
}

func readSmall(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return nil, errors.New("invalid OpenCLI launcher metadata")
	}
	return os.ReadFile(path)
}

func environment() map[string]string {
	values := map[string]string{"OPENCLI_BROWSER_CONNECT_TIMEOUT": "45", "OPENCLI_BROWSER_COMMAND_TIMEOUT": "90"}
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "HOMEDRIVE", "HOMEPATH", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "OPENCLI_BROWSER_CONNECT_TIMEOUT", "OPENCLI_BROWSER_COMMAND_TIMEOUT", "OPENCLI_BROWSER_PORT", "OPENCLI_CONFIG_DIR"} {
		if value := os.Getenv(key); value != "" {
			values[key] = value
		}
	}
	return values
}

func AllowedSites(config harness.BuiltinToolConfig) []string {
	if config.AllowedSites != nil {
		return config.AllowedSites
	}
	return []string{"web", "twitter", "xiaohongshu", "xiaoyuzhou", "weixin", "douyin", "bilibili"}
}
