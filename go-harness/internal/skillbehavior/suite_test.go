package skillbehavior

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func fixtureSuite() harness.SkillBehaviorSuite {
	return harness.SkillBehaviorSuite{Version: Version, ID: "suite", SourceID: "project", Name: "research", Cases: []harness.SkillBehaviorCase{{ID: "case", Kind: "targeted", Prompt: "Write the answer", Repetitions: 1, Files: map[string]string{"input.txt": "21"}, Fixtures: []harness.SkillBehaviorFixture{}, Assertions: []harness.SkillBehaviorAssertion{{ID: "answer", Kind: "file_equals", Path: "result.txt", Expected: "42"}}}}}
}
func TestBehaviorSuiteStrictParsingAndDigest(t *testing.T) {
	original := fixtureSuite()
	data, _ := json.Marshal(original)
	got, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := Digest(got)
	var pretty any
	_ = json.Unmarshal(data, &pretty)
	formatted, _ := json.MarshalIndent(pretty, "", "  ")
	again, err := Parse(formatted)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := Digest(again)
	if first != second {
		t.Fatal("formatting changed semantic digest")
	}
	again.Cases[0].Assertions[0].Expected = "43"
	changed, _ := Digest(again)
	if changed == first {
		t.Fatal("oracle edit did not change digest")
	}
	for _, bad := range []string{strings.Replace(string(data), `"version":`, `"version":"duplicate","version":`, 1), strings.Replace(string(data), `"repetitions":1`, `"repetitions":1.5`, 1), strings.Replace(string(data), `"repetitions":1`, `"repetitions":null`, 1), strings.Replace(string(data), `"kind":"targeted"`, `"kind":"unknown"`, 1), strings.Replace(string(data), `"source_id":"project"`, `"source_id":"../project"`, 1), string(data) + `{}`, strings.Replace(string(data), `"prompt":`, `"executable":"cmd.exe","prompt":`, 1)} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Fatal("accepted invalid suite", bad)
		}
	}
}
func TestBehaviorPortablePathsAndToolArguments(t *testing.T) {
	for _, p := range []string{"../outside", "a/../b", "/absolute", `C:\outside`, `\\server\share`, "file:stream", "CON", "con.txt", "lpt1.txt", "COM¹", "LPT².txt", "CON .txt", "dir./file", "dir /file", "a\\b", "./a", "a//b", "a\n.txt"} {
		if ValidPath(p) {
			t.Fatal("unsafe fixture path", p)
		}
	}
	if !ValidPath("nested/input.txt") {
		t.Fatal("valid relative path rejected")
	}
	params := map[string]harness.SkillBehaviorParameter{"value": {Type: "integer", Required: true}}
	for _, raw := range []string{`{"value":2.5}`, `{"value":null}`, `{"value":1,"value":2}`, `{"value":"2"}`, `{}`, `{"value":2,"other":3}`, `[]`} {
		if _, err := Arguments([]byte(raw), params); err == nil {
			t.Fatal("unsafe arguments", raw)
		}
	}
	if _, err := Arguments([]byte(`{"value":2}`), params); err != nil {
		t.Fatal(err)
	}
}
func TestBehaviorSuiteRejectsOracleAndToolContractAmbiguity(t *testing.T) {
	for _, mode := range []string{"shadow", "file-alias", "file-directory", "duplicate-check", "no-target", "empty-contains", "script-assertion"} {
		s := fixtureSuite()
		c := &s.Cases[0]
		switch mode {
		case "shadow":
			c.Fixtures = []harness.SkillBehaviorFixture{{Name: "write_file", Description: "Override", Exchanges: []harness.SkillBehaviorExchange{{Arguments: json.RawMessage(`{}`), Output: "ok"}}}}
		case "file-alias":
			c.Files["INPUT.TXT"] = "other"
		case "file-directory":
			c.Files["input.txt/child"] = "other"
		case "duplicate-check":
			c.Assertions = append(c.Assertions, c.Assertions[0])
		case "no-target":
			c.Kind = "regression"
		case "empty-contains":
			c.Assertions[0] = harness.SkillBehaviorAssertion{ID: "check", Kind: "output_contains"}
		case "script-assertion":
			c.Assertions[0].Kind = "exec"
		}
		if err := Validate(s); err == nil {
			t.Fatal("invalid contract", mode)
		}
	}
}
