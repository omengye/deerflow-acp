package learning

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestExternalInfluenceMarksEpisodesThatConsumedThirdPartyContent(t *testing.T) {
	native, err := sqlite.Open(filepath.Join(t.TempDir(), "learning.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	db := native.DB()
	if _, err = db.Exec(`CREATE TABLE df_learning_extractions(id TEXT PRIMARY KEY,payload BLOB NOT NULL);
CREATE TABLE df_learning_experiences(id TEXT PRIMARY KEY,extraction_id TEXT NOT NULL);
CREATE TABLE df_learning_pattern_members(pattern_id TEXT NOT NULL,revision INTEGER NOT NULL,experience_id TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	view := func(tools ...string) []byte {
		snap := extractionSnapshot{View: harness.SkillLearningInput{References: []harness.SkillLearningReference{{ID: "input/1", Kind: "user"}}}}
		for i, name := range tools {
			snap.View.References = append(snap.View.References, harness.SkillLearningReference{ID: "receipt/" + string(rune('a'+i)), Kind: "tool_observation", ToolName: name})
		}
		data, _ := json.Marshal(snap)
		return data
	}
	for id, payload := range map[string][]byte{
		"local":    view("bash", "read_file", "search_memory"),
		"web":      view("bash", "web_fetch"),
		"mcp":      view("mcp_github_issues_0123456789abcdef"),
		"corrupt":  []byte("{"),
		"attached": view("read_attachment"),
	} {
		if _, err = db.Exec(`INSERT INTO df_learning_extractions VALUES(?,?)`, "x-"+id, payload); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`INSERT INTO df_learning_experiences VALUES(?,?)`, "e-"+id, "x-"+id); err != nil {
			t.Fatal(err)
		}
		revision := 1
		if id == "attached" {
			revision = 3 // a later revision is outside the pinned evidence
		}
		if _, err = db.Exec(`INSERT INTO df_learning_pattern_members VALUES('p',?,?)`, revision, "e-"+id); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	got, err := (&Store{}).ExternalInfluenceTx(context.Background(), tx, "p", 2)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"e-local": false, "e-web": true, "e-mcp": true, "e-corrupt": true}
	if len(got) != len(want) {
		t.Fatalf("members=%v", got)
	}
	for id, external := range want {
		if got[id] != external {
			t.Errorf("%s external=%v want %v", id, got[id], external)
		}
	}
}
