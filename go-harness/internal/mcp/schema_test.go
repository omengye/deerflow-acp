package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestCanonicalToolInfoPreservesSchemaValues(t *testing.T) {
	first := `{"name":"record","has_params_one_of":true,"json_schema":{"type":"object","properties":{"options":{"type":"object","properties":{"timezone":{"type":"string"},"action":{"type":"string","enum":["launch","queue"]}}},"count":{"type":"integer","minimum":9007199254740993}},"required":["options","count"]}}`
	reordered := `{"json_schema":{"required":["options","count"],"properties":{"count":{"minimum":9007199254740993,"type":"integer"},"options":{"properties":{"action":{"enum":["launch","queue"],"type":"string"},"timezone":{"type":"string"}},"type":"object"}},"type":"object"},"has_params_one_of":true,"name":"record"}`
	encode := func(raw string) string {
		t.Helper()
		var info schema.ToolInfo
		if err := json.Unmarshal([]byte(raw), &info); err != nil {
			t.Fatal(err)
		}
		encoded, err := canonicalToolInfo(&info)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	baseline := encode(first)
	if baseline != encode(reordered) {
		t.Fatal("nested object order changed tool identity")
	}
	if !strings.Contains(baseline, "9007199254740993") {
		t.Fatal("schema number lost precision")
	}
	for _, changed := range []string{
		strings.Replace(first, "9007199254740993", "9007199254740994", 1),
		strings.Replace(first, `["launch","queue"]`, `["queue","launch"]`, 1),
		strings.Replace(first, `"type":"integer"`, `"type":"number"`, 1),
	} {
		if baseline == encode(changed) {
			t.Fatal("actual schema change lost from tool identity")
		}
	}
}
