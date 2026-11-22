package tools

import (
	"errors"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
	"github.com/omengye/deerflow-acp/go-harness/internal/toolidentity"
)

type bashInput struct {
	Description string `json:"description,omitempty"`
	Command     string `json:"command"`
	Timeout     int    `json:"timeout,omitempty"`
}

type bashSecretInput struct {
	Credential  *secrets.CredentialBinding `json:"credential,omitempty" jsonschema_description:"Service login scoped to this tool and origin. Supply service origin and username_env and/or key_env; never include actual values."`
	Description string                     `json:"description,omitempty"`
	Command     string                     `json:"command"`
	Timeout     int                        `json:"timeout,omitempty"`
	Secrets     map[string]string          `json:"secrets,omitempty" jsonschema_description:"Optional map from environment variable to vault secret name, for example {GITHUB_TOKEN: GITHUB_TOKEN}. A string secret becomes the variable value; a file secret becomes the path of a private file. Reference secrets only through these variables and never paste values into commands. Known values in output appear as [secret:NAME]."`
	SecretOut   bool                       `json:"secret_out,omitempty" jsonschema_description:"Set DEERFLOW_SECRET_OUT to a private directory. After the command ends, each file written there and named like a secret (for example $DEERFLOW_SECRET_OUT/API_TOKEN) is stored as that secret's new version, so refreshed tokens or generated keys are saved without printing them."`
}

func BashTool(backend harness.CommandBackend, provider harness.SandboxProvider, options ...CommandOption) (tool.InvokableTool, error) {
	o := applyCommandOptions(options)
	desc := "Run a foreground shell command in the workspace using the configured " + string(provider) + " provider. On Windows powershell, use PowerShell syntax. Requires host shell opt-in and session permission. Returns bounded stdout/stderr and exit code; background jobs are not supported."
	if backend == nil {
		return nil, errors.New("command backend is required")
	}
	var info *schema.ToolInfo
	var err error
	if o.vault != nil {
		info, err = utils.GoStruct2ToolInfo[bashSecretInput]("bash", desc)
	} else {
		info, err = utils.GoStruct2ToolInfo[bashInput]("bash", desc)
	}
	if err != nil {
		return nil, err
	}
	return &commandTool{Native: toolidentity.Builtin("bash"), info: info, backend: backend, vault: o.vault}, nil
}
