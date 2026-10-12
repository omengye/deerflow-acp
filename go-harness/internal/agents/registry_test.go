package agents

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func ptr[T any](v T) *T { return &v }

func TestUnlimitedRoleTimeoutStillInheritsParentLimit(t *testing.T) {
	for _, tc := range []struct{ role, parent, want float64 }{{0, 0, 0}, {0, 12, 12}, {259200, 0, 259200}, {259200, 12, 12}, {12, 259200, 12}} {
		role := researcher()
		role.TimeoutSeconds = ptr(tc.role)
		r, err := New(harness.SubagentsConfig{CustomAgents: map[string]harness.AgentConfig{"reader": role}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		spec, err := r.Resolve("reader", Parent{Model: "fixture", TimeoutSeconds: tc.parent})
		if err != nil || spec.TimeoutSeconds != tc.want {
			t.Fatal(spec, err)
		}
		if err := spec.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	r, err := New(harness.SubagentsConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := r.Resolve("general-purpose", Parent{Model: "fixture"})
	if err != nil || spec.TimeoutSeconds != 0 {
		t.Fatal("implicit role deadline", spec, err)
	}
}

func researcher() harness.AgentConfig {
	return harness.AgentConfig{Description: "Read evidence", SystemPrompt: "Return evidence and sources.", Model: "research-model"}
}

func TestOptionalRoleTurnLimitsInheritParentBounds(t *testing.T) {
	for _, tc := range []struct {
		name               string
		role, parent, want int
	}{
		{"unrestricted", 0, 0, 0},
		{"parent-bound", 0, 8, 8},
		{"role-bound", 12, 0, 12},
		{"narrower-parent", 12, 8, 8},
		{"narrower-role", 4, 8, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			role := researcher()
			role.MaxTurns = ptr(tc.role)
			r, err := New(harness.SubagentsConfig{CustomAgents: map[string]harness.AgentConfig{"reader": role}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			spec, err := r.Resolve("reader", Parent{Model: "fixture", MaxTurns: tc.parent})
			if err != nil || spec.MaxTurns != tc.want {
				t.Fatalf("spec=%+v err=%v", spec, err)
			}
			if err := spec.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
	r, err := New(harness.SubagentsConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := r.Resolve("general-purpose", Parent{Model: "fixture"})
	if err != nil || spec.MaxTurns != 0 {
		t.Fatalf("hidden default role cap: %+v err=%v", spec, err)
	}
}

func TestResolveNarrowsParentAndPinsIndependentSpec(t *testing.T) {
	role := researcher()
	role.Tools = []string{"web_search", "read_file", "write_file"}
	role.DisallowedTools = []string{"read_file"}
	role.Skills = []string{"research", "private"}
	role.ModelSettings.Temperature = ptr(0.2)
	config := harness.SubagentsConfig{TimeoutSeconds: ptr(900.0), MaxTurns: ptr(50), CustomAgents: map[string]harness.AgentConfig{"researcher": role}}
	r, err := New(config, map[string]bool{"research-model": true})
	if err != nil {
		t.Fatal(err)
	}
	parent := Parent{Model: "parent-model", Tools: harness.ToolPolicy{Allowlist: []string{"read_file", "web_search"}, Denylist: []string{"write_file"}}, Skills: []string{"research"}, MaxTurns: 8, TimeoutSeconds: 20}
	first, err := r.Resolve("researcher", parent)
	if err != nil {
		t.Fatal(err)
	}
	if first.Model != "research-model" || !first.Tools.Allows("web_search") || first.Tools.Allows("read_file") || first.Tools.Allows("write_file") {
		t.Fatalf("incorrect child authority: %+v", first)
	}
	if !reflect.DeepEqual(first.Skills, []string{"research"}) || first.MaxTurns != 8 || first.TimeoutSeconds != 20 || len(first.Version) != 64 {
		t.Fatalf("incorrect child bounds: %+v", first)
	}
	// Mutating deployment input, returned specs, or parent input cannot alter
	// another execution's pinned role recipe.
	role.Tools[0] = "delete_path"
	*role.ModelSettings.Temperature = 1.8
	config.CustomAgents["researcher"] = harness.AgentConfig{}
	first.Tools.Allowlist[0] = "delete_path"
	*first.ModelSettings.Temperature = 1.9
	first.Skills[0] = "private"
	second, err := r.Resolve("researcher", parent)
	if err != nil {
		t.Fatal(err)
	}
	if second.Version != first.Version || !second.Tools.Allows("web_search") || *second.ModelSettings.Temperature != 0.2 || second.Skills[0] != "research" {
		t.Fatalf("role mutated across executions: %+v", second)
	}
	parent.Tools.Allowlist[0] = "delete_path"
	if second.Tools.Allowlist[0] == "delete_path" {
		t.Fatal("returned policy aliases parent")
	}
	if _, err := r.Resolve("unknown", parent); err == nil {
		t.Fatal("unregistered role accepted")
	}
}

func TestNilAndEmptySelectionsRemainDistinct(t *testing.T) {
	for _, tc := range []struct {
		name         string
		role, parent []string
		wantNil      bool
		length       int
	}{
		{"inherit-unrestricted", nil, nil, true, 0},
		{"explicit-empty", []string{}, nil, false, 0},
		{"parent-empty", nil, []string{}, false, 0},
		{"inherit-parent", nil, []string{"read_file"}, false, 1},
		{"disjoint", []string{"write_file"}, []string{"read_file"}, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			role := researcher()
			role.Tools = tc.role
			role.Skills = tc.role
			r, err := New(harness.SubagentsConfig{CustomAgents: map[string]harness.AgentConfig{"reader": role}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			spec, err := r.Resolve("reader", Parent{Tools: harness.ToolPolicy{Allowlist: tc.parent}, Skills: tc.parent})
			if err != nil {
				t.Fatal(err)
			}
			for _, value := range [][]string{spec.Tools.Allowlist, spec.Skills} {
				if (value == nil) != tc.wantNil || len(value) != tc.length {
					t.Fatalf("selection=%#v", value)
				}
			}
		})
	}
}

func TestRoleVersionTracksEffectivePolicyAndCanonicalOrder(t *testing.T) {
	r, err := New(harness.SubagentsConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := r.Resolve("general-purpose", Parent{Model: "one", Tools: harness.ToolPolicy{Allowlist: []string{"read_file", "ls"}}})
	b, _ := r.Resolve("general-purpose", Parent{Model: "one", Tools: harness.ToolPolicy{Allowlist: []string{"ls", "read_file"}}})
	c, _ := r.Resolve("general-purpose", Parent{Model: "two", Tools: harness.ToolPolicy{Allowlist: []string{"ls", "read_file"}}})
	if a.Version != b.Version || a.Version == c.Version {
		t.Fatal("version does not describe effective canonical role")
	}
	command, _ := r.Resolve("bash", Parent{Model: "one"})
	if !command.Tools.Allows("bash") || command.Tools.Allows("write_file") {
		t.Fatal("builtin command role lost its tool restriction")
	}
	data, _ := json.Marshal(command)
	if strings.Contains(string(data), "APIKey") {
		t.Fatal("role recipe must not contain credentials")
	}
}

func TestConfigRejectsInvalidAuthorityAndBounds(t *testing.T) {
	tests := []struct {
		name   string
		change func(*harness.SubagentsConfig)
		path   string
	}{
		{"unknown-model", func(c *harness.SubagentsConfig) {
			r := c.CustomAgents["researcher"]
			r.Model = "missing"
			c.CustomAgents["researcher"] = r
		}, ".model"},
		{"negative-timeout", func(c *harness.SubagentsConfig) { c.TimeoutSeconds = ptr(-1.0) }, "timeout_seconds"},
		{"nan-timeout", func(c *harness.SubagentsConfig) { c.TimeoutSeconds = ptr(math.NaN()) }, "timeout_seconds"},
		{"too-deep", func(c *harness.SubagentsConfig) { c.MaxDepth = ptr(4) }, "max_depth"},
		{"zero-concurrency", func(c *harness.SubagentsConfig) { c.MaxConcurrent = ptr(0) }, "max_concurrent"},
		{"reserved-name", func(c *harness.SubagentsConfig) { c.CustomAgents["bash"] = researcher() }, "reserved"},
		{"unknown-builtin", func(c *harness.SubagentsConfig) { c.Agents = map[string]harness.AgentConfig{"typo": {}} }, "unknown builtin"},
		{"missing-instruction", func(c *harness.SubagentsConfig) {
			r := researcher()
			r.SystemPrompt = ""
			c.CustomAgents["researcher"] = r
		}, "system_prompt"},
		{"duplicate-tool", func(c *harness.SubagentsConfig) {
			r := researcher()
			r.Tools = []string{"read_file", "read_file"}
			c.CustomAgents["researcher"] = r
		}, "tools"},
		{"unknown-child", func(c *harness.SubagentsConfig) {
			r := researcher()
			r.DelegateTo = []string{"missing"}
			c.CustomAgents["researcher"] = r
		}, "unknown role"},
		{"cycle", func(c *harness.SubagentsConfig) {
			r := researcher()
			r.DelegateTo = []string{"general-purpose"}
			c.CustomAgents["researcher"] = r
			c.Agents = map[string]harness.AgentConfig{"general-purpose": {DelegateTo: []string{"researcher"}}}
		}, "cycle"},
		{"temperature", func(c *harness.SubagentsConfig) {
			r := researcher()
			r.ModelSettings.Temperature = ptr(math.Inf(1))
			c.CustomAgents["researcher"] = r
		}, "temperature"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := harness.SubagentsConfig{CustomAgents: map[string]harness.AgentConfig{"researcher": researcher()}}
			tc.change(&c)
			_, err := New(c, map[string]bool{"research-model": true})
			if err == nil || !strings.Contains(err.Error(), tc.path) {
				t.Fatalf("error=%v, want %q", err, tc.path)
			}
		})
	}
}

func TestDecodeConfigRejectsTyposAndPreservesExplicitSelections(t *testing.T) {
	for _, data := range []string{
		"max_dept: 2\n",
		"custom_agents:\n  reader:\n    description: reader\n    system_prompt: read\n    model_settings:\n      tempurature: 0.5\n",
		"enabled: true\n---\nenabled: false\n",
		"enabled: true\nenabled: false\n",
	} {
		if _, err := DecodeConfig([]byte(data), nil); err == nil {
			t.Fatalf("accepted invalid document: %s", data)
		}
	}
	c, err := DecodeConfig([]byte("enabled: false\nmax_auto_continuations: 0\ncustom_agents:\n  reader:\n    description: reader\n    system_prompt: read\n    skills: []\n    tools: null\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Enabled == nil || *c.Enabled || c.MaxAutoContinuations == nil || *c.MaxAutoContinuations != 0 || c.CustomAgents["reader"].Skills == nil || c.CustomAgents["reader"].Tools != nil {
		t.Fatalf("explicit values lost: %+v", c)
	}
}
