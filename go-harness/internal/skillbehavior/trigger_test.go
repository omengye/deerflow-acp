package skillbehavior

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func triggerSuite() harness.SkillBehaviorSuite {
	s := fixtureSuite()
	s.Version = TriggerVersion
	for _, kind := range []string{"trigger_positive", "trigger_negative"} {
		count := 0
		if kind == "trigger_positive" {
			count = 1
		}
		s.Cases = append(s.Cases, harness.SkillBehaviorCase{ID: kind, Kind: kind, Prompt: "Decide which skill applies", Repetitions: 1, Files: map[string]string{}, Fixtures: []harness.SkillBehaviorFixture{}, Assertions: []harness.SkillBehaviorAssertion{{ID: "private_label", Kind: "skill_selected", Count: count}}, TriggerSkills: []harness.SkillBehaviorTriggerSkill{{Name: "other-task", Description: "Handle the neighboring task"}}})
	}
	return s
}

func TestSkillTriggerSuiteExplicitContractAndHiddenOracle(t *testing.T) {
	s := triggerSuite()
	data, _ := json.Marshal(s)
	if _, err := Parse(data); err != nil {
		t.Fatal(err)
	}
	digest, _ := Digest(s)
	s.Cases[1].TriggerSkills[0].Description = "Different fixed competing description"
	changed, _ := Digest(s)
	if digest == changed {
		t.Fatal("host competitor change did not fence saved evaluations")
	}
	for _, mode := range []string{"v1", "missing-positive", "missing-negative", "mixed-oracle", "wrong-label", "execution-file", "execution-tool", "execution-assertion", "duplicate-name", "target-override", "too-many", "null", "unknown", "execution-catalog"} {
		t.Run(mode, func(t *testing.T) {
			s := triggerSuite()
			c := &s.Cases[1]
			switch mode {
			case "v1":
				s.Version = Version
			case "missing-positive":
				s.Cases = append(s.Cases[:1], s.Cases[2])
			case "missing-negative":
				s.Cases = s.Cases[:2]
			case "mixed-oracle":
				c.Assertions = append(c.Assertions, harness.SkillBehaviorAssertion{ID: "answer", Kind: "output_equals", Expected: "secret"})
			case "wrong-label":
				c.Assertions[0].Count = 0
			case "execution-file":
				c.Files["task.txt"] = "real task data"
			case "execution-tool":
				c.Fixtures = append(c.Fixtures, harness.SkillBehaviorFixture{Name: "external"})
			case "execution-assertion":
				c.Assertions[0].Kind = "output_equals"
			case "duplicate-name":
				c.TriggerSkills = append(c.TriggerSkills, c.TriggerSkills[0])
			case "target-override":
				c.TriggerSkills[0].Name = s.Name
			case "too-many":
				for range 9 {
					c.TriggerSkills = append(c.TriggerSkills, harness.SkillBehaviorTriggerSkill{Name: "another", Description: "Neighbor"})
				}
			case "execution-catalog":
				s.Cases[0].TriggerSkills = c.TriggerSkills
			}
			data, _ := json.Marshal(s)
			if mode == "null" {
				var tree map[string]any
				_ = json.Unmarshal(data, &tree)
				tree["cases"].([]any)[1].(map[string]any)["trigger_skills"] = nil
				data, _ = json.Marshal(tree)
			}
			if mode == "unknown" {
				data = []byte(strings.Replace(string(data), `"trigger_skills":[`, `"trigger_script":"shell","trigger_skills":[`, 1))
			}
			if _, err := Parse(data); err == nil {
				t.Fatal("unsafe or ambiguous trigger contract accepted", mode)
			}
		})
	}
	if _, err := Parse(mustSuiteJSON(t, fixtureSuite())); err != nil {
		t.Fatal("v1 compatibility changed", err)
	}
}

func mustSuiteJSON(t *testing.T, s harness.SkillBehaviorSuite) []byte {
	t.Helper()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestSkillTriggerCatalogPinsMetadataNotBodyOrOracle(t *testing.T) {
	s := triggerSuite()
	in := harness.SkillBehaviorInput{SkillName: s.Name, Version: strings.Repeat("a", 64), Case: s.Cases[1], Files: map[string][]byte{"SKILL.md": []byte("---\nname: research\ndescription: Verify sourced facts\n---\nPRIVATE BODY\n")}}
	items, first, err := SelectionCatalog(in)
	if err != nil || len(items) != 2 {
		t.Fatal(items, err)
	}
	in.Files["SKILL.md"] = []byte(strings.ReplaceAll(string(in.Files["SKILL.md"]), "PRIVATE BODY", "Different instructions"))
	_, again, err := SelectionCatalog(in)
	if err != nil || again != first {
		t.Fatal("body influenced metadata catalog", err)
	}
	in.Files["SKILL.md"] = []byte(strings.ReplaceAll(string(in.Files["SKILL.md"]), "Verify sourced facts", "Verify historical sources"))
	_, changed, err := SelectionCatalog(in)
	if err != nil || changed == first {
		t.Fatal("description not pinned", err)
	}
	selection := harness.SkillBehaviorSelection{RuleVersion: SelectionRuleVersion, TargetName: s.Name, SelectedName: s.Name, CatalogDigest: first}
	if !SelectionCheck(in.Case.Assertions[0], selection).Passed {
		t.Fatal("positive selection not graded")
	}
	in.Case = s.Cases[2]
	if SelectionCheck(in.Case.Assertions[0], selection).Passed {
		t.Fatal("negative overtrigger passed")
	}
	in.Version, in.Files = "", nil
	items, _, err = SelectionCatalog(in)
	if err != nil || len(items) != 1 || items[0].Name == s.Name {
		t.Fatal("absent base invented a skill", items, err)
	}
}
