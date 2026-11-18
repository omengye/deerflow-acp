package memory

import (
	"encoding/json"
	"strings"
	"testing"
)

// Examples in the actual model instruction must be executable under the same
// host validation that will decide whether a live response can be published.
func TestMemoryModelFixExtractionGuidanceExamples(t *testing.T) {
	for _, personal := range []bool{true, false} {
		allowed := []ScopeKind{WorkspaceScope}
		if personal {
			allowed = append(allowed, GlobalScope)
		}
		prompt := ExtractionPrompt(allowed, personal)
		if !strings.Contains(prompt, "decoded user field") || !strings.Contains(prompt, "JSON string delimiters") {
			t.Fatal("transport JSON is not distinguished from a quote in the accepted user text")
		}
		var previous string
		examples := 0
		for _, line := range strings.Split(prompt, "\n") {
			if strings.HasPrefix(line, "Example input: ") {
				var envelope struct {
					User string `json:"user"`
				}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "Example input: ")), &envelope); err != nil {
					t.Fatal(err)
				}
				previous = envelope.User
			}
			if strings.HasPrefix(line, "Example output: ") {
				facts, rejected, err := ParseExtraction([]byte(strings.TrimPrefix(line, "Example output: ")), previous, allowed, personal)
				if err != nil || len(facts) != 1 || len(rejected) != 0 {
					t.Fatalf("guidance disagrees with host validation: %v %v", rejected, err)
				}
				examples++
			}
		}
		if examples < 4 {
			t.Fatalf("missing grounded field examples: %d", examples)
		}
	}
}
