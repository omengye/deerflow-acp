package secrets

import (
	"context"
	"fmt"
	"strings"
)

// MaskOutput masks complete tool output against every open vault, including
// values inside escaped JSON string literals.
func MaskOutput(text string) string {
	for _, m := range maskers() {
		text = m.MaskOutput(text)
	}
	return text
}

// ValueBinding binds a value sealed into tool arguments to one tool call and
// secret name.
func ValueBinding(callID, name string) string { return callID + "\x00" + name }

type vaultKey struct{}

// WithVault makes v the vault that restores markers for tools run with ctx.
func WithVault(ctx context.Context, v *Vault) context.Context {
	if v == nil {
		return ctx
	}
	return context.WithValue(ctx, vaultKey{}, v)
}

// FromContext returns the vault attached by WithVault, or nil.
func FromContext(ctx context.Context) *Vault {
	v, _ := ctx.Value(vaultKey{}).(*Vault)
	return v
}

type placeholderUsedKey struct{}

// WithPlaceholderUsed marks ctx for a tool dispatch whose arguments received
// substituted {{secret:NAME}} values.
func WithPlaceholderUsed(ctx context.Context) context.Context {
	return context.WithValue(ctx, placeholderUsedKey{}, true)
}

// HasPlaceholderUsed reports whether the active tool dispatch received
// substituted {{secret:NAME}} values.
func HasPlaceholderUsed(ctx context.Context) bool {
	used, _ := ctx.Value(placeholderUsedKey{}).(bool)
	return used
}

type disallowedKey struct{}

// Disallow marks ctx (such as delegated subagent runs) as prohibited from
// injecting, restoring or writing back vault secrets.
func Disallow(ctx context.Context) context.Context {
	return context.WithValue(ctx, disallowedKey{}, true)
}

// Disallowed reports whether ctx is barred from accessing vault secrets.
func Disallowed(ctx context.Context) bool {
	denied, _ := ctx.Value(disallowedKey{}).(bool)
	return denied
}

// Restore replaces reversible markers in text a tool is about to write with
// the exact text they stand for, using the vault attached to ctx. A truncated
// (:partial) marker of a known secret cannot be restored and is rejected.
func Restore(ctx context.Context, text string) (string, error) {
	v := FromContext(ctx)
	if v == nil || !strings.Contains(text, "[secret:") {
		return text, nil
	}
	return v.Masker().Restore(text)
}

// RestoreInFile restores reversible markers in text only when the underlying
// secret text already appeared in original. This preserves existing credentials
// during file edits while preventing workspace writes from materializing
// unrelated vault secrets into new or arbitrary files.
func RestoreInFile(ctx context.Context, text, original string) (string, error) {
	v := FromContext(ctx)
	if v == nil || !strings.Contains(text, "[secret:") {
		return text, nil
	}
	return v.Masker().RestoreInFile(text, original)
}

// Restore is the Masker form of the package-level Restore.
func (m *Masker) Restore(s string) (string, error) {
	if m == nil || !strings.Contains(s, "[secret:") {
		return s, nil
	}
	if err := m.rejectPartialMarkers(s); err != nil {
		return "", err
	}
	out, _ := m.Unmask(s)
	return out, nil
}

// RestoreInFile is the Masker form of the package-level RestoreInFile.
func (m *Masker) RestoreInFile(s, original string) (string, error) {
	if m == nil || !strings.Contains(s, "[secret:") {
		return s, nil
	}
	if err := m.rejectPartialMarkers(s); err != nil {
		return "", err
	}
	if original == "" {
		return s, nil
	}
	out := markerPattern.ReplaceAllStringFunc(s, func(marker string) string {
		if i, ok := m.longRestore[marker]; ok {
			p := &m.list[i]
			for pos := 0; pos+p.size <= len(original); {
				n := strings.Index(original[pos:], p.text)
				if n < 0 {
					break
				}
				pos += n
				if pos+p.size <= len(original) && p.matchesPrefix(original[pos:pos+p.size]) {
					return original[pos : pos+p.size]
				}
				pos++
			}
		}
		if value, ok := m.restore[marker]; ok && len(value) >= MinMaskBytes && strings.Contains(original, value) {
			return value
		}
		return marker
	})
	return out, nil
}

func (m *Masker) rejectPartialMarkers(s string) error {
	if !strings.Contains(s, ":partial]") {
		return nil
	}
	for _, match := range markerPattern.FindAllStringSubmatch(s, -1) {
		if match[3] != "partial" {
			continue
		}
		key := strings.TrimSuffix(match[0], ":partial]") + "]"
		_, small := m.restore[key]
		_, large := m.longRestore[key]
		if small || large {
			return fmt.Errorf("%w: %s stands for a secret value that an output limit cut off, so it cannot be restored; read the complete text again before writing it", ErrInvalid, match[0])
		}
	}
	return nil
}
