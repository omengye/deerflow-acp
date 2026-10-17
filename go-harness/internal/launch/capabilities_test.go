package launch

import (
	"os"
	"path/filepath"
	"testing"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
)

func TestApplyConfigGoalAndWeChatHostPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := `local_acp:
  goal_auto_continue: true
wechat:
  enabled: false
  allowed_users: [fixture-user]
  approval: read_only
models:
  - name: model
    provider: openai
    model: fixture-model
`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	var cfg deerflow.Config
	connections := 16
	if _, err := ApplyConfig(path, &cfg, &connections, nil); err != nil {
		t.Fatal(err)
	}
	if !cfg.GoalAutoContinue || cfg.WeChat.Enabled || cfg.WeChat.Approval != "read_only" || len(cfg.WeChat.AllowedUsers) != 1 {
		t.Fatal("host policy not loaded")
	}
}
