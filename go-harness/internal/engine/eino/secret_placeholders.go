package eino

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
)

// {{secret:NAME}} placeholders let a tool receive a vault value without the
// value entering the conversation. A call keeps the placeholder in history,
// approval prompts, events and receipts; the value is substituted only into
// the arguments handed to the tool, after approval.

type placeholderUse uint8

const (
	placeholdersLiteral placeholderUse = iota
	placeholdersSubstituted
	placeholdersRejected
)

func (m *toolMiddleware) placeholderUse(name string) placeholderUse {
	if m.vault == nil || m.disallowSecrets {
		return placeholdersLiteral
	}
	switch name {
	case "execute", "bash", "skill_run", "host_opencli":
		// A value in a command line would be visible to other processes.
		// Reject by name before checking trust so a missing builtin marker
		// can never silently substitute plaintext into commands.
		return placeholdersRejected
	case "task", "skill", "write_todos", "invoke_acp_agent", "present_files":
		// Delegation, artifacts and engine-reserved tools must never receive plaintext.
		return placeholdersLiteral
	}
	if !m.trusted[name] {
		// External tools, such as MCP tools, have no other way to receive a
		// named secret. Such unbound references require explicit approval.
		return placeholdersSubstituted
	}
	if name == "web_fetch" {
		return placeholdersSubstituted
	}
	// Built-in tools keep placeholders as text: delegation, memory, schedules,
	// skills, questions and workspace files must never receive plaintext.
	return placeholdersLiteral
}

func placeholderCommandHint(name string) string {
	switch name {
	case "skill_run":
		return `pass secrets={"ENV_VAR":"NAME"} or declare the secret in the skill's skill-runtime.json, and read it from the environment in the script.`
	case "host_opencli":
		return "this tool cannot receive secrets."
	}
	return `pass secrets={"ENV_VAR":"NAME"} and refer to $ENV_VAR in the command instead.`
}

// Determine the service before generic placeholder substitution. Dispatch
// must match the same service even when an MCP argument itself is a URL.
func (m *toolMiddleware) desktopCredentialTarget(name, args string) (string, error) {
	if lookup := m.credentialScopes[name]; lookup != nil && harness.HasCredentialMarker(json.RawMessage(args)) {
		scope, err := lookup(args)
		return scope.Service, err
	}
	if name == "web_fetch" && m.trusted[name] {
		var in struct{ URL, Credential string }
		if err := json.Unmarshal([]byte(args), &in); err != nil {
			return "", err
		}
		if in.Credential != "" {
			if in.Credential != "basic" && in.Credential != "bearer" {
				return "", errors.New("invalid desktop authentication method")
			}
			return secrets.ServiceOrigin(in.URL)
		}
	}
	return "", nil
}

// credentialScopeForApproval derives enrollment metadata solely from the tool selected
// by the host. skill_run arguments here already contain its host-pinned secret plan.
func (m *toolMiddleware) credentialScopeForApproval(name string, args json.RawMessage) (*harness.ServiceCredentialScope, error) {
	if m.vault == nil || m.disallowSecrets || m.readOnly {
		return nil, nil
	}
	var scope secrets.CredentialScope
	var err error
	if !m.trusted[name] {
		lookup := m.credentialScopes[name]
		if lookup == nil || !harness.HasCredentialMarker(args) {
			return nil, nil
		}
		scope, err = lookup(string(args))
	} else {
		switch name {
		case "execute", "bash":
			var in struct {
				Credential *secrets.CredentialBinding
				Secrets    map[string]string
			}
			if json.Unmarshal(args, &in) != nil || in.Credential == nil {
				return nil, nil
			}
			scope, _, err = in.Credential.Bind("tool:"+name, in.Secrets)
		case "web_fetch":
			var in struct{ Credential, Account string }
			if err := json.Unmarshal(args, &in); err != nil {
				return nil, errors.New("invalid web_fetch credential arguments")
			}
			if in.Credential == "" {
				return nil, nil
			}
			target, targetErr := m.desktopCredentialTarget(name, string(args))
			if targetErr != nil {
				return nil, targetErr
			}
			scope, err = secrets.NewCredentialScope("tool:web_fetch", target, in.Account)
		case "skill_run":
			var in struct {
				Plan struct {
					Credential *secrets.CredentialScope `json:"credential_scope"`
				} `json:"secret_plan"`
			}
			if json.Unmarshal(args, &in) != nil || in.Plan.Credential == nil {
				return nil, nil
			}
			scope = *in.Plan.Credential
		default:
			return nil, nil
		}
	}
	if err != nil {
		return nil, err
	}
	result := harness.ServiceCredentialScope(scope)
	return &result, nil
}

// secretPlaceholders checks the placeholders of a call before it is approved
// or recorded as started. It returns the names to resolve at dispatch, or a
// failure certified as not executed.
func (m *toolMiddleware) secretPlaceholders(ctx context.Context, tc *adk.ToolContext, args string) ([]string, error) {
	if m.policy != nil && !m.policy.Allows(tc.Name) {
		return nil, nil
	}
	if secrets.Disallowed(ctx) {
		return nil, nil
	}
	if m.readOnly {
		return nil, nil
	}
	raw := json.RawMessage(args)
	scope, err := m.credentialScopeForApproval(tc.Name, raw)
	if err != nil {
		return nil, harness.MarkToolNotExecuted(err)
	}
	if scope != nil {
		if _, err := secrets.ApprovalFields(harness.PermissionRequest{ToolName: tc.Name, TrustedBuiltin: m.trusted[tc.Name], Arguments: raw}); err != nil {
			return nil, harness.MarkToolNotExecuted(err)
		}
	}
	use := m.placeholderUse(tc.Name)
	// Without a backslash, every decoded string is a substring of args.
	if use == placeholdersLiteral || !strings.Contains(args, "{{secret:") && !strings.Contains(args, `\`) || !json.Valid([]byte(args)) {
		return nil, nil
	}
	names, err := secrets.PlaceholderNames(args)
	if err != nil {
		return nil, harness.MarkToolNotExecuted(err)
	}
	if len(names) == 0 {
		return nil, nil
	}
	if use == placeholdersRejected {
		return nil, harness.MarkToolNotExecuted(fmt.Errorf("%s does not substitute {{secret:NAME}} placeholders, so nothing was run; %s", tc.Name, placeholderCommandHint(tc.Name)))
	}
	// A resumed call passed this check before it was suspended. Dispatch
	// resolves its secrets again, and a secret deleted meanwhile fails there.
	if resumed, _, _ := tool.GetInterruptState[permissionState](ctx); !resumed {
		missing, err := m.vault.Missing(ctx, names)
		if err != nil {
			return nil, harness.MarkToolNotExecuted(err)
		}
		if len(missing) > 0 {
			return nil, harness.MarkToolNotExecuted(&secrets.MissingError{Names: missing})
		}
	}
	return names, nil
}

// rejectPlaceholders records a call refused by secretPlaceholders and reports
// whether the model should see the failure instead of the run ending.
func (m *toolMiddleware) rejectPlaceholders(ctx context.Context, tc *adk.ToolContext, args string, failure error) (bool, error) {
	if err := m.sink.emit(ctx, harness.RunEvent{Kind: "tool_start", ToolCallID: tc.CallID, ToolName: tc.Name, Status: "pending", Arguments: json.RawMessage(args)}); err != nil {
		return false, err
	}
	recoverable := m.recoverableFailure(ctx, tc, failure)
	if err := m.finish(ctx, tc, nil, failure, recoverable); err != nil {
		return false, errors.Join(failure, err)
	}
	if !recoverable {
		return false, failure
	}
	return true, nil
}

// dispatchArguments returns the arguments to hand to the tool. Secrets are
// resolved at dispatch so that the call uses their current values. The
// result may hold plaintext and must not be recorded.
func (m *toolMiddleware) dispatchArguments(ctx context.Context, args string, names []string) (string, error) {
	if len(names) == 0 {
		return args, nil
	}
	values, err := m.vault.Resolve(ctx, names)
	if err != nil {
		return "", harness.MarkToolNotExecuted(err)
	}
	defer func() {
		for _, secret := range values {
			clear(secret.Value)
		}
	}()
	out, err := secrets.SubstitutePlaceholders(args, values)
	if err != nil {
		return "", harness.MarkToolNotExecuted(err)
	}
	return out, nil
}

func withArgumentText(args *schema.ToolArgument, text string) *schema.ToolArgument {
	if text == args.Text {
		return args
	}
	copy := *args
	copy.Text = text
	return &copy
}
