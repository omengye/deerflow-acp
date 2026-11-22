package secrets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"golang.org/x/net/idna"
)

// CredentialBinding contains public routing information only. Consumers select
// which variables they need; users enter the username and key in a dedicated client form.
type CredentialBinding struct {
	Service     string `json:"service" jsonschema:"required,description=HTTP(S) or SSH service origin without a path or credentials"`
	Account     string `json:"account,omitempty" jsonschema_description:"Stable nonsecret account label; defaults to default. Never put a username or key here."`
	UsernameEnv string `json:"username_env,omitempty" jsonschema_description:"Environment variable that receives the username."`
	KeyEnv      string `json:"key_env,omitempty" jsonschema_description:"Environment variable that receives the key or password."`
}

type CredentialScope harness.ServiceCredentialScope

var ErrCredentialCancelled = errors.New("credential entry cancelled; no credential was saved or used")
var accountLabel = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)
var errCredentialScope = errors.New("invalid credential scope; use a service origin and a nonsecret account label")

func ServiceOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 8192 || (u.Scheme != "http" && u.Scheme != "https") {
		return "", errCredentialScope
	}
	return normalizedServiceOrigin(u)
}

func normalizedServiceOrigin(u *url.URL) (string, error) {
	if u.User != nil || u.Hostname() == "" {
		return "", errCredentialScope
	}
	host := strings.ToLower(u.Hostname())
	if len(host) > 253 || strings.ContainsAny(host, "%{}") {
		return "", errCredentialScope
	}
	if net.ParseIP(host) == nil {
		asciiHost, err := idna.Lookup.ToASCII(host)
		if err != nil {
			return "", errCredentialScope
		}
		host = asciiHost
	}
	port := u.Port()
	if u.Scheme == "ssh" {
		if strings.HasSuffix(u.Host, ":") {
			return "", errCredentialScope
		}
		if port != "" {
			number, err := strconv.ParseUint(port, 10, 16)
			if err != nil || number == 0 {
				return "", errCredentialScope
			}
			port = strconv.FormatUint(number, 10)
		}
	}
	if port == "80" && u.Scheme == "http" || port == "443" && u.Scheme == "https" || port == "22" && u.Scheme == "ssh" {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return u.Scheme + "://" + host, nil
}

func NewCredentialScope(consumer, service, account string) (CredentialScope, error) {
	if strings.ContainsAny(consumer, "\r\n\x00") || len(consumer) > 160 || consumer == "" {
		return CredentialScope{}, errCredentialScope
	}
	if account == "" {
		account = "default"
	}
	if !accountLabel.MatchString(account) {
		return CredentialScope{}, errCredentialScope
	}
	u, err := url.Parse(service)
	if err != nil || len(service) > 8192 || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.EscapedPath() != "" && u.EscapedPath() != "/") {
		return CredentialScope{}, errCredentialScope
	}
	allowSSH := consumer == "tool:execute" || consumer == "tool:bash" || strings.HasPrefix(consumer, "skill:")
	if u.Scheme != "https" && u.Scheme != "http" && !(allowSSH && u.Scheme == "ssh") {
		return CredentialScope{}, errCredentialScope
	}
	origin, err := normalizedServiceOrigin(u)
	if err != nil {
		return CredentialScope{}, err
	}
	return CredentialScope{consumer, origin, account}, nil
}

const credentialPrefix = "DF_CREDENTIAL_"

func IsCredentialName(name string) bool { return strings.HasPrefix(name, credentialPrefix) }
func (s CredentialScope) Names() (string, string) {
	d := sha256.Sum256([]byte(s.Consumer + "\x00" + s.Service + "\x00" + s.Account))
	prefix := credentialPrefix + strings.ToUpper(hex.EncodeToString(d[:16]))
	return prefix + "_USER", prefix + "_KEY"
}

func (b *CredentialBinding) Bind(consumer string, extra map[string]string) (CredentialScope, map[string]string, error) {
	if b == nil {
		return CredentialScope{}, extra, nil
	}
	scope, err := NewCredentialScope(consumer, b.Service, b.Account)
	if err != nil {
		return scope, nil, err
	}
	if b.UsernameEnv == "" && b.KeyEnv == "" || strings.EqualFold(b.UsernameEnv, b.KeyEnv) {
		return scope, nil, errors.New("choose distinct username_env and/or key_env")
	}
	bindings := maps.Clone(extra)
	if bindings == nil {
		bindings = map[string]string{}
	}
	user, key := scope.Names()
	for env, name := range map[string]string{b.UsernameEnv: user, b.KeyEnv: key} {
		if env == "" {
			continue
		}
		for existing := range extra {
			if strings.EqualFold(env, existing) {
				return scope, nil, errors.New("credential environment variable already bound")
			}
		}
		bindings[env] = name
	}
	return scope, bindings, ValidateBindings(bindings)
}

type CredentialPair struct {
	Username []byte
	Key      []byte
}

func (p CredentialPair) Clear() { clear(p.Username); clear(p.Key) }

type CredentialPrompt func(context.Context, CredentialScope, CredentialFields) (CredentialPair, error)
type promptKey struct{}
type remoteCredentialClient struct{}
type credentialGrantKey struct{}
type credentialScopeKey struct{}
type credentialTargetKey struct{}

func WithCredentialPrompt(ctx context.Context, prompt CredentialPrompt) context.Context {
	return context.WithValue(ctx, promptKey{}, prompt)
}

// WithRemoteCredentialClient marks a connection which completes enrollment at
// approval time. It never authorizes a call by itself.
func WithRemoteCredentialClient(ctx context.Context) context.Context {
	return context.WithValue(ctx, remoteCredentialClient{}, true)
}

type remoteEnrollmentKey struct{}

// WithRemoteEnrollmentHandler binds secure enrollment to the owning client.
// The handler may only enroll fields of an already admitted tool request.
func WithRemoteEnrollmentHandler(ctx context.Context, enroll harness.PermissionHandler) context.Context {
	return context.WithValue(WithRemoteCredentialClient(ctx), remoteEnrollmentKey{}, enroll)
}

func RemoteEnrollmentHandler(ctx context.Context) harness.PermissionHandler {
	enroll, _ := ctx.Value(remoteEnrollmentKey{}).(harness.PermissionHandler)
	return enroll
}

type scopedCredentialGrant struct {
	scope  CredentialScope
	fields CredentialFields
}

// WithScopedCredentialGrant grants the dispatched call its requested fields
// for exactly one consumer, service and account.
func WithScopedCredentialGrant(ctx context.Context, scope CredentialScope, fields CredentialFields) context.Context {
	return context.WithValue(ctx, credentialGrantKey{}, scopedCredentialGrant{scope, fields})
}

// WithCredentialGrant is a host-provided grant for a directly invoked tool.
func WithCredentialGrant(ctx context.Context, target ...string) context.Context {
	if len(target) > 0 && target[0] != "" {
		ctx = context.WithValue(ctx, credentialTargetKey{}, target[0])
	}
	return context.WithValue(ctx, credentialGrantKey{}, true)
}
func permittedCredential(ctx context.Context, name string) bool {
	if !IsCredentialName(name) {
		return true
	}
	names, _ := ctx.Value(credentialScopeKey{}).([]string)
	return slices.Contains(names, name)
}

func (v *Vault) missingCredentialFields(ctx context.Context, scope CredentialScope, fields CredentialFields) (CredentialFields, error) {
	if fields == 0 || fields&^BothFields != 0 {
		return 0, errors.New("invalid credential fields")
	}
	missing, err := v.Missing(ctx, fields.Names(scope))
	if err != nil {
		return 0, err
	}
	u, k := scope.Names()
	var result CredentialFields
	if slices.Contains(missing, u) {
		result |= UsernameField
	}
	if slices.Contains(missing, k) {
		result |= KeyField
	}
	return result, nil
}

func (v *Vault) CredentialContext(ctx context.Context, scope CredentialScope, fields CredentialFields) (context.Context, error) {
	if err := v.usable(); err != nil {
		return ctx, err
	}
	prompt, _ := ctx.Value(promptKey{}).(CredentialPrompt)
	remote, _ := ctx.Value(remoteCredentialClient{}).(bool)
	grant := ctx.Value(credentialGrantKey{})
	granted, _ := grant.(bool)
	if scoped, ok := grant.(scopedCredentialGrant); ok {
		granted = scoped.scope == scope && fields != 0 && fields&^scoped.fields == 0
	}
	expected, _ := ctx.Value(credentialTargetKey{}).(string)
	if !granted || Disallowed(ctx) || (!remote && prompt == nil) || expected != "" && expected != scope.Service {
		return ctx, fmt.Errorf("%w: credential use requires an authorized tool and connected credential client", harness.ErrPermissionDenied)
	}
	allowed := context.WithValue(ctx, credentialScopeKey{}, fields.Names(scope))
	if missing, err := v.missingCredentialFields(ctx, scope, fields); missing == 0 || err != nil {
		return allowed, err
	}
	if remote {
		return ctx, errors.New("service credential enrollment is incomplete; request a fresh secure credential form in the connected client")
	}
	// Wait for at most one native form per profile. No vault or DB lock is
	// held while a user enters or cancels a credential.
	select {
	case v.credentialGate <- struct{}{}:
		defer func() { <-v.credentialGate }()
	case <-ctx.Done():
		return ctx, ctx.Err()
	}
	missing, err := v.missingCredentialFields(ctx, scope, fields)
	if missing == 0 || err != nil {
		return allowed, err
	}
	pair, err := prompt(ctx, scope, missing)
	defer pair.Clear()
	if err != nil {
		return ctx, err
	}
	if err = ctx.Err(); err != nil {
		return ctx, err
	}
	err = v.saveCredential(ctx, scope, missing, pair, "desktop_credential")
	return allowed, err
}

// A concurrent enrollment must preserve the account already saved.
var errCredentialExists = errors.New("credential already enrolled; retry the tool to use the saved account")

func (v *Vault) saveCredential(ctx context.Context, scope CredentialScope, fields CredentialFields, pair CredentialPair, source string) error {
	if err := v.usable(); err != nil {
		return err
	}
	if fields == 0 || fields&^BothFields != 0 {
		return errors.New("invalid credential fields")
	}
	user, key := scope.Names()
	v.writes.Lock()
	defer v.writes.Unlock()
	v.mu.Lock()
	defer v.mu.Unlock()
	tx, err := v.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	desc := scope.Consumer + " / " + scope.Service + " / " + scope.Account
	for _, item := range []struct {
		field CredentialFields
		name  string
		value []byte
	}{{UsernameField, user, pair.Username}, {KeyField, key, pair.Key}} {
		if fields&item.field == 0 {
			continue
		}
		// A separate process may have completed enrollment while the form was open.
		// Check availability in the same SQLite snapshot that creates the version.
		var available bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM harness_secrets s JOIN harness_secret_versions v
			  ON v.name=s.name AND v.version=s.current_version
			WHERE s.name=? AND s.deleted_at='' AND v.key_id=?
		)`, item.name, v.keyID).Scan(&available); err != nil {
			return err
		}
		if available {
			return errCredentialExists
		}
		req := SetRequest{Name: item.name, Kind: KindString, Value: item.value, Description: &desc, Source: source, Actor: scope.Consumer}
		if err = validateSetRequest(req); err != nil {
			return err
		}
		if err = v.writeVersion(ctx, tx, req, false); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return v.rebuild(context.WithoutCancel(ctx))
}
