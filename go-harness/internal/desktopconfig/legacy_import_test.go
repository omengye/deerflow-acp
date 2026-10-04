package desktopconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLegacyDataImportPreservesRestrictionsAndDoesNotActivateHistory(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "current.yaml")
	if err := os.WriteFile(config, []byte("models:\n  - name: current\n    provider: openai\n    model: fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s := service{config: config, userData: dir}
	old := filepath.Join(dir, "old-agent")
	if err := os.Mkdir(old, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "config.yaml"), []byte("name: legacy-role\nmodel: current\nskills: []\ntool_groups: []\nmcp_servers: []\nmemory_enabled: false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "SOUL.md"), []byte("Complete the assigned inspection."), 0600); err != nil {
		t.Fatal(err)
	}
	request, _ := json.Marshal(map[string]any{"directory": old})
	preview, err := s.legacyImport("legacy-agent-preview", request)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(preview)
	var draft struct {
		Role struct {
			Skills     []string `json:"skills"`
			Tools      []string `json:"tools"`
			DelegateTo []string `json:"delegate_to"`
		}
		DraftOnly bool `json:"draft_only"`
	}
	if json.Unmarshal(encoded, &draft) != nil || !draft.DraftOnly || draft.Role.Tools == nil || len(draft.Role.Tools) != 0 || draft.Role.Skills == nil || draft.Role.DelegateTo == nil {
		t.Fatalf("lost restrictions: %s", encoded)
	}
	history := filepath.Join(dir, "old-history")
	if err = os.MkdirAll(filepath.Join(history, "proposals", "p", "candidate", "sample"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"state.json": `{"schema_version":1,"probations":{"sample":{"status":"probation"}}}`, "proposals/p/proposal.json": `{"id":"p","status":"publishing"}`, "proposals/p/candidate/sample/SKILL.md": "Historical content", "proposals/p/candidate/sample/old.py": "raise RuntimeError('must never execute')"} {
		if err = os.WriteFile(filepath.Join(history, filepath.FromSlash(name)), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	request, _ = json.Marshal(map[string]any{"directory": history})
	first, err := s.legacyImport("legacy-history-import", request)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.legacyImport("legacy-history-import", request)
	if err != nil {
		t.Fatal(err)
	}
	id := first.(map[string]any)["id"].(string)
	if id != again.(map[string]any)["id"] {
		t.Fatal("history import is not idempotent")
	}
	archiveBytes, err := os.ReadFile(filepath.Join(dir, "data", "legacy-history", id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var archive legacyArchive
	if json.Unmarshal(archiveBytes, &archive) != nil || !archive.ReadOnly || len(archive.Entries) != 4 {
		t.Fatal("invalid archive")
	}
	found := false
	for _, entry := range archive.Entries {
		if filepath.Ext(entry.Path) == ".py" {
			found = true
			if !entry.Omitted || entry.Text != "" || len(entry.Content) != 0 {
				t.Fatal("executable copied into history")
			}
		}
	}
	if !found {
		t.Fatal("missing omitted-resource digest")
	}
	request, _ = json.Marshal(map[string]any{"id": id})
	page, err := s.legacyImport("legacy-history-get", request)
	if err != nil {
		t.Fatal(err)
	}
	if page.(map[string]any)["read_only"] != true {
		t.Fatal("history not labelled read-only")
	}
	current, err := os.ReadFile(config)
	if err != nil || string(current) != "models:\n  - name: current\n    provider: openai\n    model: fixture\n" {
		t.Fatal("import changed active configuration")
	}
}
