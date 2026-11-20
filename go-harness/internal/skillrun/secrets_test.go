package skillrun

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/sandbox"
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestRealPythonRunInjectsDeclaredSecrets(t *testing.T) {
	exe, _ := pythonVenv(t)
	ctx := context.Background()
	cfg := harness.SandboxConfig{Enabled: true, Provider: harness.SandboxLocal, AllowedExecutables: []string{exe}, Environment: map[string]string{"DEERFLOW_SKILL_PYTHON": exe}, Limits: harness.CommandLimits{Timeout: 15 * time.Second}}
	manifest := `{"version":1,"python":{"min_version":"3.8","packages":[],"entrypoints":["scripts/run.py"]},"secrets":[{"name":"API_TOKEN","env":"SERVICE_TOKEN","description":"Service API token"},{"name":"REFRESHED","required":false,"write":true}]}`
	script := "import os,pathlib\n" +
		"print('token=' + os.environ['SERVICE_TOKEN'])\n" +
		"out = pathlib.Path(os.environ['DEERFLOW_SECRET_OUT'])\n" +
		"(out / 'REFRESHED').write_text('refreshed-secret-value-1')\n" +
		"(out / 'API_TOKEN').write_text('overwrite-attempt-value')\n"
	snap, ws, _ := sourceFixtureWithManifest(t, script, manifest)
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "vault.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	vault, err := secrets.Open(ctx, store.DB(), filepath.Join(t.TempDir(), "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	defer vault.Close()
	backend, err := sandbox.New(ctx, cfg, ws)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()

	checked, err := Run(ctx, snap, backend, cfg, ws, Input{Action: "check", Name: "sample"}, vault)
	if err != nil || checked.Readiness.Ready || checked.Readiness.Code != "secrets_missing" || len(checked.Readiness.MissingSecrets) != 1 || checked.Readiness.MissingSecrets[0] != "API_TOKEN" {
		t.Fatalf("readiness without secret = %+v, %v", checked.Readiness, err)
	}
	var notExecuted *harness.ToolNotExecutedError
	if _, err = Run(ctx, snap, backend, cfg, ws, Input{Action: "run", Name: "sample", Path: "scripts/run.py"}, vault); !errors.As(err, &notExecuted) || !strings.Contains(err.Error(), "API_TOKEN") {
		t.Fatalf("run without secret = %v", err)
	}
	if _, err = vault.Set(ctx, secrets.SetRequest{Name: "API_TOKEN", Kind: secrets.KindString, Value: []byte("skill-api-token-value")}); err != nil {
		t.Fatal(err)
	}
	r, err := Run(ctx, snap, backend, cfg, ws, Input{Action: "run", Name: "sample", Path: "scripts/run.py"}, vault)
	if err != nil || r.Command == nil || strings.TrimSpace(r.Command.Stdout.Text) != "token=[secret:API_TOKEN]" {
		t.Fatalf("run = %+v, %v", r, err)
	}
	if r.Secrets == nil || len(r.Secrets.Used) != 1 || r.Secrets.Used[0].Env != "SERVICE_TOKEN" || len(r.Secrets.Updated) != 2 {
		t.Fatalf("secret report = %+v", r.Secrets)
	}
	statuses := map[string]string{}
	for _, update := range r.Secrets.Updated {
		statuses[update.Name] = update.Status
	}
	if statuses["API_TOKEN"] != "rejected" || statuses["REFRESHED"] != "created" {
		t.Fatalf("write-back statuses = %v", statuses)
	}
	resolved, err := vault.Resolve(ctx, []string{"API_TOKEN", "REFRESHED"})
	if err != nil || string(resolved["API_TOKEN"].Value) != "skill-api-token-value" || string(resolved["REFRESHED"].Value) != "refreshed-secret-value-1" {
		t.Fatalf("vault after run = %v, %v", resolved, err)
	}
}

func TestRuntimeManifestSecretDeclarations(t *testing.T) {
	base := `{"version":1,"python":{"min_version":"3.8","entrypoints":["run.py"]},"secrets":%s}`
	files := func(secretsJSON string) map[string][]byte {
		return map[string][]byte{"run.py": []byte(""), skills.RuntimeManifestFile: []byte(strings.Replace(base, "%s", secretsJSON, 1))}
	}
	m, _, err := skills.ParseRuntimeManifest(files(`[{"name":"TOKEN"},{"name":"KEY","env":"KEY_FILE","required":false,"write":true}]`))
	if err != nil || len(m.Secrets) != 2 || !m.Secrets[0].IsRequired() || m.Secrets[1].IsRequired() || m.Secrets[1].EnvName() != "KEY_FILE" {
		t.Fatalf("valid declarations = %+v, %v", m.Secrets, err)
	}
	for _, bad := range []string{`{}`, `[{"name":"lower"}]`, `[{"name":"A","env":"PATH"}]`, `[{"name":"A"},{"name":"A"}]`, `[{"name":"A","value":"x"}]`, `[{"name":"A","env":null}]`, `[{"name":"A","env":"X"},{"name":"B","env":"x"}]`} {
		if _, _, err := skills.ParseRuntimeManifest(files(bad)); err == nil {
			t.Errorf("accepted invalid declarations %s", bad)
		}
	}
}
