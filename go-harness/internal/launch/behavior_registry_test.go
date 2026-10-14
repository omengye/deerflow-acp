package launch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSkillBehaviorRegistryIsBoundedAuthorizedAndBackwardCompatible(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "fixtures", "skill-behavior-v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := parseBehaviorRegistry(data, []string{"managed-skills"})
	if err != nil || len(first.Suites) != 1 {
		t.Fatal(first, err)
	}
	second := first.Suites[0]
	second.Name = "another-skill"
	second.ID = "another-suite"
	b, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	registry := func(entries []json.RawMessage) []byte {
		b, err := json.Marshal(map[string]any{"version": "skill-behavior-registry-v1", "suites": entries})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	loaded, err := parseBehaviorRegistry(registry([]json.RawMessage{data, b}), []string{"managed-skills"})
	if err != nil || len(loaded.Suites) != 2 || loaded.Suites[1].Name != second.Name {
		t.Fatal(loaded, err)
	}
	for _, bad := range [][]byte{
		registry(nil), registry([]json.RawMessage{data, data}),
		[]byte(`{"version":"skill-behavior-registry-v1","suites":[],"paths":["private.json"]}`),
		[]byte(`{"version":"skill-behavior-registry-v1","suites":[],"suites":[]}`),
	} {
		if _, err := parseBehaviorRegistry(bad, []string{"managed-skills"}); err == nil {
			t.Fatal("invalid registry accepted")
		}
	}
	if _, err := parseBehaviorRegistry(registry([]json.RawMessage{data, b}), []string{"other"}); err == nil {
		t.Fatal("source grant bypassed")
	}
	tooMany := make([]json.RawMessage, 33)
	for i := range tooMany {
		tooMany[i] = data
	}
	if _, err := parseBehaviorRegistry(registry(tooMany), []string{"managed-skills"}); err == nil {
		t.Fatal("registry limit bypassed")
	}
}
