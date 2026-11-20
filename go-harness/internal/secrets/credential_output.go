package secrets

import (
	"context"
	"maps"
	"slices"
	"sort"
	"strings"
)

// CredentialOutput masks short account names and authentication headers in
// the output of the one call that received them. One-character account names
// do not change the permanent masking rules for other sessions or inputs.
type CredentialOutput struct{ replace *strings.Replacer }

func NewCredentialOutput(fields map[string]string) *CredentialOutput {
	replacements := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		value := fields[name]
		if value == "" {
			continue
		}
		for _, v := range variants([]byte(value)) {
			replacements[v.text] = "[secret:" + name + v.suffix + "]"
		}
	}
	if len(replacements) == 0 {
		return nil
	}
	keys := make([]string, 0, len(replacements))
	for k := range replacements {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) == len(keys[j]) {
			return keys[i] < keys[j]
		}
		return len(keys[i]) > len(keys[j])
	})
	pairs := make([]string, 0, len(keys)*2)
	for _, k := range keys {
		pairs = append(pairs, k, replacements[k])
	}
	return &CredentialOutput{strings.NewReplacer(pairs...)}
}

func (m *CredentialOutput) Mask(text string) string {
	if m == nil {
		return text
	}
	return m.replace.Replace(text)
}

type credentialOutputKey struct{}

func WithCredentialOutput(ctx context.Context, m *CredentialOutput) context.Context {
	if m == nil {
		return ctx
	}
	return context.WithValue(ctx, credentialOutputKey{}, m)
}
func CallCredentialOutput(ctx context.Context) *CredentialOutput {
	m, _ := ctx.Value(credentialOutputKey{}).(*CredentialOutput)
	return m
}
