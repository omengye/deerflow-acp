package launch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillbehavior"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
	"go.yaml.in/yaml/v3"
)

// Opt-in release acceptance. Reads the actual primary route without modifying
// its profile, running old work or exposing provider credentials in diagnostics.
func TestLivePrimaryModelAndOCIAcceptance(t *testing.T) {
	path := os.Getenv("DEERFLOW_EVOLUTION_ACCEPTANCE_CONFIG")
	if path == "" {
		t.Skip("requires an explicitly selected live primary-model profile")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	if !skillbehavior.RuntimeReady(ctx) {
		t.Fatal("OCI runtime unavailable")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("cannot read selected profile")
	}
	var source runtimeDocument
	var native map[string]any
	if yaml.Unmarshal(raw, &source) != nil || yaml.Unmarshal(raw, &native) != nil {
		t.Fatal("invalid selected profile")
	}
	name := source.DefaultModel
	if name == "" && len(source.Models) > 0 {
		name = source.Models[0].Name
	}
	var route harness.ModelRoute
	for _, m := range source.Models {
		if m.Name != name {
			continue
		}
		route.Provider, err = modelProvider(m.provider())
		if err != nil {
			t.Fatal("invalid primary provider")
		}
		route.Model = m.Model
		route.API = m.API
		route.OutputTokenParameter = m.OutputTokenParameter
		route.BaseURL, err = resolveConfigValue(m.BaseURL)
		if err != nil {
			t.Fatal("primary endpoint unavailable")
		}
		route.APIKey, err = resolveConfigValue(m.APIKey)
		if err != nil {
			t.Fatal("primary credential unavailable")
		}
	}
	if route.Model == "" {
		t.Fatal("primary model missing")
	}
	route.Network, err = ResolveNetworkConfig(native["network"], ConfigValues{})
	if err != nil {
		t.Fatal("primary network unavailable")
	}
	evaluator, err := deerflow.NewSkillBehaviorEvaluator(ctx, harness.SkillBehaviorModelConfig{Route: route, Timeout: 120 * time.Second, MaxModelCalls: 6, MaxToolCalls: 12, MaxOutputTokens: 1024})
	if err != nil {
		t.Fatal("cannot initialize primary evaluator")
	}
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "live-acceptance.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	queue, err := skillmaintenance.New(ctx, db.DB(), skillmaintenance.Policy{MaxJobSeconds: 900, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	job, err := queue.Enqueue(ctx, skillmaintenance.Request{Kind: skillmaintenance.SkillBehavior, SourceIdentity: strings.Repeat("a", 64), Name: "isolated-answer", Key: "live-acceptance", Reference: "live-acceptance", InputDigest: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	var results []harness.SkillBehaviorResult
	var failure error
	worked, err := queue.RunOne(ctx, func(work context.Context, _ skillmaintenance.Job) (string, bool) {
		for _, variant := range []string{"base", "candidate"} {
			value := "21"
			if variant == "candidate" {
				value = "42"
			}
			files := map[string][]byte{"SKILL.md": []byte("---\nname: isolated-answer\ndescription: Produce the exact answer artifact by running scripts/answer.py.\n---\nRun scripts/answer.py unchanged exactly once with run_script using path scripts/answer.py and args_json []. Do not replace or repair its output.\n"), "scripts/answer.py": []byte("from pathlib import Path\nPath('answer.txt').write_text('" + value + "')\nprint('done')\n")}
			in := harness.SkillBehaviorInput{EvaluationID: "live-acceptance", CaseID: "answer", Variant: variant, Version: skillbehavior.Hash(files["scripts/answer.py"]), SkillName: "isolated-answer", Repetition: 1, Files: files, Case: harness.SkillBehaviorCase{ID: "answer", Kind: "targeted", Prompt: "Run the shipped scripts/answer.py unchanged exactly once with run_script using path scripts/answer.py and args_json [] to produce answer.txt. Do not use write_file, repair output, or compute a replacement. Reply done after execution.", Repetitions: 1, Files: map[string]string{}, Fixtures: []harness.SkillBehaviorFixture{}, Assertions: []harness.SkillBehaviorAssertion{{ID: "artifact", Kind: "file_equals", Path: "answer.txt", Expected: "42"}, {ID: "execution", Kind: "tool_calls", Tool: "run_script", Count: 1}}}}
			result, e := evaluator.Evaluate(work, in)
			if e != nil {
				failure = fmt.Errorf("%s provider evaluation unavailable", variant)
				return "model_unavailable", false
			}
			results = append(results, result)
		}
		return "", false
	})
	if err != nil || !worked || failure != nil {
		t.Fatal("live model comparison did not complete", worked, err, failure)
	}
	if len(results) != 2 {
		t.Fatal("comparison incomplete")
	}
	for i, r := range results {
		if r.Passed != (i == 1) || len(r.Scripts) != 1 || r.Scripts[0].Runtime != skillbehavior.RuntimeVersion || r.Scripts[0].ExitCode != 0 {
			t.Fatalf("variant=%s passed=%v code=%s script_proofs=%d", r.Variant, r.Passed, r.ErrorCode, len(r.Scripts))
		}
	}
	receipts, err := queue.Receipts(ctx, job.ID, 0, 100)
	if err != nil || len(receipts) < 2 {
		t.Fatal("live usage missing")
	}
	for _, r := range receipts {
		if r.State != "settled" {
			t.Fatal("live usage unsettled")
		}
	}
	record := struct {
		Model, Environment, Runtime string
		Results                     []harness.SkillBehaviorResult
		Calls                       int
	}{route.Model, evaluator.Identity(), skillbehavior.RuntimeVersion, results, len(receipts)}
	body, _ := json.MarshalIndent(record, "", "  ")
	if output := os.Getenv("DEERFLOW_EVOLUTION_ACCEPTANCE_REPORT"); output != "" {
		if err = os.WriteFile(output, body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("primary=%s baseline_failed=%v candidate_passed=%v isolated_executions=%d settled_calls=%d", route.Model, !results[0].Passed, results[1].Passed, 2, len(receipts))
}
