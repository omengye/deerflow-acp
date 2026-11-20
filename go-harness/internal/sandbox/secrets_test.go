package sandbox

import (
	"context"
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
	"github.com/omengye/deerflow-acp/go-harness/internal/testutil"
)

func testVault(t *testing.T) *secrets.Vault {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	v, err := secrets.Open(context.Background(), store.DB(), filepath.Join(dir, "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	ctx := context.Background()
	if _, err = v.Set(ctx, secrets.SetRequest{Name: "TOKEN", Kind: secrets.KindString, Value: []byte("token-value-for-sandbox-123")}); err != nil {
		t.Fatal(err)
	}
	if _, err = v.Set(ctx, secrets.SetRequest{Name: "SSH_KEY", Kind: secrets.KindFile, Value: []byte("PRIVATE-KEY-MATERIAL-xyz"), FileName: "id_test"}); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestLocalSecretInjectionMasksOutputAndCollectsWriteBack(t *testing.T) {
	v := testVault(t)
	ctx := context.Background()
	in, err := v.Prepare(ctx, map[string]string{"API_TOKEN": "TOKEN", "KEY_FILE": "SSH_KEY"}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Cleanup()
	b := newTestBackend(t, testConfig(t), testutil.CanonicalTempDir(t))
	r := helperRequest(t, "secretenv")
	r.Secrets = in.Command()
	s := waitTask(t, b, launchTask(t, b, r).ID)
	if s.State != harness.CommandCompleted {
		t.Fatalf("outcome: %+v", s)
	}
	if strings.Contains(s.Stdout.Text, "token-value") || strings.Contains(s.Stdout.Text, "KEY-MATERIAL") {
		t.Fatalf("secret leaked in captured output: %q", s.Stdout.Text)
	}
	if !strings.Contains(s.Stdout.Text, "token=[secret:TOKEN]\n") || !strings.Contains(s.Stdout.Text, "key=[secret:SSH_KEY]\n") || !strings.Contains(s.Stdout.Text, "file=id_test\n") {
		t.Fatalf("secret was not injected: %q", s.Stdout.Text)
	}
	updates, err := in.Collect(ctx, v, "test", nil)
	if err != nil || len(updates) != 1 || updates[0].Status != "updated" {
		t.Fatalf("write-back = %+v, %v", updates, err)
	}
	if got := secrets.Mask("rotated-token-value-456"); got != "[secret:TOKEN]" {
		t.Fatalf("rotated value not masked: %q", got)
	}
}

func TestDockerSecretsStayOutOfArgv(t *testing.T) {
	v := testVault(t)
	in, err := v.Prepare(context.Background(), map[string]string{"API_TOKEN": "TOKEN", "KEY_FILE": "SSH_KEY"}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Cleanup()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := normalizeConfig(harness.SandboxConfig{Enabled: true, Provider: harness.SandboxDocker, AllowShell: true, Environment: map[string]string{"api_token": "configured", "OTHER": "kept"}, Docker: harness.DockerSandboxConfig{Executable: exe, Image: "fixture:locked", AllowedImages: []string{"fixture:locked"}}})
	if err != nil {
		t.Fatal(err)
	}
	r := harness.CommandRequest{Script: "true", Secrets: in.Command()}
	args := dockerCreateArgs(cfg, t.TempDir(), "deerflow-go-fixture", r)
	joined := strings.Join(args, "\x00")
	if strings.Contains(joined, "token-value") || strings.Contains(joined, "configured") {
		t.Fatalf("secret value or shadowed config in argv: %q", args)
	}
	for _, want := range []string{"--env\x00API_TOKEN\x00", "--env\x00OTHER=kept\x00", "--env\x00DEERFLOW_SECRET_OUT\x00", "--env\x00KEY_FILE\x00"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %q", want, args)
		}
	}
	var mounts [][]string
	for i, arg := range args {
		if arg == "--mount" {
			fields, err := csv.NewReader(strings.NewReader(args[i+1])).Read()
			if err != nil {
				t.Fatal(err)
			}
			mounts = append(mounts, fields)
		}
	}
	if len(mounts) != 3 || mounts[1][2] != "target=/run/secrets" || mounts[1][3] != "readonly" || mounts[2][2] != "target=/run/deerflow-secret-out" || len(mounts[2]) != 3 {
		t.Fatalf("secret mounts = %#v", mounts)
	}
	env := strings.Join(environment(dockerSecretEnvironment(r)), "\n")
	if !strings.Contains(env, "API_TOKEN=token-value-for-sandbox-123") || !strings.Contains(env, "KEY_FILE=/run/secrets/KEY_FILE/id_test") || !strings.Contains(env, "DEERFLOW_SECRET_OUT=/run/deerflow-secret-out") {
		t.Fatalf("docker CLI environment = %q", env)
	}
	if cfg.Environment["api_token"] != "configured" {
		t.Fatal("configured environment was mutated")
	}
}

func TestInvalidSecretMaterialIsRejected(t *testing.T) {
	b := newTestBackend(t, testConfig(t), t.TempDir())
	for _, s := range []*harness.CommandSecrets{
		{Env: map[string]string{"PATH": "x"}},
		{Env: map[string]string{"DOCKER_HOST": "tcp://evil:2375"}},
		{Env: map[string]string{"DOCKER_CONFIG": "/tmp/evil"}},
		{Env: map[string]string{"WSLENV": "SECRET/u"}},
		{Env: map[string]string{"HTTPS_PROXY": "http://evil:8080"}},
		{Env: map[string]string{"OK": "a\x00b"}},
		{Files: map[string]string{"KEY": "../escape"}, FilesDir: t.TempDir()},
		{Files: map[string]string{"KEY": "KEY/file"}, FilesDir: "relative"},
	} {
		r := helperRequest(t, "echo")
		r.Secrets = s
		if _, err := b.Start(context.Background(), r); err == nil {
			t.Fatalf("accepted invalid material %+v", s)
		}
	}
}

func TestDockerSecretEnvFileKeepsSecretsOutOfHostCLIEnv(t *testing.T) {
	v := testVault(t)
	in, err := v.Prepare(context.Background(), map[string]string{"API_TOKEN": "TOKEN", "KEY_FILE": "SSH_KEY"}, true)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Cleanup()
	r := harness.CommandRequest{Script: "true", Secrets: in.Command()}
	envFile, fallbackEnv, cleanup, err := writeDockerSecretEnvFile(dockerSecretEnvironment(r), r)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if envFile == "" || len(fallbackEnv) != 0 {
		t.Fatalf("envFile=%q fallbackEnv=%v", envFile, fallbackEnv)
	}
	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "API_TOKEN=token-value-for-sandbox-123\n") {
		t.Fatalf("envFile content = %q", data)
	}
	cleanup()
	if _, err := os.Stat(envFile); !os.IsNotExist(err) {
		t.Fatalf("envFile not removed: %v", err)
	}
}
