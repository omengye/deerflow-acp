package agents

import (
	"encoding/json"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestCatalogSnapshotDetachesAndDetectsMutation(t *testing.T) {
	config := &harness.SubagentsConfig{CustomAgents: map[string]harness.AgentConfig{"reader": {Description: "read", SystemPrompt: "original", Tools: []string{}}}}
	pinned, err := Snapshot(config)
	if err != nil {
		t.Fatal(err)
	}
	config.CustomAgents["reader"] = harness.AgentConfig{Description: "changed", SystemPrompt: "changed"}
	if pinned.Config.CustomAgents["reader"].SystemPrompt != "original" || pinned.Config.CustomAgents["reader"].Tools == nil {
		t.Fatal("snapshot was mutated or empty-list meaning changed")
	}
	raw, err := json.Marshal(pinned)
	if err != nil {
		t.Fatal(err)
	}
	var restored Catalog
	if err = json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if err = restored.Validate(); err != nil {
		t.Fatal(err)
	}
	role := restored.Config.CustomAgents["reader"]
	role.SystemPrompt = "tampered"
	restored.Config.CustomAgents["reader"] = role
	if err = restored.Validate(); err == nil {
		t.Fatal("changed snapshot kept valid digest")
	}
}
