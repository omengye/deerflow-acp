package evolution

import (
	"context"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/learning"
	hr "github.com/omengye/deerflow-acp/go-harness/internal/runtime"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHostArtifactOracleCapturesFinalBytesAndCannotBeRewrittenByCollector(t *testing.T) {
	for _, mode := range []string{"correct", "wrong", "missing", "no-oracle", "escape", "private"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f := newCollectorFixture(t)
			l, err := learning.New(ctx, f.db.DB(), &harness.SkillLearningOptions{Idle: time.Second, Policy: harness.SkillMaintenancePolicy{MaxJobSeconds: 60, Workers: 1}})
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			f.runtime.Learning = l
			f.runtime.CaptureSkillOracle = f.service.CaptureTaskOracleTx
			x, err := f.runtime.CreateSession(ctx, f.cfg.Sources[0].Workspace, "fixture")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = l.SetControl(ctx, x.ID, harness.SkillLearningControlRequest{Contribute: true}); err != nil {
				t.Fatal(err)
			}
			prompt := "Write the computed answer as a text artifact.\nExpected artifact `answer.txt`:\n```text\n42\n```"
			switch mode {
			case "no-oracle":
				prompt = "Write the computed answer as an artifact."
			case "escape":
				prompt = "Write the computed answer as an artifact.\nExpected artifact `../outside.txt`:\n```text\n42\n```"
			case "private":
				prompt = "Write the computed answer as an artifact.\nExpected artifact `answer.txt`:\n```text\napi_key=PRIVATE_CREDENTIAL_LITERAL\n```"
			}
			id := hr.NewID()
			req := harness.RunRequest{Session: x, RunID: id, RootBudgetID: id, InputID: hr.NewID(), Input: []harness.Content{{Type: "text", Text: prompt}}}
			if err = f.runtime.BeginRun(ctx, req); err != nil {
				t.Fatal(err)
			}
			if mode != "missing" {
				content := "42"
				if mode == "wrong" {
					content = "wrong"
				}
				if err = os.WriteFile(filepath.Join(x.CWD, "answer.txt"), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			// A model success claim is never the artifact judgment.
			if _, err = f.runtime.Append(ctx, harness.RunEvent{SessionID: x.ID, RunID: id, Kind: "text_delta", AgentName: "deerflow", Text: "All assertions passed."}); err != nil {
				t.Fatal(err)
			}
			if err = f.runtime.Finish(ctx, id, "end_turn", nil); err != nil {
				t.Fatal(err)
			}
			var count int
			if err = f.db.DB().QueryRow(`SELECT count(*) FROM df_skill_task_oracles WHERE root_id=?`, id).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if mode == "no-oracle" || mode == "escape" || mode == "private" {
				if count != 0 {
					t.Fatal("untrusted oracle admitted")
				}
				return
			}
			var passed bool
			var sha, kind, path string
			if err = f.db.DB().QueryRow(`SELECT o.passed,o.output_sha,k.kind,k.path FROM df_skill_task_oracles o JOIN df_skill_task_oracle_kinds k ON k.root_id=o.root_id WHERE o.root_id=?`, id).Scan(&passed, &sha, &kind, &path); err != nil {
				t.Fatal(err)
			}
			if passed != (mode == "correct") || kind != "file_equals" || path != "answer.txt" {
				t.Fatal("host artifact verification wrong", mode, passed)
			}
			if err = os.WriteFile(filepath.Join(x.CWD, "answer.txt"), []byte("later task changed this"), 0600); err != nil {
				t.Fatal(err)
			}
			tx, err := f.db.DB().BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if err = captureTaskOracleTx(ctx, tx, id); err != nil {
				t.Fatal(err)
			}
			var after string
			if err = tx.QueryRow(`SELECT output_sha FROM df_skill_task_oracles WHERE root_id=?`, id).Scan(&after); err != nil || after != sha {
				t.Fatal("collector replaced completion-time observation", err)
			}
		})
	}
}
