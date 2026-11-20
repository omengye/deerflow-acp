// Package skillrun executes pinned skill source through the existing command policy.
package skillrun

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
	"github.com/omengye/deerflow-acp/go-harness/internal/tools"
)

type Input struct {
	Action         string                     `json:"action" jsonschema:"required,enum=check,enum=run"`
	Name           string                     `json:"name" jsonschema:"required,description=Enabled skill in this execution snapshot"`
	Path           string                     `json:"path,omitempty" jsonschema:"description=Packaged relative .py entrypoint for run"`
	Args           []string                   `json:"args,omitempty"`
	TimeoutSeconds int64                      `json:"timeout_seconds,omitempty"`
	Credential     *secrets.CredentialBinding `json:"credential,omitempty" jsonschema_description:"Vault username/key for this skill and service. Specify service origin and username_env and/or key_env. Enter the values in the dedicated credential form."`
	Secrets        map[string]string          `json:"secrets,omitempty" jsonschema_description:"Optional extra map from environment variable to vault secret name for run. Secrets declared in skill-runtime.json are injected automatically. Values never appear in output; known values are shown as [secret:NAME]."`
}

type Readiness struct {
	Ready                bool              `json:"ready"`
	Code                 string            `json:"code"`
	PythonVersion        string            `json:"python_version,omitempty"`
	Packages             map[string]string `json:"packages,omitempty"`
	Missing              []string          `json:"missing,omitempty"`
	DependenciesDeclared bool              `json:"dependencies_declared"`
	// MissingSecrets are required vault secrets that do not exist yet.
	MissingSecrets []string `json:"missing_secrets,omitempty"`
	SecretsHint    string   `json:"secrets_hint,omitempty"`
}

type Result struct {
	Name              string                   `json:"name"`
	Version           string                   `json:"version"`
	Provider          harness.SandboxProvider  `json:"provider"`
	Interpreter       string                   `json:"interpreter,omitempty"`
	Readiness         Readiness                `json:"readiness"`
	ExecutionChecked  bool                     `json:"execution_checked"`
	Command           *harness.CommandSnapshot `json:"command,omitempty"`
	RetainedDirectory string                   `json:"retained_directory,omitempty"`
	Secrets           *tools.SecretReport      `json:"secrets,omitempty"`
}

func ValidateInput(in Input) error {
	if in.Name == "" || len(in.Name) > 64 || in.Action != "check" && in.Action != "run" || in.TimeoutSeconds < 0 || in.TimeoutSeconds > 3600 || len(in.Args) > 128 {
		return harness.ErrInvalidInput
	}
	if in.Action == "check" && (in.Path != "" || len(in.Args) != 0 || len(in.Secrets) != 0 || in.Credential != nil) {
		return harness.ErrInvalidInput
	}
	if _, _, err := in.Credential.Bind("skill:"+in.Name, in.Secrets); err != nil {
		return err
	}
	if secrets.ValidateBindings(in.Secrets) != nil {
		return harness.ErrInvalidInput
	}
	if in.Action == "run" && (in.Path == "" || path.Clean(in.Path) != in.Path || strings.ContainsAny(in.Path, "\\:\x00") || strings.HasPrefix(in.Path, "/") || strings.HasPrefix(in.Path, "../") || strings.ToLower(path.Ext(in.Path)) != ".py") {
		return harness.ErrInvalidInput
	}
	n := 0
	for _, a := range in.Args {
		n += len(a)
		if strings.ContainsRune(a, 0) || n > 32768 {
			return harness.ErrInvalidInput
		}
	}
	return nil
}

func Interpreter(cfg harness.SandboxConfig) (string, error) {
	if !cfg.Enabled {
		return "", harness.ErrSandboxDisabled
	}
	value := cfg.Environment["DEERFLOW_SKILL_PYTHON"]
	if cfg.Provider == harness.SandboxPowerShell {
		if !cfg.AllowShell {
			return "", harness.ErrPermissionDenied
		}
		if value == "" {
			for _, name := range []string{"python", "python3"} {
				if found, err := exec.LookPath(name); err == nil && !strings.Contains(strings.ToLower(found), "windowsapps") {
					value = found
					break
				}
			}
		}
		if !filepath.IsAbs(value) {
			return "", harness.ErrSandboxUnavailable
		}
		return value, nil
	}
	if value == "" {
		for _, allowed := range cfg.AllowedExecutables {
			name := strings.ToLower(path.Base(strings.ReplaceAll(allowed, "\\", "/")))
			if name == "python" || name == "python.exe" || name == "python3" || strings.HasPrefix(name, "python3.") {
				value = allowed
				break
			}
		}
	}
	if value == "" || !slices.Contains(cfg.AllowedExecutables, value) {
		return "", harness.ErrPermissionDenied
	}
	return value, nil
}

func command(cfg harness.SandboxConfig, interpreter string, args []string, timeout time.Duration) harness.CommandRequest {
	r := harness.CommandRequest{Executable: interpreter, Args: args, Timeout: timeout}
	if cfg.Provider == harness.SandboxPowerShell {
		// Legacy Windows PowerShell drops empty argv and embedded quotes. Carry
		// Python's code and positional arguments as inert base64 JSON instead.
		if i := slices.Index(args, "-c"); i >= 0 && i+1 < len(args) {
			data, _ := json.Marshal(args[i+1:])
			args = append(slices.Clone(args[:i+1]), "import sys,json,base64; a=json.loads(base64.b64decode(sys.argv[1])); sys.argv=['-c']+a[1:]; exec(compile(a[0],'<skill-run>','exec'))", base64.StdEncoding.EncodeToString(data))
		}
		quote := func(v string) string { return "'" + strings.ReplaceAll(v, "'", "''") + "'" }
		parts := []string{"&", quote(interpreter)}
		for _, a := range args {
			parts = append(parts, quote(a))
		}
		r.Executable, r.Args = "", nil
		r.Script = strings.Join(parts, " ") + "; exit $LASTEXITCODE"
	}
	return r
}

// Only the trusted interpreter and standard library run here, not skill code or
// dependency imports. Metadata discovery never installs missing packages.
// Before Python 3.14, -S also skips venv prefix setup. Restore only the prefixes;
// never call site.main/addsitedir, which would execute startup hooks and .pth code.
const probe = `import sys,json,site,pathlib,importlib.metadata as m,re
r=json.loads(sys.argv[1]); versions={}; missing=[]
prefixes=[sys.base_prefix,sys.base_exec_prefix]; exe=pathlib.Path(sys.executable)
for p in [exe.parent,exe.parent.parent]:
 cfg=p/'pyvenv.cfg'
 if cfg.is_file():
  include=True
  for line in cfg.read_text(encoding='utf-8').splitlines():
   key,sep,value=line.partition('=')
   if sep and key.strip().lower()=='include-system-site-packages': include=value.strip().lower()=='true'
  sys.prefix=sys.exec_prefix=str(p)
  prefixes=[str(p)]+(prefixes if include else []); break
paths=site.getsitepackages(prefixes); normalize=lambda n:re.sub(r'[-_.]+','-',n).lower()
installed={}
for d in m.distributions(path=paths):
 if d.metadata['Name']: installed.setdefault(normalize(d.metadata['Name']),d.version)
for p in r['packages']:
 if normalize(p) in installed: versions[p]=installed[normalize(p)]
 else: missing.append(p)
v=tuple(int(x) for x in r['min_version'].split('.')); ok=sys.version_info[:2]>=v
print(json.dumps({'ready':ok and not missing,'code':'ready' if ok and not missing else ('python_version_unsupported' if not ok else 'dependencies_missing'),'python_version':'.'.join(map(str,sys.version_info[:3])),'packages':versions,'missing':missing}))`

const bootstrap = `import sys,runpy,pathlib
p=pathlib.Path(sys.argv[1]).resolve(); sys.argv=sys.argv[1:]; sys.path.insert(0,str(p.parent)); runpy.run_path(str(p),run_name='__main__')`

// SecretPlan is host-prepared from the immutable skill version before approval.
type SecretPlan struct {
	Read       map[string]string        `json:"read,omitempty"`
	Write      []string                 `json:"write,omitempty"`
	Missing    []string                 `json:"missing,omitempty"`
	Credential *secrets.CredentialScope `json:"credential_scope,omitempty"`
}

func credentialConsumer(snapshot *skills.Snapshot, name string) string {
	for _, ref := range snapshot.Refs() {
		if ref.Name == name {
			// Installation identity survives version updates and session changes.
			digest := sha256.Sum256([]byte(ref.SourceID + "\x00" + ref.SourceIdentity))
			return fmt.Sprintf("skill:%s [%s@%x]", name, ref.SourceID, digest[:8])
		}
	}
	return ""
}
func planSecrets(ctx context.Context, snapshot *skills.Snapshot, in Input, declared []skills.SecretRequirement, vault *secrets.Vault) (*SecretPlan, error) {
	scope, extra, err := in.Credential.Bind(credentialConsumer(snapshot, in.Name), in.Secrets)
	if err != nil {
		return nil, err
	}
	plan := &SecretPlan{Read: map[string]string{}}
	if in.Credential != nil {
		plan.Credential = &scope
	}
	for _, r := range declared {
		if r.Write {
			plan.Write = append(plan.Write, r.Name)
		}
		overridden := false
		for env := range extra {
			if strings.EqualFold(env, r.EnvName()) {
				overridden = true
				break
			}
		}
		if overridden {
			continue
		}
		exists := false
		if vault != nil {
			m, err := vault.Missing(ctx, []string{r.Name})
			if err != nil {
				return nil, err
			}
			exists = len(m) == 0
		}
		if exists {
			plan.Read[r.EnvName()] = r.Name
		} else if r.IsRequired() {
			plan.Missing = append(plan.Missing, r.Name)
		}
	}
	for env, name := range extra {
		plan.Read[env] = name
	}
	slices.Sort(plan.Write)
	return plan, secrets.ValidateBindings(plan.Read)
}

func PrepareSecrets(ctx context.Context, snapshot *skills.Snapshot, in Input, vault *secrets.Vault) (*SecretPlan, error) {
	if in.Action != "run" {
		return &SecretPlan{}, nil
	}
	files, err := snapshot.Files(ctx, in.Name)
	if err != nil {
		return nil, err
	}
	entries := map[string][]byte{}
	for _, f := range files {
		entries[f.Path] = nil
		if f.Path == skills.RuntimeManifestFile {
			entries[f.Path], err = snapshot.ReadFile(ctx, in.Name, f.Path)
			if err != nil {
				return nil, err
			}
		}
	}
	manifest, _, err := skills.ParseRuntimeManifest(entries)
	if err != nil {
		return nil, err
	}
	return planSecrets(ctx, snapshot, in, manifest.Secrets, vault)
}

func Run(ctx context.Context, snapshot *skills.Snapshot, backend harness.CommandBackend, cfg harness.SandboxConfig, workspace string, in Input, vault *secrets.Vault) (Result, error) {
	return RunPrepared(ctx, snapshot, backend, cfg, workspace, in, vault, nil)
}

func RunPrepared(ctx context.Context, snapshot *skills.Snapshot, backend harness.CommandBackend, cfg harness.SandboxConfig, workspace string, in Input, vault *secrets.Vault, approved *SecretPlan) (Result, error) {
	r := Result{Name: in.Name, Provider: cfg.Provider}
	if err := ValidateInput(in); err != nil {
		return r, harness.MarkToolNotExecuted(err)
	}
	if snapshot == nil || backend == nil {
		return r, harness.MarkToolNotExecuted(harness.ErrSandboxUnavailable)
	}
	for _, ref := range snapshot.Refs() {
		if ref.Name == in.Name {
			r.Version = ref.Version
		}
	}
	if r.Version == "" {
		return r, harness.MarkToolNotExecuted(harness.ErrPermissionDenied)
	}
	manifest, err := snapshot.Files(ctx, in.Name)
	if err != nil {
		return r, harness.MarkToolNotExecuted(err)
	}
	files := map[string][]byte{}
	total := 0
	for _, f := range manifest {
		b, err := snapshot.ReadFile(ctx, in.Name, f.Path)
		if err != nil {
			return r, harness.MarkToolNotExecuted(err)
		}
		total += len(b)
		if total > 2<<20 || len(files) >= 256 {
			return r, harness.MarkToolNotExecuted(harness.ErrInvalidInput)
		}
		files[f.Path] = b
	}
	m, declared, err := skills.ParseRuntimeManifest(files)
	if err != nil {
		return r, harness.MarkToolNotExecuted(err)
	}
	if in.Action == "run" {
		if _, exists := files[in.Path]; !exists || declared && !slices.Contains(m.Python.Entrypoints, in.Path) {
			return r, harness.MarkToolNotExecuted(harness.ErrPermissionDenied)
		}
	}
	r.Interpreter, err = Interpreter(cfg)
	if err != nil {
		r.Readiness = Readiness{Code: "python_unavailable_or_not_authorized", DependenciesDeclared: declared}
		if in.Action == "run" {
			return r, harness.MarkToolNotExecuted(err)
		}
		return r, nil
	}
	limit := cfg.Limits.Timeout
	if limit == 0 {
		limit = 2 * time.Minute
	}
	if in.TimeoutSeconds > 0 && time.Duration(in.TimeoutSeconds)*time.Second < limit {
		limit = time.Duration(in.TimeoutSeconds) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	if m.Python.Packages == nil {
		m.Python.Packages = []string{}
	}
	input, _ := json.Marshal(m.Python)
	checked, err := tools.ExecuteCommand(ctx, backend, command(cfg, r.Interpreter, []string{"-I", "-S", "-B", "-c", probe, string(input)}, min(limit, 15*time.Second)))
	if err != nil {
		r.Readiness = Readiness{Code: "python_check_failed", DependenciesDeclared: declared}
		r.Command = &checked
		return r, err
	}
	if checked.Stdout.Truncated || json.Unmarshal([]byte(checked.Stdout.Text), &r.Readiness) != nil {
		return r, harness.MarkToolExecutionFailed(errors.New("invalid Python readiness response"))
	}
	r.Readiness.DependenciesDeclared = declared
	if approved == nil || in.Action == "check" {
		approved, err = planSecrets(ctx, snapshot, in, m.Secrets, vault)
	}
	if err != nil {
		return r, harness.MarkToolNotExecuted(err)
	}
	requiredMissing := approved.Missing
	if len(requiredMissing) > 0 {
		r.Readiness.MissingSecrets = requiredMissing
		r.Readiness.SecretsHint = "Supply a credential binding for this service and map username_env/key_env to the declared variables."
		if r.Readiness.Ready {
			r.Readiness.Ready, r.Readiness.Code = false, "secrets_missing"
		}
	}
	if !r.Readiness.Ready || in.Action == "check" {
		if in.Action == "run" {
			if r.Readiness.Code == "secrets_missing" {
				return r, harness.MarkToolNotExecuted(errors.New("required secrets are missing: " + strings.Join(requiredMissing, ", ") + "; specify the service and credential environment bindings to open the credential form"))
			}
			return r, harness.MarkToolNotExecuted(errors.New("Python runtime requirements are not ready"))
		}
		return r, nil
	}
	if in.Credential != nil {
		scope, _, err := in.Credential.Bind(credentialConsumer(snapshot, in.Name), in.Secrets)
		if err != nil {
			return r, harness.MarkToolNotExecuted(err)
		}
		if approved.Credential == nil || *approved.Credential != scope {
			return r, harness.MarkToolNotExecuted(harness.ErrPermissionDenied)
		}
		ctx, err = vault.CredentialContext(ctx, scope, in.Credential.Fields())
		if err != nil {
			return r, harness.MarkToolNotExecuted(err)
		}
	}

	root, err := os.OpenRoot(workspace)
	if err != nil {
		return r, err
	}
	defer root.Close()
	// Exclusive, random run directory; source installation is never executed.
	rel := ".deerflow-skill-run-" + rand.Text()
	if err = root.Mkdir(rel, 0700); err != nil {
		return r, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = root.RemoveAll(rel)
		}
	}()
	staged, err := root.OpenRoot(rel)
	if err != nil {
		return r, err
	}
	for name, data := range files {
		if err = staged.MkdirAll(filepath.Dir(name), 0700); err != nil {
			break
		}
		var f *os.File
		f, err = staged.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			break
		}
		_, err = f.Write(data)
		err = errors.Join(err, f.Close())
		if err != nil {
			break
		}
	}
	err = errors.Join(err, staged.Close())
	if err != nil {
		return r, err
	}
	args := append([]string{"-I", "-B", "-c", bootstrap, path.Join(rel, in.Path)}, in.Args...)
	result, report, err := tools.ExecuteWithSecrets(ctx, backend, command(cfg, r.Interpreter, args, limit), tools.SecretRequest{
		Vault: vault, Bindings: approved.Read, WriteBack: len(approved.Write) > 0, Actor: "skill_run:" + in.Name,
		Writable: func(name string) bool { return slices.Contains(approved.Write, name) },
	})
	r.Secrets = report
	// Clean up staged files whenever termination is confirmed OR nothing ran
	// (e.g. secret preparation / missing secret failure before process launch).
	var notExecuted *harness.ToolNotExecutedError
	cleanup = result.TerminationConfirmed || errors.As(err, &notExecuted)
	if !cleanup {
		r.RetainedDirectory = rel
	}
	r.Command = &result
	r.ExecutionChecked = result.TerminationConfirmed && result.ExitCode != nil && *result.ExitCode == 0 && result.State == harness.CommandCompleted
	return r, err
}
