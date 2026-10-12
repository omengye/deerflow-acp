package learning

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func resolutionFixture() (extractionSnapshot, extractionSnapshot, harness.SkillExperienceCandidate, harness.SkillExperienceCandidate) {
	first := sourceRef{Outbox: 1, Run: "first", Input: "first", Accepted: 1, Frontier: 3, Epoch: 1, Status: "completed", Ended: time.Now().UTC().Format(time.RFC3339Nano)}
	later := sourceRef{Outbox: 2, Run: "later", Input: "later", Accepted: 4, Frontier: 8, Epoch: 1, Status: "completed", Ended: first.Ended}
	user := harness.SkillLearningReference{ID: "input/first", RunID: first.Run, InputID: first.Input, Sequence: 1, Kind: "user", Text: "Check complete JSON"}
	correction := harness.SkillLearningReference{ID: "input/later", RunID: later.Run, InputID: later.Input, Sequence: 4, Kind: "user", Text: "Correct the incomplete check"}
	receipt := harness.SkillLearningReference{ID: "receipt/later/read", RunID: later.Run, Kind: "tool_observation", ToolName: "read_file", State: "completed"}
	before := extractionSnapshot{Session: "session", Episode: "episode", Owner: "workspace", Epoch: 1, Sources: []sourceRef{first}, View: harness.SkillLearningInput{References: []harness.SkillLearningReference{user}}}
	after := extractionSnapshot{Session: "session", Episode: "episode", Owner: "workspace", Epoch: 1, Sources: []sourceRef{first, later}, View: harness.SkillLearningInput{References: []harness.SkillLearningReference{user, correction, receipt}}}
	prior := harness.SkillExperienceCandidate{Outcome: "unresolved", References: []string{user.ID}}
	verified := harness.SkillExperienceCandidate{Outcome: "observed_execution", References: []string{user.ID, correction.ID, receipt.ID}}
	return before, after, prior, verified
}

func TestSkillLearningResolutionRequiresSourceCoveredLaterCompletedTask(t *testing.T) {
	for _, mode := range []string{"valid", "user-reported", "failed-earlier-root", "missing-original-citation", "missing-one-original-citation", "changed-original", "truncated-original", "missing-later-citation", "missing-receipt-citation", "old-receipt", "nonlocal", "truncated-receipt", "failed-receipt", "failed-later-root", "no-later-input", "old-outbox", "frontier-not-later", "different-session", "different-episode", "different-epoch", "different-workspace", "delegated-completed", "delegated-failed", "delegated-earlier-frontier"} {
		t.Run(mode, func(t *testing.T) {
			before, after, prior, verified := resolutionFixture()
			switch mode {
			case "user-reported":
				prior.Outcome = "user_reported"
			case "failed-earlier-root":
				before.Sources[0].Status, after.Sources[0].Status = "failed", "failed"
			case "missing-original-citation":
				verified.References = verified.References[1:]
			case "missing-one-original-citation":
				additional := harness.SkillLearningReference{ID: "input/earlier", RunID: "earlier", InputID: "earlier", Sequence: 2, Kind: "user", Text: "Also check required fields"}
				before.View.References = append(before.View.References, additional)
				after.View.References = append(after.View.References, additional)
				prior.References = append(prior.References, additional.ID)
			case "changed-original":
				after.View.References[0].Text = "Different original task"
			case "truncated-original":
				before.View.References[0].Truncated, after.View.References[0].Truncated = true, true
			case "missing-later-citation":
				verified.References = []string{verified.References[0], verified.References[2]}
			case "missing-receipt-citation":
				verified.References = verified.References[:2]
			case "old-receipt":
				after.View.References[2].RunID = "first"
			case "nonlocal":
				after.View.References[2].ToolName = "web_search"
			case "truncated-receipt":
				after.View.References[2].Truncated = true
			case "failed-receipt":
				after.View.References[2].State = "failed"
			case "failed-later-root":
				after.Sources[1].Status = "failed"
			case "no-later-input":
				after.Sources = after.Sources[:1]
			case "old-outbox":
				after.Sources[1].Outbox = 1
			case "frontier-not-later":
				before.Sources[0].Frontier = 5
			case "different-session":
				after.Session = "copy"
			case "different-episode":
				after.Episode = "independent"
			case "different-epoch":
				after.Epoch = 2
			case "different-workspace":
				after.Owner = "foreign"
			case "delegated-completed", "delegated-failed", "delegated-earlier-frontier":
				after.View.References[2].RunID = "child"
				after.Roots = []rootCapture{{Source: "later", Members: []RootObservation{{Run: "child", Status: "completed", Ended: after.Sources[1].Ended}}}}
				if mode == "delegated-failed" {
					after.Roots[0].Members[0].Status = "failed"
				}
				if mode == "delegated-earlier-frontier" {
					before.Roots = []rootCapture{{Source: "first", Members: []RootObservation{{Run: "old-child", Frontier: 6}}}}
				}
			}
			sequence := resolutionAccepted(before, after, prior, verified)
			want := mode == "valid" || mode == "user-reported" || mode == "failed-earlier-root" || mode == "delegated-completed"
			if (sequence == 4) != want || !want && sequence != 0 {
				t.Fatal("unverified source gained same-task resolution authority", sequence)
			}
		})
	}
}

func TestSkillLearningResolutionProjectionPreservesLegacyAndOriginalVotes(t *testing.T) {
	deps := []experienceDependency{{ID: "a", Session: "session", Episode: "episode", Epoch: 1, Independent: "task", Extraction: "before"}, {ID: "b", Session: "session", Episode: "episode", Epoch: 1, Independent: "task", Extraction: "after"}}
	members := []harness.SkillExperience{{ID: "a", EpisodeID: "episode", Candidate: harness.SkillExperienceCandidate{Outcome: "unresolved"}}, {ID: "b", EpisodeID: "episode", Candidate: harness.SkillExperienceCandidate{Outcome: "observed_execution"}}}
	legacy, err := buildPattern("pattern", "session", 2, "b", deps, members, nil, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"legacy", "resolved", "conflicting", "ambiguous", "unversioned", "unknown-policy", "invented", "duplicate", "reversed", "self", "different-session", "different-episode", "different-epoch", "different-lineage", "same-extraction"} {
		t.Run(mode, func(t *testing.T) {
			snap := patternSnapshot{Pattern: legacy, Members: slices.Clone(deps), ResolutionPolicy: ResolutionPolicyVersion, Resolutions: []experienceResolution{{Prior: "a", Verified: "b"}}}
			switch mode {
			case "legacy":
				snap.ResolutionPolicy, snap.Resolutions = "", nil
			case "conflicting":
				snap.Pattern.ConflictsWith = []string{"other"}
			case "ambiguous":
				snap.Pattern.EvidenceState = "ambiguous"
			case "unversioned":
				snap.ResolutionPolicy = ""
			case "unknown-policy":
				snap.ResolutionPolicy = "future"
			case "invented":
				snap.Resolutions[0].Verified = "foreign"
			case "duplicate":
				snap.Resolutions = append(snap.Resolutions, snap.Resolutions[0])
			case "reversed":
				snap.Resolutions[0].Prior, snap.Resolutions[0].Verified = "b", "a"
			case "self":
				snap.Resolutions[0].Verified = "a"
			case "different-session":
				snap.Members[1].Session = "copy"
			case "different-episode":
				snap.Members[1].Episode = "independent"
			case "different-epoch":
				snap.Members[1].Epoch = 2
			case "different-lineage":
				snap.Members[1].Independent = "other"
			case "same-extraction":
				snap.Members[1].Extraction = "before"
			}
			p, err := projectPattern(snap, members)
			want := mode == "legacy" || mode == "resolved" || mode == "conflicting" || mode == "ambiguous"
			if (err == nil) != want {
				t.Fatal(p, err)
			}
			if !want {
				return
			}
			state := "insufficient_evidence"
			if mode == "legacy" {
				state = "unresolved"
				if encodedDigest(p) != encodedDigest(legacy) {
					t.Fatal("legacy pattern was silently upgraded")
				}
				data, _ := json.Marshal(snap)
				if strings.Contains(string(data), `"resolution_policy"`) || strings.Contains(string(data), `"resolutions"`) {
					t.Fatal("legacy optional fields changed saved digests")
				}
			} else if mode == "conflicting" || mode == "ambiguous" {
				state = mode
			}
			if p.EvidenceState != state || p.SupportEpisodes != 1 || p.ObservedEpisodes != 1 || len(p.ExperienceIDs) != 2 || members[0].Candidate.Outcome != "unresolved" {
				t.Fatal("resolution changed history or independent votes", p)
			}
		})
	}
}
