package launch

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadACPAgentsConfig(t *testing.T) {
	t.Setenv("DEERFLOW_EXTERNAL_TEST_KEY", "private-value")
	file := filepath.Join(t.TempDir(), "agents.json")
	if err := os.WriteFile(file, []byte(`{"fixture":{"command":"/absolute/agent","env":{"KEY":"$DEERFLOW_EXTERNAL_TEST_KEY"},"timeout_seconds":30}}`), 0600); err != nil {
		t.Fatal(err)
	}
	agents, err := LoadACPAgentsConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	if agents["fixture"].Env["KEY"] != "private-value" {
		t.Fatal("environment reference not resolved")
	}
	if err := os.WriteFile(file, []byte(`{"fixture":{"command":"/absolute/agent","auto_approve_permissions":true}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadACPAgentsConfig(file); err == nil {
		t.Fatal("unsafe unknown option accepted")
	}
}
