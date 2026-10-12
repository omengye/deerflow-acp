// Package skilllearningtest provides deterministic loopback model fixtures for
// self-evolution integration tests. It is not a runtime learning adapter.
package skilllearningtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/modelstreamtest"
)

const Instruction = "Verify all required fields from the complete JSON text before reporting completion."
const CapabilityName = "complete-json-audit"

type Provider struct {
	t             *testing.T
	mu            sync.Mutex
	calls         map[string]int
	named         bool
	newCapability bool
	delegated     bool
	resolution    bool
}

const SharedModelID = "shared-provider-model"

func New(t *testing.T) *Provider {
	return &Provider{t: t, calls: make(map[string]int)}
}

// NamedRoutes verifies distinct saved endpoint/credential routes even when
// every role deliberately shares the same underlying provider model ID.
func NamedRoutes(t *testing.T) *Provider {
	p := New(t)
	p.named = true
	return p
}

func NamedNewCapabilityRoutes(t *testing.T) *Provider {
	p := NamedRoutes(t)
	p.newCapability = true
	return p
}

func Delegated(t *testing.T) *Provider {
	p := New(t)
	p.delegated = true
	return p
}

func FollowupResolution(t *testing.T) *Provider {
	p := New(t)
	p.resolution = true
	return p
}

func (p *Provider) Counts() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]int, len(p.calls))
	for stage, count := range p.calls {
		out[stage] = count
	}
	return out
}

func (p *Provider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Model    string
		Stream   bool
		Messages []struct {
			Role, Content string
		}
		Tools []json.RawMessage
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Messages) < 2 || !request.Stream {
		p.t.Error("invalid native pipeline request", err)
		http.Error(w, "invalid fixture request", http.StatusBadRequest)
		return
	}
	stage := request.Model
	if p.named {
		stage = strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")[0]
		if r.URL.Path != "/"+stage+"/v1/chat/completions" || request.Model != SharedModelID || r.Header.Get("Authorization") != "Bearer fixture-"+stage {
			p.t.Error("saved native pipeline route or credential mismatch")
			http.Error(w, "invalid fixture route", http.StatusBadRequest)
			return
		}
		// The native launcher uses the configured evaluation route for both
		// content quality review and its isolated tool-enabled behavior runs.
		if stage == "quality" && len(request.Tools) != 0 {
			stage = "behavior"
		}
	}
	p.mu.Lock()
	p.calls[stage]++
	p.mu.Unlock()
	fail := func(message string) {
		p.t.Error(message)
		http.Error(w, "invalid fixture request", http.StatusBadRequest)
	}
	if stage != "foreground" && stage != "child" {
		for _, message := range request.Messages {
			if strings.Contains(message.Content, "NATIVE_PRIVATE") || strings.Contains(message.Content, "NATIVE_HIDDEN_") || strings.Contains(message.Content, "oracle-native-") || strings.Contains(message.Content, "oracle-delegated-") {
				fail("maintenance received an unredacted secret or host assertion identity")
				return
			}
		}
		if stage != "behavior" && (len(request.Tools) != 0 || len(request.Messages) != 2) {
			fail("isolated maintenance adapter received tools or conversation history")
			return
		}
	}
	message := map[string]any{"role": "assistant"}
	finish := "stop"
	var answer any
	switch stage {
	case "foreground", "child":
		lastUser, results := -1, 0
		for index, message := range request.Messages {
			if message.Role == "user" {
				lastUser, results = index, 0
			} else if message.Role == "tool" {
				results++
			}
		}
		if lastUser < 0 || len(request.Tools) == 0 {
			fail("foreground fixture has no real user input or native tools")
			return
		}
		if p.resolution && !strings.Contains(request.Messages[lastUser].Content, "Correct the previous inspection") && !strings.Contains(request.Messages[lastUser].Content, "Use the adopted research") {
			message["content"] = "The required fields still need a complete local verification."
			break
		}
		if p.delegated && stage == "foreground" {
			if results == 0 {
				message["tool_calls"] = []any{map[string]any{"index": 0, "id": "native-delegate", "type": "function", "function": map[string]string{"name": "background_agent", "arguments": `{"instruction":"DELEGATED_ONLY_NATIVE_INPUT token=NATIVE_PRIVATE_CHILD_TOKEN","role":"worker","description":"Verify complete local input"}`}}}
				finish = "tool_calls"
			} else {
				message["content"] = "Delegated check accepted; effectiveness remains unassessed."
			}
			break
		}
		readSkill := !p.newCapability || strings.Contains(request.Messages[lastUser].Content, "Use the adopted "+CapabilityName)
		steps := 1
		if readSkill {
			steps = 2
		}
		if results < steps {
			name, arguments := "skill", `{"skill":"research"}`
			if p.newCapability && readSkill {
				arguments = `{"skill":"` + CapabilityName + `"}`
			}
			if !readSkill || results == 1 {
				name, arguments = "read_file", `{"path":"config.json"}`
			}
			message["tool_calls"] = []any{map[string]any{"index": 0, "id": fmt.Sprintf("native-%s-%d", name, lastUser), "type": "function", "function": map[string]string{"name": name, "arguments": arguments}}}
			finish = "tool_calls"
		} else {
			message["content"] = "Inspected the complete local JSON input."
		}
	case "extract":
		var input harness.SkillLearningInput
		if json.Unmarshal([]byte(request.Messages[1].Content), &input) != nil {
			fail("native extractor did not receive its actual bounded view")
			return
		}
		refs, users, reads := []string{}, 0, 0
		for _, ref := range input.References {
			if ref.Kind == "user" {
				users++
				refs = append(refs, ref.ID)
			}
			if ref.Kind == "skill_read" {
				reads++
			}
			if ref.Kind == "tool_observation" && ref.ToolName == "read_file" && ref.State == "completed" && !ref.Truncated {
				refs = append(refs, ref.ID)
			}
		}
		wantReads := 2
		wantUsers, wantRefs := 2, 4
		outcome := "observed_execution"
		if p.resolution {
			wantReads, wantRefs = 1, 3
			if users == 1 {
				wantReads, wantUsers, wantRefs, outcome = 0, 1, 1, "unresolved"
			}
		}
		if p.delegated {
			wantReads, wantUsers, wantRefs = 1, 1, 2
			if strings.Contains(request.Messages[1].Content, "DELEGATED_ONLY_NATIVE_INPUT") || !strings.Contains(request.Messages[0].Content, "delegated runs") {
				fail("delegated extractor exposed private instructions or lost role policy")
				return
			}
		}
		if p.newCapability {
			wantReads = 0
		}
		if users != wantUsers || reads != wantReads || len(refs) != wantRefs || !strings.Contains(request.Messages[0].Content, "untrusted historical data") {
			fail("native extractor lost multi-turn accepted input, actual skill reads or local receipts")
			return
		}
		if p.resolution && !strings.Contains(request.Messages[0].Content, "cite the earlier accepted user inputs being corrected") {
			fail("native extractor lost the source-covered correction contract")
			return
		}
		kind := "correction_chain"
		if p.delegated {
			kind = "proven_recipe"
		}
		answer = map[string]any{"episode_id": input.EpisodeID, "experiences": []any{map[string]any{
			"kind": kind, "goal": "Inspect complete JSON configuration", "applicability": "Local project JSON input",
			"problem": "Required fields need verification across the complete document", "resolution": "Read complete text and verify required fields",
			"steps": []string{"Read complete JSON text", "Verify required fields before reporting"}, "verification": "Completed local read of the complete document",
			"outcome": outcome, "references": refs,
		}}}
	case "consolidate":
		var input harness.SkillExperienceConsolidationInput
		if json.Unmarshal([]byte(request.Messages[1].Content), &input) != nil || input.Seed.ID == "" || input.Patterns == nil {
			fail("native consolidator did not receive its actual host shortlist")
			return
		}
		relation, pattern := "distinct", ""
		if len(input.Patterns) != 0 {
			relation, pattern = "equivalent", input.Patterns[0].ID
		}
		answer = map[string]any{"seed_id": input.Seed.ID, "relation": relation, "pattern_id": pattern, "reason": "Same complete-document procedure and local environment"}
	case "match":
		var input harness.SkillExperienceMatchInput
		if json.Unmarshal([]byte(request.Messages[1].Content), &input) != nil || !input.InventoryComplete || len(input.Skills) != 1 ||
			input.Skills[0].Name != "research" || input.Skills[0].ActuallyRead != !p.newCapability || input.Skills[0].CurrentVersionRead != !p.newCapability {
			fail("native matcher lost exact authorized inventory or current-version read attribution")
			return
		}
		answer = map[string]any{"pattern_id": input.PatternID, "revision": input.Revision, "relation": "body_gap", "skill_id": input.Skills[0].ID,
			"reason": "Applicable source research lacks the full-document verification instruction", "suggested_name": ""}
		if p.newCapability {
			if !strings.Contains(request.Messages[0].Content, "suggested_name") {
				fail("native matcher lost the readable capability-name contract")
				return
			}
			answer = map[string]any{"pattern_id": input.PatternID, "revision": input.Revision, "relation": "new_capability", "skill_id": "",
				"reason": "Complete local JSON verification is not covered by the authorized source-research skill", "suggested_name": CapabilityName}
		}
	case "generate":
		var input struct {
			SkillName   string `json:"skill_name"`
			BaseVersion string `json:"base_version"`
			BaseFiles   map[string]struct {
				Text string `json:"text"`
			} `json:"base_files"`
			Evidence []harness.SkillGenerationEvidence `json:"evidence"`
		}
		if json.Unmarshal([]byte(request.Messages[1].Content), &input) != nil || len(input.Evidence) == 0 || !strings.Contains(request.Messages[0].Content, "instruction-only historical-experience") ||
			!p.newCapability && (input.SkillName != "research" || len(input.BaseVersion) != 64 || len(input.BaseFiles) != 2) ||
			p.newCapability && (input.SkillName != CapabilityName || input.BaseVersion != "" || len(input.BaseFiles) != 0 || !strings.Contains(request.Messages[0].Content, "Create exactly one file, SKILL.md")) {
			fail("native generator lost automatic evidence, exact base or instruction-only boundary")
			return
		}
		files := make(map[string]string, len(input.BaseFiles))
		for path, file := range input.BaseFiles {
			files[path] = file.Text
		}
		files["SKILL.md"] += Instruction + "\n"
		if p.newCapability {
			files["SKILL.md"] = "---\nname: " + CapabilityName + "\ndescription: Verify required fields in complete local JSON documents; not bibliographic source research\n---\n" + Instruction + "\n"
		}
		answer = map[string]any{"skill_name": input.SkillName, "base_version": input.BaseVersion, "reason": "Add complete-document verification", "files": files}
	case "security", "quality":
		var input struct {
			BaseVersion      string `json:"base_version"`
			CandidateVersion string `json:"candidate_version"`
		}
		if json.Unmarshal([]byte(request.Messages[1].Content), &input) != nil || len(input.CandidateVersion) != 64 ||
			!p.newCapability && len(input.BaseVersion) != 64 || p.newCapability && input.BaseVersion != "" {
			fail("native content review lost pinned versions")
			return
		}
		answer = map[string]any{"base_version": input.BaseVersion, "candidate_version": input.CandidateVersion, "passed": true, "reason": "Fixture content review passed"}
	case "behavior":
		if len(request.Tools) == 0 || len(request.Messages) != 2 || !strings.Contains(request.Messages[0].Content, "isolated fixtures") {
			fail("behavior adapter did not construct a fresh isolated variant")
			return
		}
		message["content"] = "retained"
		if request.Messages[1].Content == "Inspect the complete JSON document and report completion." {
			message["content"] = "not-verified"
			if strings.Contains(request.Messages[0].Content, Instruction) {
				message["content"] = "verified"
			}
		}
	default:
		fail("unexpected model route in native skill pipeline")
		return
	}
	if answer != nil {
		data, err := json.Marshal(answer)
		if err != nil {
			fail("cannot encode fixture response")
			return
		}
		message["content"] = string(data)
	}
	usage := map[string]int{"prompt_tokens": 100, "completion_tokens": 25, "total_tokens": 125}
	_ = modelstreamtest.WriteCompletion(w, map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": finish}}, "usage": usage})
}
