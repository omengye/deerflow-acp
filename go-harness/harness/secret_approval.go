package harness

import (
	"bytes"
	"encoding/json"
	"strings"
)

// ExplicitApproval reports whether p requires an explicit per-call human
// decision that cannot be bypassed by broad session/deployment allow grants or
// cached AllowAlways decisions.
func ExplicitApproval(p PermissionRequest) bool {
	return ExplicitSkillEditApproval(p) || ExplicitSecretApproval(p)
}

// ExplicitSecretApproval protects vault mutations and disclosure of secrets
// without a host-bound consumer. Valid scoped reads use the tool's policy.
func ExplicitSecretApproval(p PermissionRequest) bool {
	if !p.TrustedBuiltin || p.ToolName == "web_fetch" {
		if containsJSONMarker(p.Arguments, "{{secret:") || p.CredentialScope == nil && HasCredentialMarker(p.Arguments) {
			return true
		}
	}
	switch p.ToolName {
	case "secret_manage":
		var in struct {
			Action       string `json:"action"`
			DryRun       bool   `json:"dry_run"`
			DeleteSource bool   `json:"delete_source"`
		}
		if json.Unmarshal(p.Arguments, &in) != nil {
			return true
		}
		switch in.Action {
		case "set":
			return in.DeleteSource
		case "delete", "restore":
			return true
		case "import_config":
			return !in.DryRun
		}
		return false
	case "execute", "bash", "skill_run", "web_fetch":
		if len(bytes.TrimSpace(p.Arguments)) == 0 {
			return false
		}
		var in struct {
			Credential json.RawMessage `json:"credential"`
			SecretPlan struct {
				Read  map[string]string `json:"read"`
				Write []string          `json:"write"`
			} `json:"secret_plan"`
			Secrets   map[string]string `json:"secrets"`
			SecretOut bool              `json:"secret_out"`
		}
		if json.Unmarshal(p.Arguments, &in) != nil {
			return true
		}
		if len(in.Secrets) > 0 || in.SecretOut || len(in.SecretPlan.Write) > 0 ||
			len(in.SecretPlan.Read) > 0 && !(p.TrustedBuiltin && p.ToolName == "skill_run") ||
			len(in.Credential) > 0 && string(in.Credential) != "null" && p.CredentialScope == nil {
			return true
		}
	}
	return false
}

func HasCredentialMarker(raw json.RawMessage) bool { return containsJSONMarker(raw, "{{credential:") }

func containsJSONMarker(raw json.RawMessage, prefix string) bool {
	s := string(raw)
	if !strings.Contains(s, prefix) && !strings.Contains(s, `\`) {
		return false
	}
	if !json.Valid(raw) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '"' {
			continue
		}
		end, escaped := i+1, false
		for ; end < len(s) && s[end] != '"'; end++ {
			if s[end] == '\\' {
				escaped = true
				end++
			}
		}
		if end >= len(s) {
			break
		}
		text := s[i+1 : end]
		if escaped {
			if json.Unmarshal([]byte(s[i:end+1]), &text) != nil {
				return true
			}
		}
		if strings.Contains(text, prefix) {
			return true
		}
		i = end
	}
	return false
}
