package desktopconfig

import (
	"testing"

	deerflow "github.com/omengye/deerflow-acp/go-harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/launch"
)

func TestSkillLearningReviewDraftSettingsSaveLaunchDefaultOffAndEffective(t *testing.T) {
	options, doc := reviewDocument(t)
	e := readObject(doc, "skill_evolution")
	d, l := readObject(e, "discovery"), readObject(e, "learning")
	if d["experience_review_drafts_enabled"] != false {
		t.Fatal("automatic review drafts enabled by default")
	}
	e["generation_model_name"] = "first"
	readObject(e, "generation")["enabled"] = true
	l["enabled"], l["consolidation_enabled"], l["matching_enabled"] = true, true, true
	d["experience_patterns_enabled"], d["experience_review_drafts_enabled"] = true, true
	run(t, options, "save", doc)
	var cfg deerflow.Config
	connections := 0
	source, err := launch.ApplyConfig(options.Config, &cfg, &connections, nil)
	if err != nil || cfg.SkillDiscovery == nil || !cfg.SkillDiscovery.ExperienceReviewDraftsEnabled || cfg.SkillDiscovery.RepeatThreshold != 2 || cfg.SkillAutoPatch != nil {
		t.Fatal("draft settings widened auto-publication or were not consumed", cfg.SkillDiscovery, err)
	}
	f, ok := source.Settings.Fields["/skill_evolution/discovery/experience_review_drafts_enabled"]
	if !ok || f.Value != true || f.Source != "yaml" || len(source.Settings.Fields) > 96 {
		t.Fatal("effective draft field missing or outside desktop envelope", source.Settings)
	}
}
