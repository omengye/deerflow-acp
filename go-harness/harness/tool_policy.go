package harness

import (
	"fmt"
	"strings"
)

// ToolPolicy is a deployment-owned tool boundary. A nil Allowlist permits
// tools unless denied; a non-nil empty Allowlist permits none. Names are exact.
type ToolPolicy struct {
	Allowlist []string
	Denylist  []string
}

func (p ToolPolicy) Validate() error {
	if len(p.Allowlist) > 256 || len(p.Denylist) > 256 {
		return fmt.Errorf("tool policy has too many names")
	}
	for _, list := range [][]string{p.Allowlist, p.Denylist} {
		seen := make(map[string]bool, len(list))
		for _, name := range list {
			if name == "" || name != strings.TrimSpace(name) || len(name) > 256 || strings.ContainsAny(name, "\x00\r\n") || seen[name] {
				return fmt.Errorf("tool policy contains an invalid or duplicate name")
			}
			seen[name] = true
		}
	}
	return nil
}

func (p ToolPolicy) Allows(name string) bool {
	for _, denied := range p.Denylist {
		if denied == name {
			return false
		}
	}
	if p.Allowlist == nil {
		return true
	}
	for _, allowed := range p.Allowlist {
		if allowed == name {
			return true
		}
	}
	return false
}
