package tools

import (
	"context"
	"errors"
	"fmt"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
)

// CommandOption configures optional command tool capabilities.
type CommandOption func(*commandOptions)

type commandOptions struct{ vault *secrets.Vault }

// WithSecretVault lets commands reference vault secrets by name.
func WithSecretVault(v *secrets.Vault) CommandOption {
	return func(o *commandOptions) { o.vault = v }
}

func applyCommandOptions(options []CommandOption) commandOptions {
	var o commandOptions
	for _, option := range options {
		if option != nil {
			option(&o)
		}
	}
	return o
}

// restoreSecretMarkersInFile turns [secret:NAME] markers back into plaintext
// only when the target file's existing content already contained that exact
// value. New files and unrelated secrets are never materialized from the vault.
func restoreSecretMarkersInFile(ctx context.Context, text, original string) (string, error) {
	if secrets.Disallowed(ctx) {
		return text, nil
	}
	restored, err := secrets.RestoreInFile(ctx, text, original)
	if err != nil {
		return "", harness.MarkToolNotExecuted(err)
	}
	return restored, nil
}

// SecretReport tells the model which secret versions a command received and
// which secrets it wrote back. It never contains values.
type SecretReport struct {
	Used    []secrets.Use    `json:"used,omitempty"`
	Updated []secrets.Update `json:"updated,omitempty"`
	Error   string           `json:"error,omitempty"`
}

// SecretRequest binds vault secrets to one command.
type SecretRequest struct {
	Credential *secrets.CredentialBinding
	Vault      *secrets.Vault
	Bindings   map[string]string // environment variable -> secret name
	WriteBack  bool
	Actor      string
	// Writable limits write-back to these names; nil accepts any valid name.
	Writable func(name string) bool
}

func (s SecretRequest) empty() bool {
	return len(s.Bindings) == 0 && !s.WriteBack && s.Credential == nil
}

// ExecuteWithSecrets materializes bound secrets for exactly one command,
// removes the material after termination and ingests write-back files once
// termination is confirmed, whatever the exit code.
func ExecuteWithSecrets(ctx context.Context, backend harness.CommandBackend, request harness.CommandRequest, s SecretRequest) (harness.CommandSnapshot, *SecretReport, error) {
	if s.empty() {
		result, err := ExecuteCommand(ctx, backend, request)
		return result, nil, err
	}
	if secrets.Disallowed(ctx) {
		return harness.CommandSnapshot{}, nil, harness.MarkToolNotExecuted(fmt.Errorf("%w: only the foreground agent may inject or write back secrets", harness.ErrPermissionDenied))
	}
	if s.Vault == nil {
		return harness.CommandSnapshot{}, nil, harness.MarkToolNotExecuted(errors.New("the secret vault is not available in this session"))
	}
	if s.Credential != nil {
		scope, bindings, err := s.Credential.Bind("tool:"+s.Actor, s.Bindings)
		if err != nil {
			return harness.CommandSnapshot{}, nil, harness.MarkToolNotExecuted(err)
		}
		ctx, err = s.Vault.CredentialContext(ctx, scope, s.Credential.Fields())
		if err != nil {
			return harness.CommandSnapshot{}, nil, harness.MarkToolNotExecuted(err)
		}
		s.Bindings = bindings
	}
	injection, err := s.Vault.Prepare(ctx, s.Bindings, s.WriteBack)
	if err != nil {
		return harness.CommandSnapshot{}, nil, harness.MarkToolNotExecuted(err)
	}
	defer injection.Cleanup()
	fields := map[string]string{}
	for _, used := range injection.Used {
		if secrets.IsCredentialName(used.Name) {
			fields[used.Name] = injection.Env[used.Env]
		}
	}
	ctx = secrets.WithCredentialOutput(ctx, secrets.NewCredentialOutput(fields))
	request.Secrets = injection.Command()
	result, runErr := ExecuteCommand(ctx, backend, request)
	report := &SecretReport{Used: injection.Used}
	if s.WriteBack {
		if result.TerminationConfirmed && result.State != harness.CommandUncertain {
			updates, collectErr := injection.Collect(context.WithoutCancel(ctx), s.Vault, s.Actor, s.Writable)
			report.Updated = updates
			if collectErr != nil {
				report.Error = "secret write-back failed: " + collectErr.Error()
			}
		} else {
			report.Error = "secret write-back skipped because command termination was not confirmed"
		}
	}
	return result, report, runErr
}

// commandOutput keeps the historical snapshot shape and adds secret usage only
// when a command used the vault.
type commandOutput struct {
	harness.CommandSnapshot
	Secrets *SecretReport `json:"secrets,omitempty"`
}
