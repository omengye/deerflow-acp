package skillrun

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestSkillCredentialPlanFreezesManifestAndOptionalBindings(t *testing.T) {
	manifest := `{"version":1,"python":{"min_version":"3.8","entrypoints":["scripts/run.py"]},"secrets":[{"name":"LOGIN","env":"SVC_USER"},{"name":"TOKEN","env":"SVC_KEY"},{"name":"LATER","required":false},{"name":"REFRESH","write":true,"required":false}]}`
	snap, ws, _ := sourceFixtureWithManifest(t, "print('done')", manifest)
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	v, err := secrets.Open(ctx, db.DB(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	in := Input{Action: "run", Name: "sample", Path: "scripts/run.py", Credential: &secrets.CredentialBinding{Service: "https://example.test", UsernameEnv: "SVC_USER", KeyEnv: "SVC_KEY"}}
	plan, err := PrepareSecrets(ctx, snap, in, v)
	if err != nil || len(plan.Read) != 2 || !slices.Equal(plan.Write, []string{"REFRESH"}) || len(plan.Missing) != 0 {
		t.Fatal("incorrect approval plan", plan, err)
	}
	raw, _ := json.Marshal(map[string]any{"secret_plan": plan})
	if !harness.ExplicitSecretApproval(harness.PermissionRequest{ToolName: "skill_run", TrustedBuiltin: true, Arguments: raw}) {
		t.Fatal("manifest did not require approval")
	}
	if _, err = v.Set(ctx, secrets.SetRequest{Name: "LATER", Kind: secrets.KindString, Value: []byte("late-optional-fixture")}); err != nil {
		t.Fatal(err)
	}
	prompts := 0
	ctx = secrets.WithCredentialPrompt(secrets.WithCredentialGrant(ctx), func(context.Context, secrets.CredentialScope, secrets.CredentialFields) (secrets.CredentialPair, error) {
		prompts++
		return secrets.CredentialPair{Username: []byte("fixture-user"), Key: []byte("fixture-key")}, nil
	})
	cfg := harness.SandboxConfig{Enabled: true, Provider: harness.SandboxLocal, AllowedExecutables: []string{"/usr/bin/python3"}}
	for i := 0; i < 2; i++ {
		b := &fakeBackend{workspace: ws}
		r, err := RunPrepared(ctx, snap, b, cfg, ws, in, v, plan)
		if err != nil || !r.ExecutionChecked || prompts != 1 || r.Secrets == nil || len(r.Secrets.Used) != 2 {
			t.Fatal("approved scope changed", err)
		}
		for _, s := range r.Secrets.Used {
			if s.Env == "LATER" {
				t.Fatal("new optional was silently injected")
			}
		}
	}
	fresh, err := PrepareSecrets(ctx, snap, in, v)
	if err != nil || fresh.Read["LATER"] != "LATER" {
		t.Fatal("fresh approval should include new optional", err)
	}
}
