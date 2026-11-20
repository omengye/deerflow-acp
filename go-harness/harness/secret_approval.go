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

// ExplicitSecretApproval guards high-impact secret operations and tool calls
// that will materialize or consume vault secrets. Invalid arguments on
// guarded tools fail closed as requiring explicit confirmation.
func ExplicitSecretApproval(p PermissionRequest) bool {
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
		if len(in.Secrets) > 0 || in.SecretOut || len(in.SecretPlan.Read) > 0 || len(in.SecretPlan.Write) > 0 || len(in.Credential) > 0 && string(in.Credential) != "null" {
			return true
		}
		return (!p.TrustedBuiltin || p.ToolName == "web_fetch") && hasJSONSecretPlaceholder(p.Arguments)
	}
	// External tools (e.g. MCP) and web_fetch substitute {{secret:NAME}}
	// placeholders; require an explicit per-call decision whenever arguments
	// contain a placeholder (including JSON-escaped spellings).
	if !p.TrustedBuiltin || p.ToolName == "web_fetch" {
		return hasJSONSecretPlaceholder(p.Arguments)
	}
	return false
}

func hasJSONSecretPlaceholder(raw json.RawMessage) bool {
	return containsJSONMarker(raw, "{{secret:") || HasCredentialMarker(raw)
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
