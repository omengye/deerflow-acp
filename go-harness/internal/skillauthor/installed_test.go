package skillauthor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestInstalledValidationDoesNotWeakenEvolution(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fixture")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"SKILL.md":    "---\nname: fixture\ndescription: Query a local service\nmetadata:\n  owner: local\n---\nRun python query.py\n",
		"query.py":    "raise RuntimeError('validation must not execute this')\n",
		"config.json": `{"Token":"fixture-private-123","url":"https://example.invalid"}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if report, err := ValidateInstalled(context.Background(), dir); err != nil || report.Behavior != "not_run" || report.FileCount != 3 {
		t.Fatalf("installed: %+v %v", report, err)
	}
	if _, err := Validate(context.Background(), dir); err == nil {
		t.Fatal("evolution allowed Python")
	}
}
