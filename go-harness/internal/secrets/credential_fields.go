package secrets

import (
	"encoding/json"
	"errors"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// CredentialFields belongs to an individual approval, not the persistent scope.
type CredentialFields uint8

const (
	UsernameField CredentialFields = 1 << iota
	KeyField
	BothFields = UsernameField | KeyField
)

func (f CredentialFields) labels() []string {
	var labels []string
	if f&UsernameField != 0 {
		labels = append(labels, "username")
	}
	if f&KeyField != 0 {
		labels = append(labels, "key")
	}
	return labels
}

func (f CredentialFields) Names(s CredentialScope) []string {
	u, k := s.Names()
	var names []string
	if f&UsernameField != 0 {
		names = append(names, u)
	}
	if f&KeyField != 0 {
		names = append(names, k)
	}
	return names
}

func (b CredentialBinding) Fields() CredentialFields {
	var fields CredentialFields
	if b.UsernameEnv != "" {
		fields |= UsernameField
	}
	if b.KeyEnv != "" {
		fields |= KeyField
	}
	return fields
}

// ApprovalFields reads the original, host-approved arguments, including those
// in durable checkpoints written before field-selective enrollment existed.
func ApprovalFields(p harness.PermissionRequest) (CredentialFields, error) {
	if !p.TrustedBuiltin {
		return markerFields(string(p.Arguments))
	}
	switch p.ToolName {
	case "execute", "bash", "skill_run":
		var in struct{ Credential *CredentialBinding }
		if json.Unmarshal(p.Arguments, &in) == nil && in.Credential != nil {
			if f := in.Credential.Fields(); f != 0 {
				return f, nil
			}
		}
	case "web_fetch":
		var in struct{ Credential string }
		if json.Unmarshal(p.Arguments, &in) == nil {
			switch in.Credential {
			case "basic":
				return BothFields, nil
			case "bearer":
				return KeyField, nil
			}
		}
	}
	return 0, errors.New("invalid service credential field request")
}
