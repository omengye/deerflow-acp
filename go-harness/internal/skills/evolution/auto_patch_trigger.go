package evolution

import (
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillbehavior"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
	"gopkg.in/yaml.v3"
)

// Only description's YAML span can change. All other metadata stays byte
// identical after CRLF normalization, not merely semantically equivalent.
func autoPatchDescriptionChange(base, candidate []byte) (bodyChanged, allowed bool) {
	project := func(data []byte) (rest, description, body string, ok bool) {
		_, description, err := skills.ParseSkillDescription(data)
		if err != nil {
			return "", "", "", false
		}
		text := strings.ReplaceAll(string(data), "\r\n", "\n")
		end := strings.Index(text[4:], "\n---\n")
		if end < 0 {
			return "", "", "", false
		}
		raw := text[4 : 4+end]
		var doc yaml.Node
		if yaml.Unmarshal([]byte(raw), &doc) != nil || len(doc.Content) != 1 {
			return "", "", "", false
		}
		fields := doc.Content[0].Content
		lines := strings.Split(raw, "\n")
		for i := 0; i < len(fields); i += 2 {
			key := fields[i]
			if key.Value != "description" {
				continue
			}
			start, stop := key.Line-1, len(lines)
			if i+2 < len(fields) {
				stop = fields[i+2].Line - 1
			}
			if key.Column != 1 || start < 0 || start >= stop || stop > len(lines) || !strings.HasPrefix(lines[start], "description:") {
				return "", "", "", false
			}
			rest = strings.Join(append(append([]string{}, lines[:start]...), lines[stop:]...), "\n")
			return rest, description, text[4+end+5:], true
		}
		return "", "", "", false
	}
	a, ad, ab, aok := project(base)
	b, bd, bb, bok := project(candidate)
	return ab != bb, aok && bok && a == b && ad != bd
}

func learningAutoPatchContentAllowed(base, candidate []byte, report harness.SkillBehaviorReport) bool {
	if sameAutoPatchFrontmatter(base, candidate) {
		return learningAutoPatchImproved(report)
	}
	bodyChanged, onlyDescription := autoPatchDescriptionChange(base, candidate)
	if !onlyDescription || report.RuleVersion != behaviorTriggerRuleVersion || report.State != "passed" || report.TargetedCases < 1 || report.RegressionCases < 1 || report.TriggerPositiveCases < 1 || report.TriggerNegativeCases < 1 || report.Regressions != 0 {
		return false
	}
	triggerImproved := false
	for _, result := range report.Results {
		if result.BaseErrorCode != "" && result.BaseErrorCode != "assertion_failed" || result.CandidateErrorCode != "" || !result.CandidatePassed {
			return false
		}
		if result.Kind != "trigger_positive" && result.Kind != "trigger_negative" {
			continue
		}
		for _, observation := range []*harness.SkillBehaviorSelection{result.BaseSelection, result.CandidateSelection} {
			if observation == nil || observation.RuleVersion != skillbehavior.SelectionRuleVersion || !hexDigest(observation.CatalogDigest) {
				return false
			}
		}
		if result.BaseSelection.TargetName != result.CandidateSelection.TargetName || result.BaseSelection.CatalogDigest == result.CandidateSelection.CatalogDigest {
			return false
		}
		triggerImproved = triggerImproved || !result.BasePassed && result.BaseErrorCode == "assertion_failed"
	}
	// A concurrent body rewrite needs its own execution improvement; fixing a
	// trigger does not authorize unrelated instruction changes.
	return triggerImproved && (!bodyChanged || learningAutoPatchImproved(report))
}
