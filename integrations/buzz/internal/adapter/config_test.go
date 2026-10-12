package adapter

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nbd-wtf/go-nostr"
)

const testSecret = "0000000000000000000000000000000000000000000000000000000000000001"
const ownerSecret = "0000000000000000000000000000000000000000000000000000000000000002"

func testConfig(t *testing.T) Config {
	t.Helper()
	c := Defaults()
	c.Buzz.RelayURL = "https://relay.invalid"
	c.Buzz.AgentPubkey, _ = nostr.GetPublicKey(testSecret)
	owner, _ := nostr.GetPublicKey(ownerSecret)
	c.Buzz.AllowedPubkeys = []string{owner}
	c.Buzz.Command, _ = os.Executable()
	c.Deerflow.Command = c.Buzz.Command
	c.Deerflow.Workspace = t.TempDir()
	c.PrivateKey = testSecret
	c.Buzz.AutoDiscover = false
	c.Buzz.IncludeDMs = false
	c.Buzz.Channels = []string{"channel"}
	c.Buzz.Lifecycle.Status = false
	c.Buzz.Lifecycle.Presence = false
	c.Adapter.RetryBase = .001
	c.Adapter.RetryMax = .002
	return c
}
func TestConfigPolicies(t *testing.T) {
	for _, tc := range []struct {
		name         string
		change       func(*Config)
		secret, auth string
		valid        bool
	}{
		{"valid", func(c *Config) {}, testSecret, "", true},
		{"zero key", func(c *Config) {}, strings.Repeat("0", 64), "", false},
		{"wrong key", func(c *Config) {}, ownerSecret, "", false},
		{"no sender policy", func(c *Config) { c.Buzz.AllowedPubkeys = nil }, testSecret, "", false},
		{"owner default", func(c *Config) {
			c.Buzz.AllowedPubkeys = nil
			c.Observability.Owner, _ = nostr.GetPublicKey(ownerSecret)
		}, testSecret, "", true},
		{"public deny", func(c *Config) { c.Buzz.AllowAnySender = true }, testSecret, "", true},
		{"public allow", func(c *Config) { c.Buzz.AllowAnySender = true; c.Deerflow.PermissionPolicy = "allow_once" }, testSecret, "", false},
		{"allow once", func(c *Config) { c.Deerflow.PermissionPolicy = "allow_once" }, testSecret, "", true},
		{"allow always", func(c *Config) { c.Deerflow.PermissionPolicy = "allow_always" }, testSecret, "", false},
		{"v1", func(c *Config) { c.Deerflow.Protocol = "v1" }, testSecret, "", false},
		{"direct v1", func(c *Config) {
			c.Deerflow.Command = filepath.Join(t.TempDir(), executableName("deerflow-acp-go"))
			_ = os.WriteFile(c.Deerflow.Command, []byte("fixture"), 0600)
		}, testSecret, "", false},
		{"bad auth", func(c *Config) {}, testSecret, "secret-value", false},
		{"bad relay", func(c *Config) { c.Buzz.RelayURL = "file:///tmp" }, testSecret, "", false},
		{"bad limits", func(c *Config) { c.Adapter.MaxPages = 0 }, testSecret, "", false},
		{"custom receipt emoji", func(c *Config) { c.Buzz.Acknowledgement.Emoji = "👍" }, testSecret, "", true},
		{"empty receipt emoji", func(c *Config) { c.Buzz.Acknowledgement.Emoji = "  " }, testSecret, "", false},
		{"disabled receipt", func(c *Config) { c.Buzz.Acknowledgement.Enabled = false; c.Buzz.Acknowledgement.Emoji = "" }, testSecret, "", true},
		{"zero receipt timeout", func(c *Config) { c.Buzz.Acknowledgement.Timeout = 0 }, testSecret, "", false},
		{"NaN receipt timeout", func(c *Config) { c.Buzz.Acknowledgement.Timeout = math.NaN() }, testSecret, "", false},
		{"missing workspace", func(c *Config) { c.Deerflow.Workspace = filepath.Join(t.TempDir(), "absent") }, testSecret, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			tc.change(&c)
			err := c.Validate(tc.secret, tc.auth)
			if (err == nil) != tc.valid {
				t.Fatalf("validation: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "secret-value") {
				t.Fatal("secret exposed")
			}
		})
	}
}
func TestEnvPrecedenceAndRedaction(t *testing.T) {
	t.Setenv("BUZZ_PRIVATE_KEY", "existing")
	t.Setenv("BUZZ_AUTH_TAG", "")
	p := filepath.Join(t.TempDir(), ".env")
	_ = os.WriteFile(p, []byte("\ufeffBUZZ_PRIVATE_KEY='file-secret'\nUNRELATED_SECRET=ignore\n"), 0600)
	if err := LoadEnv(p); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("BUZZ_PRIVATE_KEY") != "existing" {
		t.Fatal("environment replaced")
	}
	c := testConfig(t)
	if got := c.Redact("existing " + testSecret); strings.Contains(got, "existing") || strings.Contains(got, testSecret) {
		t.Fatal("redaction failed")
	}
}
func TestLoadConfigPathsAndArgs(t *testing.T) {
	root := t.TempDir()
	exe, _ := os.Executable()
	pub, _ := nostr.GetPublicKey(testSecret)
	owner, _ := nostr.GetPublicKey(ownerSecret)
	t.Setenv("BUZZ_PRIVATE_KEY", testSecret)
	t.Setenv("BUZZ_AUTH_TAG", "")
	for _, args := range []string{`["--protocol", "v1"]`, `["--status"]`, `["--python", "python"]`, `["--protocol"]`} {
		body := "[buzz]\ncommand = '" + exe + "'\nrelay_url='https://relay.invalid'\nagent_pubkey='" + pub + "'\nallowed_pubkeys=['" + owner + "']\n[deerflow]\ncommand='" + exe + "'\nargs=" + args + "\nworkspace='.'\n"
		p := filepath.Join(root, "adapter.toml")
		_ = os.WriteFile(p, []byte(body), 0600)
		if _, err := LoadConfig(p); err == nil {
			t.Fatalf("accepted args %s", args)
		}
	}
	body := "[buzz]\ncommand='" + exe + "'\nrelay_url='https://relay.invalid'\nagent_pubkey='" + pub + "'\nallowed_pubkeys=['" + owner + "']\n[deerflow]\ncommand='" + exe + "'\nworkspace='.'\n"
	p := filepath.Join(root, "adapter.toml")
	_ = os.WriteFile(p, []byte(body), 0600)
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if c.Deerflow.Workspace != canonicalRoot || !filepath.IsAbs(c.State.Path) {
		t.Fatal("paths not based on TOML")
	}
	if !c.Buzz.Acknowledgement.Enabled || c.Buzz.Acknowledgement.Emoji != "👀" || c.Buzz.Acknowledgement.Timeout != 3 {
		t.Fatal("old configuration did not inherit receipt defaults")
	}
	body += "[buzz.acknowledgement]\nenabled=false\nemoji='👍'\ntimeout_seconds=2\n"
	if err = os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	c, err = LoadConfig(p)
	if err != nil || c.Buzz.Acknowledgement.Enabled || c.Buzz.Acknowledgement.Emoji != "👍" || c.Buzz.Acknowledgement.Timeout != 2 {
		t.Fatal("receipt override ignored", err)
	}
}
