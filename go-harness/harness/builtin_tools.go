package harness

import (
	"fmt"
	"net/url"
	"strings"
)

// BuiltinToolConfig is host-owned configuration. It is never exposed in model
// schemas, ACP options or checkpoints, which contain only a policy digest.
// Legacy use strings are aliases, not imports of Python modules.
type BuiltinToolConfig struct {
	Name           string   `yaml:"name" json:"name"`
	Group          string   `yaml:"group" json:"group,omitempty"`
	Use            string   `yaml:"use,omitempty" json:"use,omitempty"` // compatibility only; name selects native tool
	APIKey         string   `yaml:"api_key" json:"-"`
	HTTPSProxy     string   `yaml:"https_proxy" json:"-"`
	MaxResults     int      `yaml:"max_results" json:"maxResults,omitempty"`
	Timeout        int      `yaml:"timeout" json:"timeout,omitempty"` // seconds
	MaxOutputChars int      `yaml:"max_output_chars" json:"maxOutputChars,omitempty"`
	Executable     string   `yaml:"executable" json:"executable,omitempty"`
	AllowedSites   []string `yaml:"allowed_sites" json:"allowedSites,omitempty"`
}

var legacyBuiltinUses = map[string]string{
	"web_search":   "deerflow.community.brave_search.tools:web_search_tool",
	"web_fetch":    "deerflow.community.scrapling.tools:web_fetch_tool",
	"image_search": "deerflow.community.image_search.tools:image_search_tool",
	"host_opencli": "deerflow.tools.host_opencli:host_opencli_tool",
	"ls":           "deerflow.sandbox.tools:ls_tool", "read_file": "deerflow.sandbox.tools:read_file_tool",
	"glob": "deerflow.sandbox.tools:glob_tool", "grep": "deerflow.sandbox.tools:grep_tool",
	"write_file": "deerflow.sandbox.tools:write_file_tool", "str_replace": "deerflow.sandbox.tools:str_replace_tool",
	"move_path": "deerflow.sandbox.tools:move_path_tool", "delete_path": "deerflow.sandbox.tools:delete_path_tool",
	"bash": "deerflow.sandbox.tools:bash_tool",
}

func (c BuiltinToolConfig) Validate() error {
	legacy, ok := legacyBuiltinUses[c.Name]
	if !ok || (c.Use != "" && c.Use != "builtin:"+c.Name && c.Use != legacy) {
		return fmt.Errorf("unsupported Go tool provider for %q; use builtin:%s", c.Name, c.Name)
	}
	if c.Timeout < 0 || c.Timeout > 120 || c.MaxOutputChars < 0 || c.MaxOutputChars > 50_000 {
		return fmt.Errorf("%s: timeout must be 0..120 seconds and max_output_chars 0..50000", c.Name)
	}
	cap := 20
	if c.Name == "image_search" {
		cap = 50
	}
	if c.Name == "glob" {
		cap = 1000
	}
	if c.Name == "grep" {
		cap = 500
	}
	if c.MaxResults < 0 || c.MaxResults > cap {
		return fmt.Errorf("%s: max_results must be 0..%d", c.Name, cap)
	}
	if c.HTTPSProxy != "" && !strings.HasPrefix(c.HTTPSProxy, "$") {
		u, err := url.Parse(c.HTTPSProxy)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
			return fmt.Errorf("%s: invalid proxy URL", c.Name)
		}
	}
	if len(c.AllowedSites) > 64 {
		return fmt.Errorf("host_opencli: at most 64 allowed_sites")
	}
	for _, site := range c.AllowedSites {
		if !ValidOpenCLIWord(site) {
			return fmt.Errorf("host_opencli: invalid allowed site")
		}
	}
	if strings.ContainsAny(c.Executable, "\x00\r\n") {
		return fmt.Errorf("host_opencli: invalid executable")
	}
	return nil
}

// NativeModelProvider canonicalizes old configuration tags on read. New
// configuration files store provider=openai/claude rather than Python classes.
func NativeModelProvider(value string) (string, error) {
	switch value {
	case "openai", "langchain_openai:ChatOpenAI", "deerflow.models.patched_openai:PatchedChatOpenAI":
		return "openai", nil
	case "claude", "anthropic", "langchain_anthropic:ChatAnthropic":
		return "claude", nil
	default:
		return "", fmt.Errorf("unsupported Go model provider %q", value)
	}
}

func ValidOpenCLIWord(s string) bool {
	if s == "" || len(s) > 64 || s[0] == '-' {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
