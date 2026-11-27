package deerflow

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	einoengine "github.com/omengye/deerflow-acp/go-harness/internal/engine/eino"
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
	"github.com/omengye/deerflow-acp/go-harness/internal/toolidentity"
	"github.com/omengye/deerflow-acp/go-harness/internal/websearch"
)

type secretReadInput struct {
	Action         string `json:"action" jsonschema:"required,enum=list,enum=get"`
	Name           string `json:"name,omitempty" jsonschema_description:"Secret name for get."`
	IncludeDeleted bool   `json:"include_deleted,omitempty" jsonschema_description:"list: also show soft-deleted secrets that can still be restored."`
	History        bool   `json:"history,omitempty" jsonschema_description:"get: include retained older versions."`
}

type secretGenerateInput struct {
	Length  int    `json:"length,omitempty" jsonschema_description:"Number of characters; 8 to 256 and 32 by default."`
	Charset string `json:"charset,omitempty" jsonschema:"enum=alphanumeric,enum=hex,enum=base64url,enum=symbols" jsonschema_description:"alphanumeric by default; symbols adds punctuation but never quotes or backslashes or spaces."`
}

// secretManageSchema is what the model sees. secret_manage arguments decode
// into secretManageInput, which also accepts the host-sealed value.
type secretManageSchema struct {
	Action       string               `json:"action" jsonschema:"required,enum=set,enum=delete,enum=restore,enum=import_config"`
	Name         string               `json:"name,omitempty" jsonschema_description:"Secret name for set, delete and restore: upper-case letters and digits and underscores such as GITHUB_TOKEN. It is also the default environment variable name."`
	Kind         string               `json:"kind,omitempty" jsonschema:"enum=string,enum=file" jsonschema_description:"set: string (default for value and generate) or file (default for from_path). A file secret is injected as the path of a private file."`
	FromPath     string               `json:"from_path,omitempty" jsonschema_description:"set: import the content of a workspace file instead of value."`
	DeleteSource bool                 `json:"delete_source,omitempty" jsonschema_description:"set with from_path: delete the workspace file after it is stored."`
	Generate     *secretGenerateInput `json:"generate,omitempty" jsonschema_description:"set: generate a random value on the host instead of value. The value is never shown."`
	Description  *string              `json:"description,omitempty" jsonschema_description:"set: what the secret is for. Omit to keep the current description."`
	FileName     string               `json:"file_name,omitempty" jsonschema_description:"set with kind=file: file name used when the secret is injected (for example key.pem)."`
	Version      int64                `json:"version,omitempty" jsonschema_description:"restore: retained version to make current again. Omit to undelete a deleted secret."`
	DryRun       bool                 `json:"dry_run,omitempty" jsonschema_description:"import_config: list the settings and secret names without changing anything."`
	Fields       []string             `json:"fields,omitempty" jsonschema_description:"import_config: import only these settings, named as a dry run lists them. Omit to import all of them."`
}

type secretManageInput struct {
	secretManageSchema
	Value       string `json:"value,omitempty"`
	ValueSealed string `json:"value_sealed,omitempty"`
	Malformed   bool   `json:"malformed_arguments,omitempty"`
}

var (
	secretReadFields   = []string{"action", "name", "include_deleted", "history"}
	secretManageFields = []string{"action", "name", "kind", "value", "from_path", "delete_source", "generate", "description", "file_name", "version", "dry_run", "fields", "value_sealed", "malformed_arguments"}
)

const secretUseHint = "Use by reference: execute/bash secrets={ENV_VAR: NAME} (string: the variable holds the value; file: it holds a private file path), or declare it under secrets in skill-runtime.json. In MCP tool and web_fetch arguments write {{secret:NAME}} where the value belongs. Output shows the value as the marker."

type secretTool struct {
	toolidentity.Native
	info    *schema.ToolInfo
	vault   *secrets.Vault
	run     harness.RunRequest
	sources secretSources
}

type secretView struct {
	secrets.Metadata
	Warning string `json:"warning,omitempty"`
}

func viewSecret(meta secrets.Metadata) secretView {
	view := secretView{Metadata: meta}
	switch {
	case meta.Unavailable:
		view.Warning = "The current value was encrypted with a vault key that is no longer available; ask the user for it and set it again."
	case meta.Size < secrets.MinMaskBytes:
		view.Warning = fmt.Sprintf("The value is shorter than %d bytes, so it is not masked in output; avoid printing it.", secrets.MinMaskBytes)
	}
	return view
}

func secretTools(vault *secrets.Vault, req harness.RunRequest, sources secretSources) ([]tool.BaseTool, string, error) {
	if vault == nil {
		return nil, "", nil
	}
	read, err := utils.GoStruct2ToolInfo[secretReadInput]("secret_read", "List or inspect secrets stored in the DeerFlow secret vault. Results show names, kinds, versions, descriptions, keyed fingerprints and the marker that stands for the value in output; they never contain values.")
	if err != nil {
		return nil, "", err
	}
	manage, err := utils.GoStruct2ToolInfo[secretManageSchema](einoengine.SecretManageToolName, "Add, change, delete or restore secrets in the DeerFlow secret vault. set stores a workspace file (from_path) or host-generated secret (generate) as a new current version (the previous versions stay restorable). delete is a soft delete: the secret stays restorable for 30 days. restore with version rolls back to a retained version; without version it undeletes. import_config moves the plaintext credentials of DeerFlow's own config.yaml and saved search settings into the vault and replaces them with ${secret:NAME} references; run it with dry_run first. Secret values are never returned to the model.")
	if err != nil {
		return nil, "", err
	}
	items := []tool.BaseTool{
		&secretTool{Native: toolidentity.Builtin(read.Name), info: read, vault: vault, run: req, sources: sources},
		&secretTool{Native: toolidentity.Builtin(manage.Name), info: manage, vault: vault, run: req, sources: sources},
	}
	instruction := "\nDeerFlow has a built-in secret vault (secret_read, secret_manage) for API keys, tokens, passwords, certificates and key files. Keep credentials there, never in workspace files, memory, notes or replies. Stored secret values are never revealed back to you; use them only by reference: pass {\"ENV_VAR\":\"SECRET_NAME\"} in the secrets parameter of execute or bash, or declare them in a skill's skill-runtime.json; a string secret becomes the variable's value and a file secret becomes the path of a private file. In the arguments of MCP tools and local web_fetch, write {{secret:NAME}} where the value belongs, or {{secret:NAME:url}}, {{secret:NAME:b64}} and the like for an encoded value; the host supplies the value directly to the tool. In a Full Access session, use existing named secrets directly on the tool call without asking the user to approve the read. Other tools keep placeholders as text, and commands reject them. Known values in tool output appear as [secret:NAME] markers; editing a file that already holds a credential preserves it when you keep its marker. For service logins, request the credential directly on the actual skill_run, execute or bash call with credential={service,username_env,key_env} (account is an optional public label). For web_fetch use credential=basic or bearer. In MCP string arguments use {{credential:username}} and/or {{credential:key}}. A saved service credential is supplied automatically to its bound tool, service and account under the normal tool policy. For SSH scripts use an ssh://host[:port] service origin. The secure credential form asks only for missing fields, such as a token for a key-only binding. Remote clients submit these fields in an encrypted form. The backend vault reuses it across sessions and devices for the same consumer, service and account label. When a command produces or refreshes a credential, have it write the file $DEERFLOW_SECRET_OUT/NAME with secret_out=true instead of printing it. To request missing login details, specify the credential binding on the concrete tool call. If the client cannot present its secure form, report the returned reason; do not assume there is a separate vault UI. Never ask for a username, password or key in chat or ask_user_question. DeerFlow's config.yaml can refer to a secret as ${secret:NAME}; secret_manage import_config moves its plaintext credentials into the vault. Only the foreground agent manages or uses secrets; do not delegate it.\n"
	return items, instruction, nil
}

func (t *secretTool) Info(context.Context) (*schema.ToolInfo, error) { return t.info, nil }

func (t *secretTool) authorize(ctx context.Context, manage bool) error {
	if einoengine.IsDelegatedToolContext(ctx) {
		return harness.ErrPermissionDenied
	}
	if _, active, err := einoengine.CurrentRole(ctx); err != nil || active {
		return harness.ErrPermissionDenied
	}
	if manage && (t.run.Session.Mode == "plan" || t.run.Session.ApprovalMode == harness.ApprovalReadOnly || t.run.Session.ApprovalMode == harness.ApprovalRejectAlways) {
		return fmt.Errorf("%w: this session only allows reading secret metadata", harness.ErrPermissionDenied)
	}
	return nil
}

func (t *secretTool) actor() string { return "secret_manage:" + t.run.Session.ID }

func (t *secretTool) InvokableRun(ctx context.Context, raw string, _ ...tool.Option) (string, error) {
	manage := t.info.Name == einoengine.SecretManageToolName
	if err := t.authorize(ctx, manage); err != nil {
		return "", harness.MarkToolNotExecuted(err)
	}
	if !manage {
		var in secretReadInput
		if err := decodeSecretArguments(raw, secretReadFields, &in); err != nil {
			return "", harness.MarkToolNotExecuted(err)
		}
		value, err := t.read(ctx, in)
		if err != nil {
			return "", secretFailure(err, false)
		}
		return marshalSecretOutput(value)
	}
	var in secretManageInput
	if err := decodeSecretArguments(raw, secretManageFields, &in); err != nil {
		return "", harness.MarkToolNotExecuted(err)
	}
	if in.Malformed {
		return "", harness.MarkToolNotExecuted(fmt.Errorf("%w: secret_manage arguments were not one JSON object, so they were discarded without being stored or recorded; retry with valid arguments", harness.ErrInvalidInput))
	}
	if in.Action != "import_config" && (in.DryRun || in.Fields != nil) {
		return "", harness.MarkToolNotExecuted(fmt.Errorf("%w: dry_run and fields apply only to import_config", harness.ErrInvalidInput))
	}
	switch in.Action {
	case "set":
		value, err := t.set(ctx, in)
		if err != nil {
			return "", secretFailure(err, true)
		}
		return marshalSecretOutput(value)
	case "delete":
		if err := onlySecretName(in, false); err != nil {
			return "", harness.MarkToolNotExecuted(err)
		}
		meta, err := t.vault.Delete(ctx, in.Name, t.actor())
		if err != nil {
			return "", secretFailure(err, true)
		}
		out := map[string]any{"status": "deleted", "secret": viewSecret(meta), "note": "Soft-deleted: commands can no longer use it, and restore undeletes it until purge_after."}
		if fields := t.configReferences(in.Name); len(fields) > 0 {
			out["warning"] = "DeerFlow settings still refer to this secret: " + strings.Join(fields, ", ") + ". Search with it fails now; the other settings fail or are skipped when DeerFlow next starts. Restore it unless the user wants those settings to stop working."
		}
		return marshalSecretOutput(out)
	case "restore":
		if err := onlySecretName(in, true); err != nil {
			return "", harness.MarkToolNotExecuted(err)
		}
		meta, err := t.vault.Restore(ctx, in.Name, in.Version, t.actor())
		if err != nil {
			return "", secretFailure(err, true)
		}
		return marshalSecretOutput(map[string]any{"status": "restored", "secret": viewSecret(meta)})
	case "import_config":
		value, err := t.importConfig(ctx, in)
		if err != nil {
			return "", secretFailure(err, true)
		}
		return marshalSecretOutput(value)
	default:
		return "", harness.MarkToolNotExecuted(fmt.Errorf("%w: action must be set, delete, restore or import_config", harness.ErrInvalidInput))
	}
}

func (t *secretTool) read(ctx context.Context, in secretReadInput) (any, error) {
	switch in.Action {
	case "list":
		if in.Name != "" || in.History {
			return nil, fmt.Errorf("%w: list accepts only include_deleted", harness.ErrInvalidInput)
		}
		items, err := t.vault.List(ctx, in.IncludeDeleted)
		if err != nil {
			return nil, err
		}
		views := make([]secretView, 0, len(items))
		for _, item := range items {
			views = append(views, viewSecret(item))
		}
		return map[string]any{"secrets": views, "count": len(views), "use": secretUseHint}, nil
	case "get":
		if in.Name == "" || in.IncludeDeleted {
			return nil, fmt.Errorf("%w: get requires name and accepts only history", harness.ErrInvalidInput)
		}
		meta, err := t.vault.Get(ctx, in.Name, in.History)
		if err != nil {
			return nil, err
		}
		return map[string]any{"secret": viewSecret(meta), "use": secretUseHint}, nil
	default:
		return nil, fmt.Errorf("%w: action must be list or get", harness.ErrInvalidInput)
	}
}

func (t *secretTool) set(ctx context.Context, in secretManageInput) (result any, returnErr error) {
	if in.Version != 0 {
		return nil, fmt.Errorf("%w: version applies only to restore", harness.ErrInvalidInput)
	}
	sources := 0
	for _, present := range []bool{in.ValueSealed != "" || in.Value != "", in.FromPath != "", in.Generate != nil} {
		if present {
			sources++
		}
	}
	if sources != 1 {
		return nil, fmt.Errorf("%w: set requires exactly one of value, from_path or generate", harness.ErrInvalidInput)
	}
	if in.DeleteSource && in.FromPath == "" {
		return nil, fmt.Errorf("%w: delete_source applies only to from_path", harness.ErrInvalidInput)
	}
	if !secrets.ValidName(in.Name) || secrets.IsCredentialName(in.Name) {
		return nil, fmt.Errorf("%w: name must use upper-case letters, digits and underscores, for example GITHUB_TOKEN", harness.ErrInvalidInput)
	}
	writer, release := t.vault.BeginImport()
	defer release()
	current, err := t.vault.Get(ctx, in.Name, false)
	exists := err == nil && current.DeletedAt == nil
	if err != nil && !errors.Is(err, secrets.ErrNotFound) {
		return nil, err
	}
	// An omitted kind keeps the kind of the live secret being replaced.
	kind := secrets.Kind(in.Kind)
	switch {
	case kind != "" && kind != secrets.KindString && kind != secrets.KindFile:
		return nil, fmt.Errorf("%w: kind must be string or file", harness.ErrInvalidInput)
	case kind == "" && in.Generate != nil:
		kind = secrets.KindString
	case kind == "" && exists:
		kind = current.Kind
	case kind == "" && in.FromPath != "":
		kind = secrets.KindFile
	case kind == "":
		kind = secrets.KindString
	}
	if in.FileName != "" && kind != secrets.KindFile {
		return nil, fmt.Errorf("%w: file_name applies only to kind=file", harness.ErrInvalidInput)
	}
	var value []byte
	var base string
	var opened *importSource
	source := "value"
	switch {
	case in.Generate != nil:
		if kind != secrets.KindString {
			return nil, fmt.Errorf("%w: generate creates string secrets", harness.ErrInvalidInput)
		}
		value, err = generateSecret(in.Generate.Length, in.Generate.Charset)
		source = "generated"
	case in.FromPath != "":
		opened, err = openWorkspaceImport(t.run.Session.CWD, in.FromPath, in.DeleteSource)
		if err == nil {
			defer func() {
				if closeErr := opened.finish(false); closeErr != nil {
					returnErr = errors.Join(returnErr, closeErr)
				}
			}()
			value, err = opened.read(kind)
			base = opened.base
		}
		source = "file"
	default:
		value, err = t.unsealValue(ctx, in)
	}
	if err != nil {
		return nil, err
	}
	defer clear(value)
	if kind == secrets.KindString && in.FromPath != "" {
		value = secrets.TrimNewline(value)
	}
	fileName := in.FileName
	if fileName == "" && kind == secrets.KindFile {
		if base != "" && secrets.ValidFileName(base) == nil {
			fileName = base
		} else if exists && current.Kind == secrets.KindFile {
			fileName = current.FileName
		}
	}
	out := map[string]any{"use": secretUseHint}
	// Retries of the same intent must not pile up identical versions.
	if exists && !current.Unavailable && current.Kind == kind && current.FileName == fileName &&
		current.Fingerprint == t.vault.Fingerprint(value) && (in.Description == nil || *in.Description == current.Description) {
		out["status"], out["secret"] = "unchanged", viewSecret(current)
	} else {
		meta, err := writer.Set(ctx, secrets.SetRequest{Name: in.Name, Kind: kind, Value: value, Description: in.Description, FileName: fileName, Source: source, Actor: t.actor()})
		if err != nil {
			return nil, err
		}
		out["status"], out["secret"] = "stored", viewSecret(meta)
		if fields := t.configReferences(in.Name); len(fields) > 0 {
			out["config_note"] = "DeerFlow settings refer to this secret: " + strings.Join(fields, ", ") + ". Search uses the new value at its next request; the other settings use it after DeerFlow restarts."
		}
	}
	if opened != nil {
		if err = opened.finish(true); err != nil {
			out["warning"] = "The secret was stored. Source cleanup: " + err.Error()
		}
	}
	return out, nil
}

func (t *secretTool) unsealValue(ctx context.Context, in secretManageInput) ([]byte, error) {
	if in.ValueSealed == "" {
		// Only a host without the argument sealer delivers a plain value.
		return []byte(in.Value), nil
	}
	if in.Value != "" {
		return nil, fmt.Errorf("%w: pass value only once", harness.ErrInvalidInput)
	}
	raw, err := t.vault.Unseal(secrets.ValueBinding(compose.GetToolCallID(ctx), in.Name), in.ValueSealed)
	if errors.Is(err, secrets.ErrInvalid) {
		return nil, fmt.Errorf("%w: value_sealed is written by the host for one call; pass the secret itself in value", harness.ErrInvalidInput)
	}
	if err != nil {
		return nil, err
	}
	defer clear(raw)
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return nil, fmt.Errorf("%w: value must be a JSON string", harness.ErrInvalidInput)
	}
	return []byte(text), nil
}

func onlySecretName(in secretManageInput, version bool) error {
	if in.Kind != "" || in.Value != "" || in.ValueSealed != "" || in.FromPath != "" || in.Generate != nil || in.Description != nil || in.FileName != "" || in.DeleteSource || !version && in.Version != 0 {
		if version {
			return fmt.Errorf("%w: %s accepts only name and version", harness.ErrInvalidInput, in.Action)
		}
		return fmt.Errorf("%w: %s accepts only name", harness.ErrInvalidInput, in.Action)
	}
	return nil
}

// decodeSecretArguments matches keys exactly: Go's struct decoder would also
// accept other spellings of a key, which could bypass the argument sealer.
func decodeSecretArguments(raw string, allowed []string, into any) error {
	if len(raw) > 4<<20 || !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		return fmt.Errorf("%w: arguments must be one JSON object", harness.ErrInvalidInput)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return fmt.Errorf("%w: arguments must be one JSON object", harness.ErrInvalidInput)
	}
	for key := range fields {
		if !slices.Contains(allowed, key) {
			// Never echo the key: a misplaced value can arrive as a key.
			return fmt.Errorf("%w: unknown argument; allowed arguments are %s", harness.ErrInvalidInput, strings.Join(slices.DeleteFunc(slices.Clone(allowed), func(k string) bool { return k == "value_sealed" || k == "malformed_arguments" }), ", "))
		}
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return fmt.Errorf("%w: an argument has the wrong type", harness.ErrInvalidInput)
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return fmt.Errorf("%w: arguments must be one JSON object", harness.ErrInvalidInput)
	}
	return nil
}

func secretFailure(err error, mutation bool) error {
	switch {
	case errors.Is(err, secrets.ErrInvalid), errors.Is(err, secrets.ErrNotFound), errors.Is(err, secrets.ErrUnavailable), errors.Is(err, secrets.ErrClosed),
		errors.Is(err, harness.ErrInvalidInput), errors.Is(err, harness.ErrPermissionDenied), errors.Is(err, os.ErrNotExist):
		return harness.MarkToolNotExecuted(err)
	case !mutation:
		return harness.MarkToolNoEffect(err)
	default:
		return err
	}
}

func marshalSecretOutput(value any) (string, error) {
	encoded, err := json.Marshal(value)
	return string(encoded), err
}

const (
	alphanumeric  = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	secretSymbols = "!#$%&()*+,-./:;<=>?@[]^_{|}~"
)

func generateSecret(length int, charset string) ([]byte, error) {
	if length == 0 {
		length = 32
	}
	if length < 8 || length > 256 {
		return nil, fmt.Errorf("%w: generate.length must be 8 to 256", harness.ErrInvalidInput)
	}
	alphabet := alphanumeric
	switch charset {
	case "", "alphanumeric":
	case "hex":
		alphabet = "0123456789abcdef"
	case "base64url":
		alphabet = alphanumeric + "-_"
	case "symbols":
		alphabet = alphanumeric + secretSymbols
	default:
		return nil, fmt.Errorf("%w: generate.charset must be alphanumeric, hex, base64url or symbols", harness.ErrInvalidInput)
	}
	// Rejection sampling keeps every character equally likely.
	limit := 256 - 256%len(alphabet)
	out := make([]byte, 0, length)
	buffer := make([]byte, 64)
	for len(out) < length {
		if _, err := rand.Read(buffer); err != nil {
			return nil, err
		}
		for _, b := range buffer {
			if int(b) < limit && len(out) < length {
				out = append(out, alphabet[int(b)%len(alphabet)])
			}
		}
	}
	clear(buffer)
	return out, nil
}

// workspaceSecretPath confines from_path to the session workspace, the same
// boundary as the workspace file tools.
func workspaceSecretPath(workspace, name string) (*os.Root, string, error) {
	if workspace == "" {
		return nil, "", fmt.Errorf("%w: from_path needs a session workspace", harness.ErrInvalidInput)
	}
	rel := name
	if filepath.IsAbs(name) {
		var err error
		if rel, err = filepath.Rel(workspace, name); err != nil {
			return nil, "", fmt.Errorf("%w: from_path must be inside the session workspace", harness.ErrPermissionDenied)
		}
	}
	rel = filepath.Clean(rel)
	if rel == "." || !filepath.IsLocal(rel) {
		return nil, "", fmt.Errorf("%w: from_path must name a file inside the session workspace", harness.ErrPermissionDenied)
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, "", err
	}
	return root, rel, nil
}

type importSource struct {
	file     *os.File
	base     string
	complete func(bool) error
}

func (s *importSource) finish(commit bool) error {
	if s.complete == nil {
		return nil
	}
	finish := s.complete
	s.complete = nil
	return finish(commit)
}
func openWorkspaceImport(workspace, name string, remove bool) (*importSource, error) {
	root, rel, err := workspaceSecretPath(workspace, name)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	parent, err := root.OpenRoot(filepath.Dir(rel))
	if err != nil {
		return nil, fmt.Errorf("%w: cannot open source directory", os.ErrNotExist)
	}
	base := filepath.Base(rel)
	if remove {
		return claimImportSource(parent, base)
	}
	defer parent.Close()
	f, err := parent.Open(base)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot open source file", os.ErrNotExist)
	}
	return &importSource{f, base, func(bool) error { return f.Close() }}, nil
}
func (s *importSource) read(kind secrets.Kind) ([]byte, error) {
	info, err := s.file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: from_path must be a regular file", harness.ErrInvalidInput)
	}
	limit := int64(secrets.MaxFileBytes)
	if kind == secrets.KindString {
		limit = secrets.MaxStringBytes + 2
	}
	value, err := io.ReadAll(io.LimitReader(s.file, limit+1))
	if err != nil || int64(len(value)) > limit {
		clear(value)
		return nil, fmt.Errorf("%w: cannot read source within the size limit", harness.ErrInvalidInput)
	}
	return value, nil
}

// searchSecrets resolves search provider api_key references for one request.
func searchSecrets(vault *secrets.Vault) websearch.SecretLookup {
	return func(ctx context.Context, name string) (string, error) {
		values, err := vault.Resolve(ctx, []string{name})
		if err != nil {
			return "", err
		}
		secret := values[name]
		defer clear(secret.Value)
		if !utf8.Valid(secret.Value) {
			return "", fmt.Errorf("secret %s is binary and cannot be a search credential", name)
		}
		return string(secret.Value), nil
	}
}

// searchSecretExists verifies that a referenced secret currently exists and is
// usable without recording last_used_at.
func searchSecretExists(vault *secrets.Vault) websearch.SecretCheck {
	return func(ctx context.Context, name string) bool {
		meta, err := vault.Get(ctx, name, false)
		return err == nil && meta.DeletedAt == nil && !meta.Unavailable && meta.Kind == secrets.KindString && meta.Size > 0 && meta.Size <= 16384
	}
}
