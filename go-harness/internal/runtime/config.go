package runtime

import (
	"context"
	"fmt"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// ConfigSettings must be installed before the service is shared with clients.
// Model identifiers use the host's configured provider and credentials.
type ConfigSettings struct {
	Models           []harness.ConfigValue
	EnableSubagents  bool
	DefaultSubagents bool
}

func (s *Service) modelOptions() []harness.ConfigValue {
	var options []harness.ConfigValue
	seen := make(map[string]bool)
	for _, option := range s.Settings.Models {
		if option.Value == "" || seen[option.Value] {
			continue
		}
		seen[option.Value] = true
		if option.Name == "" {
			option.Name = option.Value
		}
		options = append(options, option)
	}
	if s.Model != "" && !seen[s.Model] {
		options = append(options, harness.ConfigValue{Value: s.Model, Name: s.Model})
	}
	return options
}

func (s *Service) ConfigOptions(x harness.Session) []harness.ConfigOption {
	options := make([]harness.ConfigOption, 0, 3)
	if models := s.modelOptions(); len(models) > 0 {
		options = append(options, harness.ConfigOption{ID: "model", Name: "Model", Category: "model", Type: "select", CurrentValue: x.Model, Options: models})
	}
	approval := x.ApprovalMode
	if approval == "" {
		approval = harness.ApprovalAsk
	}
	options = append(options, harness.ConfigOption{ID: "approval", Name: "Tool approval", Type: "select", CurrentValue: approval, Options: []harness.ConfigValue{
		{Value: harness.ApprovalAsk, Name: "Ask before restricted tools"},
		{Value: harness.ApprovalAllowAlways, Name: "Allow tools in this session", Description: "Explicitly authorize tool requests; server startup policy and plan mode still apply."},
		{Value: harness.ApprovalRejectAlways, Name: "Reject restricted tools"},
		{Value: harness.ApprovalReadOnly, Name: "Local read-only tools", Description: "Only trusted built-in file reading, listing and searching are permitted."},
	}})
	if s.Settings.EnableSubagents {
		value := "off"
		if x.Subagents {
			value = "on"
		}
		options = append(options, harness.ConfigOption{ID: "subagent", Name: "Subagents", Type: "select", CurrentValue: value, Options: []harness.ConfigValue{{Value: "on", Name: "Enabled"}, {Value: "off", Name: "Disabled"}}})
	}
	return options
}

func (s *Service) SetConfigOption(ctx context.Context, owner, id, key, value string) ([]harness.ConfigOption, error) {
	ctx, release, err := s.Coordinator.Begin(ctx, id, owner)
	if err != nil {
		return nil, err
	}
	defer release()
	x, err := s.Store.Session(ctx, id)
	if err != nil {
		return nil, err
	}
	valid := false
	for _, option := range s.ConfigOptions(x) {
		if option.ID == key {
			for _, choice := range option.Options {
				if choice.Value == value {
					valid = true
					break
				}
			}
			break
		}
	}
	if !valid {
		return nil, fmt.Errorf("%w: unknown configuration option or value", harness.ErrInvalidInput)
	}
	switch key {
	case "model":
		x.Model = value
	case "approval":
		x.ApprovalMode = value
	case "subagent":
		x.Subagents = value == "on"
	}
	x.ConfigVersion++
	if err = s.Store.SaveConfig(ctx, x); err != nil {
		return nil, err
	}
	s.clearDecisions(owner, id)
	return s.ConfigOptions(x), nil
}

func (s *Service) clearDecisions(owner, id string) {
	prefix := owner + "/"
	if id != "" {
		prefix += id + "/"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.decisions {
		if strings.HasPrefix(key, prefix) {
			delete(s.decisions, key)
		}
	}
}

// Permission policy is applied before cached user choices. MCP annotations and
// namespaced tool names cannot impersonate these reserved built-in local tools.
func configuredPermission(x harness.Session, p harness.PermissionRequest) (harness.PermissionDecision, bool) {
	if x.Mode == "plan" || x.ApprovalMode == harness.ApprovalReadOnly {
		switch p.ToolName {
		case "read_file", "list_directory", "search_files":
			return harness.AllowOnce, true
		default:
			return harness.RejectOnce, true
		}
	}
	switch x.ApprovalMode {
	case harness.ApprovalAllowAlways:
		return harness.AllowOnce, true
	case harness.ApprovalRejectAlways:
		return harness.RejectOnce, true
	}
	return "", false
}

func (s *Store) SaveConfig(ctx context.Context, x harness.Session) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE harness_sessions SET model=?,mode=?,updated_at=? WHERE id=?`, x.Model, x.Mode, timestamp(), x.ID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return harness.ErrNotFound
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO harness_session_configs(session_id,approval_mode,subagents,version) VALUES(?,?,?,?) ON CONFLICT(session_id) DO UPDATE SET approval_mode=excluded.approval_mode,subagents=excluded.subagents,version=excluded.version`, x.ID, x.ApprovalMode, x.Subagents, x.ConfigVersion)
	if err != nil {
		return err
	}
	return tx.Commit()
}
