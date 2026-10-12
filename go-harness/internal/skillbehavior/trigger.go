package skillbehavior

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

const SelectionRuleVersion = "skill-trigger-selection-v1"

func TriggerCase(c harness.SkillBehaviorCase) bool {
	return c.Kind == "trigger_positive" || c.Kind == "trigger_negative"
}

func validateTriggerCase(c harness.SkillBehaviorCase) error {
	if len(c.Files) != 0 || len(c.Fixtures) != 0 || len(c.Assertions) != 1 || len(c.TriggerSkills) > 8 {
		return invalid("trigger cases require one selection assertion and no execution fixtures")
	}
	a := c.Assertions[0]
	want := 0
	if c.Kind == "trigger_positive" {
		want = 1
	}
	if !identifier.MatchString(a.ID) || a.Kind != "skill_selected" || a.Count != want || a.Path != "" || a.Tool != "" || a.Expected != "" {
		return invalid("trigger selection assertion does not match the host label")
	}
	seen := map[string]bool{}
	for _, sk := range c.TriggerSkills {
		if !harness.ValidSkillName(sk.Name) || seen[sk.Name] || !Text(sk.Description, 1024) || strings.TrimSpace(sk.Description) == "" {
			return invalid("trigger competitor metadata is invalid")
		}
		seen[sk.Name] = true
	}
	return nil
}

// SelectionCatalog reads only the immutable evaluated package's metadata.
// Competing descriptions are fixed by the host suite, never by the candidate.
func SelectionCatalog(in harness.SkillBehaviorInput) ([]harness.SkillBehaviorTriggerSkill, string, error) {
	if !TriggerCase(in.Case) || !harness.ValidSkillName(in.SkillName) {
		return nil, "", invalid("trigger target name is invalid")
	}
	if err := ValidateCase(in.Case); err != nil {
		return nil, "", err
	}
	items := append([]harness.SkillBehaviorTriggerSkill{}, in.Case.TriggerSkills...)
	for _, sk := range items {
		if sk.Name == in.SkillName {
			return nil, "", invalid("trigger competitor overrides the evaluated skill")
		}
	}
	if in.Version == "" {
		if len(in.Files) != 0 {
			return nil, "", invalid("absent trigger variant has package files")
		}
	} else {
		name, description, err := skills.ParseSkillDescription(in.Files["SKILL.md"])
		if err != nil || name != in.SkillName {
			return nil, "", invalid("trigger package metadata is invalid or mismatched")
		}
		items = append(items, harness.SkillBehaviorTriggerSkill{Name: name, Description: description})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	data, _ := json.Marshal([]any{SelectionRuleVersion, items})
	return items, Hash(data), nil
}

func SelectionCheck(a harness.SkillBehaviorAssertion, selection harness.SkillBehaviorSelection) harness.SkillBehaviorCheck {
	count := 0
	if selection.SelectedName == selection.TargetName {
		count = 1
	}
	data, _ := json.Marshal([]any{SelectionRuleVersion, selection.TargetName, selection.SelectedName})
	return harness.SkillBehaviorCheck{ID: a.ID, Passed: count == a.Count, ActualDigest: Hash(data)}
}
