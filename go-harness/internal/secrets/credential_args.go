package secrets

import (
	"context"
	"errors"
	"strings"
)

// Substitute only in decoded JSON values. The caller pins the actual MCP
// endpoint and tool, and only these two fields may be requested.
const (
	credentialMarker = "{{credential:"
	userMarker       = "{{credential:username}}"
	keyMarker        = "{{credential:key}}"
)

func markerFields(args string) (CredentialFields, error) {
	var fields CredentialFields
	check := strings.NewReplacer(userMarker, "", keyMarker, "")
	_, err := rewriteMarkedStrings(args, credentialMarker, true, func(s string) (string, error) {
		if strings.Contains(check.Replace(s), credentialMarker) {
			return "", errors.New("invalid service credential marker")
		}
		if strings.Contains(s, userMarker) {
			fields |= UsernameField
		}
		if strings.Contains(s, keyMarker) {
			fields |= KeyField
		}
		return s, nil
	})
	if fields == 0 && err == nil {
		return 0, errors.New("missing service credential marker")
	}
	return fields, err
}

func CredentialArguments(ctx context.Context, scope CredentialScope, args string) (string, *CredentialOutput, error) {
	fields, err := markerFields(args)
	if err != nil {
		return "", nil, err
	}
	v := FromContext(ctx)
	ctx, err = v.CredentialContext(ctx, scope, fields)
	if err != nil {
		return "", nil, err
	}
	u, k := scope.Names()
	values, err := v.Resolve(ctx, fields.Names(scope))
	if err != nil {
		return "", nil, err
	}
	defer clear(values[u].Value)
	defer clear(values[k].Value)
	replace := strings.NewReplacer(userMarker, string(values[u].Value), keyMarker, string(values[k].Value))
	size := len(args)
	out, err := rewriteMarkedStrings(args, credentialMarker, true, func(s string) (string, error) {
		size += strings.Count(s, userMarker)*(len(values[u].Value)-len(userMarker)) + strings.Count(s, keyMarker)*(len(values[k].Value)-len(keyMarker))
		if size > MaxSubstitutedBytes {
			return "", errors.New("credential arguments exceed size limit")
		}
		return replace.Replace(s), nil
	})
	if len(out) > MaxSubstitutedBytes {
		return "", nil, errors.New("encoded credential arguments exceed size limit")
	}
	return out, NewCredentialOutput(map[string]string{u: string(values[u].Value), k: string(values[k].Value)}), err
}
