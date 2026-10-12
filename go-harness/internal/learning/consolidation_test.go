package learning

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestSkillLearningConsolidationDecisionCannotInventTargetOrVotes(t *testing.T) {
	input := harness.SkillExperienceConsolidationInput{Seed: harness.SkillExperienceConsolidationSeed{ID: "seed", EpisodeID: "episode"}, Patterns: []harness.SkillExperienceConsolidationChoice{{ID: "allowed", Revision: 3}}}
	for _, mode := range []string{"equivalent", "conflicting", "distinct", "ambiguous", "foreign", "unexpected", "untargeted", "empty-reason"} {
		d := harness.SkillExperienceConsolidationDecision{Relation: mode, PatternID: "allowed", Reason: "Match environment token=PRIVATE_REASON"}
		switch mode {
		case "distinct", "ambiguous":
			d.PatternID = ""
		case "foreign":
			d.Relation, d.PatternID = "equivalent", "outside"
		case "untargeted":
			d.Relation = "distinct"
		case "empty-reason":
			d.Relation, d.Reason = "equivalent", ""
		}
		result, err := validateConsolidationDecision(input, d)
		want := mode == "equivalent" || mode == "conflicting" || mode == "distinct" || mode == "ambiguous"
		if (err == nil) != want {
			t.Fatalf("%s=%+v %v", mode, result, err)
		}
		if want && result.Reason == d.Reason {
			t.Fatal("reason not redacted")
		}
	}
}

func TestSkillLearningConsolidationEvidenceCacheIsTransactionBoundAndCapped(t *testing.T) {
	a, b := &sql.Tx{}, &sql.Tx{}
	ctx := withEvidenceCache(context.Background(), a)
	cache := transactionEvidenceCache(ctx, a)
	if cache == nil || transactionEvidenceCache(ctx, b) != nil || transactionEvidenceCache(withEvidenceCache(ctx, a), a) != cache || transactionEvidenceCache(withEvidenceCache(ctx, b), b) == cache {
		t.Fatal("provenance cache escaped its transaction")
	}
	for i := 0; i < 4; i++ {
		if !cache.admitSourceTurns(8) {
			t.Fatal("valid source bound rejected")
		}
	}
	if cache.admitSourceTurns(1) || cache.admitSourceTurns(0) || cache.admitSourceTurns(-1) {
		t.Fatal("source validation inventory unbounded")
	}
}

func TestSkillLearningConsolidationLexicalRecallPreservesProcedureBoundaries(t *testing.T) {
	left := proceduralTokens(harness.SkillExperienceCandidate{Goal: "JSON file validation", Applicability: "PowerShell"})
	right := proceduralTokens(harness.SkillExperienceCandidate{Goal: "json FILE validation", Applicability: "Bash"})
	if !left["json"] || !right["json"] || !left["powershell"] || left["bash"] || !right["bash"] {
		t.Fatal("environment or case information lost")
	}
	// Chinese bigrams permit bounded lexical recall without requiring a
	// whitespace tokenizer or equating recall with semantic equivalence.
	chinese := proceduralTokens(harness.SkillExperienceCandidate{Goal: "配置文件检查"})
	if !chinese["配置"] || !chinese["检查"] || chinese["配置文件检查"] {
		t.Fatal("Chinese recall lacks bounded terms")
	}
}

func TestSkillLearningConsolidationPatternCountsHostIndependenceOnly(t *testing.T) {
	deps := []experienceDependency{{ID: "a", Independent: "one"}, {ID: "b", Independent: "one"}, {ID: "c", Independent: "two"}}
	experiences := []harness.SkillExperience{
		{ID: "a", EpisodeID: "same", Candidate: harness.SkillExperienceCandidate{Outcome: "observed_execution"}},
		{ID: "b", EpisodeID: "same", Candidate: harness.SkillExperienceCandidate{Outcome: "observed_execution"}},
		{ID: "c", EpisodeID: "other", Candidate: harness.SkillExperienceCandidate{Outcome: "unresolved"}},
	}
	p, err := buildPattern("pattern", "session", 1, "a", deps, experiences, nil, false, time.Now())
	if err != nil || p.SupportEpisodes != 2 || p.ObservedEpisodes != 1 || p.EvidenceState != "unresolved" || p.Quality != "unassessed" || len(p.ExperienceIDs) != 3 {
		t.Fatal(p, err)
	}
	p, err = buildPattern("pattern", "session", 1, "a", deps, experiences, []string{"conflict"}, false, time.Now())
	if err != nil || p.EvidenceState != "conflicting" {
		t.Fatal(p, err)
	}
}
