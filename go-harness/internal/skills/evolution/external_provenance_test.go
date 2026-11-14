package evolution

import "testing"

func TestLearnedCandidateExternalProvenanceMajority(t *testing.T) {
	for _, tc := range []struct {
		name       string
		influenced map[string]bool
		want       bool
	}{
		{"no evidence", map[string]bool{}, false},
		{"all local", map[string]bool{"a": false, "b": false}, false},
		{"half external", map[string]bool{"a": true, "b": false}, false},
		{"mostly external", map[string]bool{"a": true, "b": true, "c": false}, true},
		{"all external", map[string]bool{"a": true, "b": true}, true},
	} {
		if got := mostlyExternal(tc.influenced); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}
