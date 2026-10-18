package adapter

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
	"github.com/pelletier/go-toml/v2"
)

type Config struct {
	Buzz struct {
		Command         string            `toml:"command"`
		Args            []string          `toml:"args"`
		RelayURL        string            `toml:"relay_url"`
		AgentPubkey     string            `toml:"agent_pubkey"`
		Channels        []string          `toml:"channels"`
		AutoDiscover    bool              `toml:"auto_discover_channels"`
		IncludeDMs      bool              `toml:"include_dms"`
		RequireMention  bool              `toml:"require_mention"`
		AllowedPubkeys  []string          `toml:"allowed_pubkeys"`
		AllowAnySender  bool              `toml:"allow_any_sender"`
		MessageKinds    []int             `toml:"message_kinds"`
		Timeout         float64           `toml:"timeout_seconds"`
		ChannelNames    map[string]string `toml:"channel_names"`
		Acknowledgement struct {
			Enabled bool    `toml:"enabled"`
			Emoji   string  `toml:"emoji"`
			Timeout float64 `toml:"timeout_seconds"`
		} `toml:"acknowledgement"`
		Profile struct {
			Name   string `toml:"name"`
			Avatar string `toml:"avatar"`
			About  string `toml:"about"`
			NIP05  string `toml:"nip05"`
		} `toml:"profile"`
		Lifecycle struct {
			Presence  bool    `toml:"presence_enabled"`
			Heartbeat float64 `toml:"presence_heartbeat_seconds"`
			Status    bool    `toml:"status_enabled"`
			Ready     string  `toml:"status_ready"`
			Working   string  `toml:"status_working"`
			Error     string  `toml:"status_error"`
			Emoji     string  `toml:"status_emoji"`
		} `toml:"lifecycle"`
	} `toml:"buzz"`
	Deerflow struct {
		Command            string            `toml:"command"`
		Args               []string          `toml:"args"`
		Protocol           string            `toml:"protocol"`
		Workspace          string            `toml:"workspace"`
		DesktopRoot        string            `toml:"desktop_root"`
		ConfigPath         string            `toml:"config_path"`
		RuntimeDir         string            `toml:"runtime_dir"`
		DataDir            string            `toml:"data_dir"`
		Env                map[string]string `toml:"env"`
		Timeout            float64           `toml:"timeout_seconds"`
		PromptTimeout      float64           `toml:"prompt_timeout_seconds"`
		PermissionPolicy   string            `toml:"permission_policy"`
		ScheduleFullAccess bool              `toml:"schedule_full_access"`
	} `toml:"deerflow"`
	Adapter struct {
		Replay        bool    `toml:"replay_existing"`
		PollInterval  float64 `toml:"poll_interval_seconds"`
		MaxPages      int     `toml:"max_poll_pages"`
		RetryAttempts int     `toml:"transport_retry_attempts"`
		RetryBase     float64 `toml:"transport_retry_base_seconds"`
		RetryMax      float64 `toml:"transport_retry_max_seconds"`
		MaxAttempts   int     `toml:"max_message_attempts"`
		SessionScope  string  `toml:"session_scope"`
		LogLevel      string  `toml:"log_level"`
	} `toml:"adapter"`
	State struct {
		Path string `toml:"path"`
	} `toml:"state"`
	Observability struct {
		Enabled   bool    `toml:"enabled"`
		Owner     string  `toml:"owner_pubkey"`
		QueueSize int     `toml:"queue_size"`
		Timeout   float64 `toml:"publish_timeout_seconds"`
		Raw       bool    `toml:"include_raw_tool_data"`
		Progress  bool    `toml:"progress_messages"`
		Interval  float64 `toml:"progress_update_interval_seconds"`
		MaxTools  int     `toml:"progress_max_tools"`
	} `toml:"observability"`
	PrivateKey string   `toml:"-"`
	AuthTag    []string `toml:"-"`
}

func Defaults() Config {
	var c Config
	c.Buzz.Command = "buzz"
	c.Buzz.AutoDiscover = true
	c.Buzz.IncludeDMs = true
	c.Buzz.RequireMention = true
	c.Buzz.MessageKinds = []int{9}
	c.Buzz.Timeout = 60
	c.Buzz.Acknowledgement.Enabled = true
	c.Buzz.Acknowledgement.Emoji = "👀"
	c.Buzz.Acknowledgement.Timeout = 3
	c.Buzz.Lifecycle.Presence = true
	c.Buzz.Lifecycle.Heartbeat = 60
	c.Buzz.Lifecycle.Status = true
	c.Buzz.Lifecycle.Ready = "DeerFlow ready"
	c.Buzz.Lifecycle.Working = "DeerFlow working"
	c.Buzz.Lifecycle.Error = "DeerFlow error"
	c.Deerflow.Protocol = "v2"
	c.Deerflow.Workspace = "."
	c.Deerflow.Timeout = 120
	c.Deerflow.PermissionPolicy = "deny"
	c.Adapter.PollInterval = 4
	c.Adapter.MaxPages = 20
	c.Adapter.RetryAttempts = 3
	c.Adapter.RetryBase = 1
	c.Adapter.RetryMax = 4
	c.Adapter.MaxAttempts = 5
	c.Adapter.SessionScope = "thread"
	c.Adapter.LogLevel = "INFO"
	c.State.Path = "data/adapter.sqlite3"
	c.Observability.QueueSize = 256
	c.Observability.Timeout = 10
	c.Observability.Interval = .5
	c.Observability.MaxTools = 12
	return c
}

// LoadEnv loads only the two supported secrets, with process environment taking
// precedence. Its errors never contain file contents or credential values.
func LoadEnv(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read adapter environment: %w", err)
	}
	for _, line := range strings.Split(strings.TrimPrefix(string(data), "\ufeff"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || (k != "BUZZ_PRIVATE_KEY" && k != "BUZZ_AUTH_TAG") {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && ((v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'')) {
			v = v[1 : len(v)-1]
		}
		if _, exists := os.LookupEnv(k); !exists {
			if err = os.Setenv(k, v); err != nil {
				return errors.New("cannot load adapter environment")
			}
		}
	}
	return nil
}

func LoadConfig(path string) (Config, error) {
	c := Defaults()
	abs, err := filepath.Abs(path)
	if err != nil {
		return c, err
	}
	base := filepath.Dir(abs)
	data, err := os.ReadFile(abs)
	if err != nil {
		return c, err
	}
	if err = toml.Unmarshal([]byte(strings.TrimPrefix(string(data), "\ufeff")), &c); err != nil {
		return c, errors.New("invalid adapter TOML; check field names and types")
	}
	if c.Buzz.RelayURL == "" {
		c.Buzz.RelayURL = os.Getenv("BUZZ_RELAY_URL")
	}
	if c.Buzz.AgentPubkey == "" {
		c.Buzz.AgentPubkey = os.Getenv("BUZZ_AGENT_PUBKEY")
	}
	resolve := func(value string) string {
		if value == "" {
			return ""
		}
		if !filepath.IsAbs(value) {
			value = filepath.Join(base, value)
		}
		return filepath.Clean(value)
	}
	c.Deerflow.Workspace = resolve(c.Deerflow.Workspace)
	c.State.Path = resolve(c.State.Path)
	c.Deerflow.DesktopRoot = resolve(c.Deerflow.DesktopRoot)
	if root := c.Deerflow.DesktopRoot; root != "" {
		if c.Deerflow.Command == "" {
			c.Deerflow.Command = filepath.Join(root, executableName("deerflow-acp"))
		}
		if c.Deerflow.ConfigPath == "" {
			c.Deerflow.ConfigPath = filepath.Join(root, "user-data/config/config.yaml")
		}
		if c.Deerflow.RuntimeDir == "" {
			c.Deerflow.RuntimeDir = filepath.Join(root, "user-data/runtime/acp-go")
		}
		if c.Deerflow.DataDir == "" {
			c.Deerflow.DataDir = filepath.Join(root, "user-data/data/go-harness")
		}
	}
	c.Deerflow.ConfigPath = resolve(c.Deerflow.ConfigPath)
	c.Deerflow.RuntimeDir = resolve(c.Deerflow.RuntimeDir)
	c.Deerflow.DataDir = resolve(c.Deerflow.DataDir)
	for i := 0; i < len(c.Deerflow.Args); i++ {
		flag := c.Deerflow.Args[i]
		switch flag {
		case "--no-auto-start":
		case "--protocol", "--config", "--runtime-dir", "--daemon", "--mcp-allow-command":
			if i+1 >= len(c.Deerflow.Args) {
				return c, fmt.Errorf("missing value for %s", flag)
			}
			i++
			if flag != "--protocol" {
				c.Deerflow.Args[i] = resolve(c.Deerflow.Args[i])
				if flag == "--config" && c.Deerflow.ConfigPath == "" {
					c.Deerflow.ConfigPath = c.Deerflow.Args[i]
				}
				if flag == "--runtime-dir" && c.Deerflow.RuntimeDir == "" {
					c.Deerflow.RuntimeDir = c.Deerflow.Args[i]
				}
			}
		default:
			return c, errors.New("deerflow.args contains an unsupported Bridge option")
		}
	}
	for _, item := range []struct{ flag, value string }{{"--protocol", "v2"}, {"--config", c.Deerflow.ConfigPath}, {"--runtime-dir", c.Deerflow.RuntimeDir}} {
		if item.value == "" {
			continue
		}
		seen := false
		for i, arg := range c.Deerflow.Args {
			if arg == item.flag {
				if i+1 >= len(c.Deerflow.Args) || c.Deerflow.Args[i+1] != item.value {
					return c, fmt.Errorf("deerflow.args conflicts with %s", item.flag)
				}
				seen = true
			} else if strings.HasPrefix(arg, item.flag+"=") {
				return c, fmt.Errorf("use separate argument/value for %s", item.flag)
			}
		}
		if !seen {
			c.Deerflow.Args = append(c.Deerflow.Args, item.flag, item.value)
		}
	}
	for _, target := range []*string{&c.Buzz.Command, &c.Deerflow.Command} {
		if strings.ContainsAny(*target, "/\\") {
			*target = resolve(*target)
		} else if *target != "" {
			p, e := exec.LookPath(*target)
			if e != nil {
				return c, fmt.Errorf("executable not found: %s", *target)
			}
			*target = p
		}
	}
	if c.Deerflow.Env == nil {
		c.Deerflow.Env = map[string]string{}
	}
	if c.Deerflow.DataDir != "" {
		c.Deerflow.Env["DEERFLOW_GO_DATA_DIR"] = c.Deerflow.DataDir
	}
	if err = c.Validate(os.Getenv("BUZZ_PRIVATE_KEY"), os.Getenv("BUZZ_AUTH_TAG")); err != nil {
		return c, err
	}
	return c, nil
}

func (c *Config) Validate(secret, auth string) error {
	u, err := url.Parse(c.Buzz.RelayURL)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "ws" && u.Scheme != "wss" && u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("buzz.relay_url must be an HTTP(S) or WS(S) relay URL")
	}
	if strings.HasPrefix(secret, "nsec1") {
		prefix, value, e := nip19.Decode(secret)
		if e != nil || prefix != "nsec" {
			return errors.New("BUZZ_PRIVATE_KEY is invalid")
		}
		secret, _ = value.(string)
	}
	raw, e := hex.DecodeString(secret)
	n := new(big.Int).SetBytes(raw)
	if e != nil || len(raw) != 32 || n.Sign() <= 0 || n.Cmp(btcec.S256().Params().N) >= 0 {
		return errors.New("BUZZ_PRIVATE_KEY must be a valid Nostr private key")
	}
	c.PrivateKey = strings.ToLower(secret)
	pub, e := nostr.GetPublicKey(c.PrivateKey)
	if e != nil || !strings.EqualFold(pub, c.Buzz.AgentPubkey) {
		return errors.New("BUZZ_PRIVATE_KEY does not match buzz.agent_pubkey")
	}
	c.Buzz.AgentPubkey = pub
	if auth != "" {
		if json.Unmarshal([]byte(auth), &c.AuthTag) != nil || len(c.AuthTag) != 4 || c.AuthTag[0] != "auth" || !nostr.IsValidPublicKey(strings.ToLower(c.AuthTag[1])) {
			return errors.New("BUZZ_AUTH_TAG must be a four-element owner auth tag")
		}
	}
	c.Observability.Owner = strings.ToLower(c.Observability.Owner)
	if len(c.AuthTag) > 0 {
		if c.Observability.Owner != "" && !strings.EqualFold(c.Observability.Owner, c.AuthTag[1]) {
			return errors.New("observability.owner_pubkey conflicts with BUZZ_AUTH_TAG")
		}
		c.Observability.Owner = strings.ToLower(c.AuthTag[1])
	}
	if len(c.Buzz.AllowedPubkeys) == 0 && !c.Buzz.AllowAnySender && c.Observability.Owner != "" {
		c.Buzz.AllowedPubkeys = []string{c.Observability.Owner}
	}
	if len(c.Buzz.AllowedPubkeys) == 0 && !c.Buzz.AllowAnySender {
		return errors.New("configure buzz.allowed_pubkeys, owner attestation, or explicitly enable buzz.allow_any_sender")
	}
	for i, pub := range c.Buzz.AllowedPubkeys {
		pub = strings.ToLower(pub)
		if !nostr.IsValidPublicKey(pub) {
			return errors.New("buzz.allowed_pubkeys contains an invalid public key")
		}
		c.Buzz.AllowedPubkeys[i] = pub
	}
	if c.Observability.Enabled && !nostr.IsValidPublicKey(c.Observability.Owner) {
		return errors.New("native observability requires a valid owner public key")
	}
	if c.Deerflow.Protocol != "v2" {
		return errors.New("native Buzz adapter requires deerflow.protocol=v2 and the Rust deerflow-acp Bridge")
	}
	if c.Deerflow.PermissionPolicy != "deny" && c.Deerflow.PermissionPolicy != "allow_once" {
		return errors.New("deerflow.permission_policy must be deny or allow_once")
	}
	if c.Deerflow.PermissionPolicy == "allow_once" && c.Buzz.AllowAnySender {
		return errors.New("allow_once tool approval requires an explicit sender allowlist")
	}
	if c.Deerflow.ScheduleFullAccess && (c.Buzz.AllowAnySender || len(c.Buzz.AllowedPubkeys) == 0) {
		return errors.New("schedule_full_access requires an explicit sender allowlist and allow_any_sender=false")
	}
	for _, file := range []string{c.Buzz.Command, c.Deerflow.Command} {
		info, e := os.Stat(file)
		if e != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("executable is missing: %s", file)
		}
	}
	if strings.Contains(strings.ToLower(filepath.Base(c.Deerflow.Command)), "acp-go") {
		return errors.New("use deerflow-acp, not the v1-only deerflow-acp-go")
	}
	if c.Deerflow.DesktopRoot != "" {
		if _, e := os.Stat(filepath.Join(c.Deerflow.DesktopRoot, executableName("deerflow-acpd"))); e != nil {
			return errors.New("desktop_root must contain the Eino Go deerflow-acpd executable")
		}
	}
	if info, e := os.Stat(c.Deerflow.Workspace); e != nil || !info.IsDir() {
		return errors.New("deerflow.workspace must be an existing directory")
	}
	workspace, e := filepath.EvalSymlinks(c.Deerflow.Workspace)
	if e != nil {
		return errors.New("cannot resolve deerflow.workspace")
	}
	c.Deerflow.Workspace = workspace
	if c.Deerflow.ConfigPath != "" {
		if info, e := os.Stat(c.Deerflow.ConfigPath); e != nil || !info.Mode().IsRegular() {
			return errors.New("deerflow.config_path is missing; configure the Go desktop first")
		}
	}
	if c.Adapter.SessionScope != "thread" && c.Adapter.SessionScope != "channel" {
		return errors.New("adapter.session_scope must be thread or channel")
	}
	if c.Buzz.Acknowledgement.Enabled && strings.TrimSpace(c.Buzz.Acknowledgement.Emoji) == "" {
		return errors.New("buzz.acknowledgement.emoji must not be empty when enabled")
	}
	if c.Adapter.MaxPages < 1 || c.Adapter.MaxPages > 1000 || c.Adapter.RetryAttempts < 1 || c.Adapter.RetryAttempts > 20 || c.Adapter.MaxAttempts < 1 || c.Observability.QueueSize < 1 || c.Observability.QueueSize > 4096 || c.Observability.MaxTools < 1 {
		return errors.New("adapter retry, paging or observability limits are invalid")
	}
	for _, v := range []float64{c.Buzz.Timeout, c.Buzz.Acknowledgement.Timeout, c.Deerflow.Timeout, c.Adapter.PollInterval, c.Adapter.RetryBase, c.Adapter.RetryMax, c.Buzz.Lifecycle.Heartbeat, c.Observability.Timeout} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 || v > 86400 {
			return errors.New("timeouts and intervals must be finite values in (0, 86400]")
		}
	}
	for _, v := range []float64{c.Deerflow.PromptTimeout, c.Observability.Interval} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 86400 {
			return errors.New("optional timeouts must be finite values in [0, 86400]")
		}
	}
	return nil
}

func seconds(v float64) time.Duration { return time.Duration(v * float64(time.Second)) }

// CheckDeployment runs only local --help commands. It never initializes ACP,
// opens the state database, loads a model or connects to a Buzz relay.
func (c Config) CheckDeployment(ctx context.Context) error {
	daemon := filepath.Join(filepath.Dir(c.Deerflow.Command), executableName("deerflow-acpd"))
	for i, arg := range c.Deerflow.Args {
		if arg == "--daemon" && i+1 < len(c.Deerflow.Args) {
			daemon = c.Deerflow.Args[i+1]
		}
	}
	checks := []struct {
		command, marker, label string
		args                   []string
	}{
		{c.Buzz.Command, "messages", "Buzz CLI", append(append([]string{}, c.Buzz.Args...), "--help")},
		{c.Deerflow.Command, "Eino Go", "native Go ACP Bridge", []string{"--help"}},
		{daemon, "Eino Go state directory", "Go ACP daemon", []string{"--help"}},
	}
	if c.Buzz.Acknowledgement.Enabled {
		checks = append(checks, struct {
			command, marker, label string
			args                   []string
		}{c.Buzz.Command, "--emoji", "Buzz reactions CLI", append(append([]string{}, c.Buzz.Args...), "reactions", "add", "--help")})
	}
	for _, check := range checks {
		deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
		cmd := exec.CommandContext(deadline, check.command, check.args...)
		hideCommand(cmd)
		var output bytes.Buffer
		bounded := &limitedOutput{w: &output, remaining: 128 << 10, cancel: cancel}
		cmd.Stdout, cmd.Stderr = bounded, bounded
		err := cmd.Run()
		cancel()
		if err != nil || !strings.Contains(output.String(), check.marker) {
			return fmt.Errorf("%s validation failed; use the current Eino Go release and supported official Buzz CLI", check.label)
		}
	}
	return nil
}

// Redact is also used by the command entry point for startup diagnostics.
func (c Config) Redact(value string) string { return c.redact(value) }
func (c Config) redact(value string) string {
	for _, key := range []string{os.Getenv("BUZZ_PRIVATE_KEY"), c.PrivateKey, os.Getenv("BUZZ_AUTH_TAG")} {
		if key != "" {
			value = strings.ReplaceAll(value, key, "[redacted]")
		}
	}
	if len(value) > 2000 {
		value = value[:2000]
	}
	return value
}
