package eino

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestPermissionResumeAcceptsExactTrustedBackgroundJSON(t *testing.T) {
	// This version is deliberately above float64's exact integer range.
	want := PermissionResume{IntentID: "intent/a", IntentVersion: 9007199254740993, GrantID: "grant/a"}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.UseNumber()
	var native any
	if err = decoder.Decode(&native); err != nil {
		t.Fatal(err)
	}
	hooks := &ExecutionHooks{Targets: map[string]PermissionResume{"native/a": want}}
	for _, input := range []any{want, native} {
		got, decodeErr := decodePermissionResume(input, hooks)
		if decodeErr != nil || got != want {
			t.Fatalf("resume data: got=%+v want=%+v err=%v", got, want, decodeErr)
		}
	}
}

func TestPermissionResumeRejectsUntrustedOrAmbiguousBackgroundJSON(t *testing.T) {
	want := PermissionResume{IntentID: "intent/a", IntentVersion: 7, GrantID: "grant/a"}
	hooks := &ExecutionHooks{Targets: map[string]PermissionResume{"native/a": want}}
	valid := func() map[string]any {
		return map[string]any{"IntentID": want.IntentID, "IntentVersion": json.Number("7"), "GrantID": want.GrantID}
	}
	for name, mutate := range map[string]func(map[string]any){
		"extra key":      func(m map[string]any) { m["Allow"] = true },
		"missing key":    func(m map[string]any) { delete(m, "IntentVersion") },
		"wrong case":     func(m map[string]any) { delete(m, "IntentVersion"); m["intentversion"] = json.Number("7") },
		"float":          func(m map[string]any) { m["IntentVersion"] = float64(7) },
		"integer":        func(m map[string]any) { m["IntentVersion"] = int64(7) },
		"string":         func(m map[string]any) { m["IntentVersion"] = "7" },
		"fraction":       func(m map[string]any) { m["IntentVersion"] = json.Number("7.0") },
		"exponent":       func(m map[string]any) { m["IntentVersion"] = json.Number("7e0") },
		"overflow":       func(m map[string]any) { m["IntentVersion"] = json.Number("9223372036854775808") },
		"zero":           func(m map[string]any) { m["IntentVersion"] = json.Number("0") },
		"negative":       func(m map[string]any) { m["IntentVersion"] = json.Number("-1") },
		"stale version":  func(m map[string]any) { m["IntentVersion"] = json.Number("8") },
		"crossed intent": func(m map[string]any) { m["IntentID"] = "intent/b" },
		"crossed grant":  func(m map[string]any) { m["GrantID"] = "grant/b" },
		"empty grant":    func(m map[string]any) { m["GrantID"] = "" },
		"nested grant":   func(m map[string]any) { m["GrantID"] = map[string]any{"GrantID": want.GrantID} },
	} {
		t.Run(name, func(t *testing.T) {
			input := valid()
			mutate(input)
			if _, err := decodePermissionResume(input, hooks); !errors.Is(err, harness.ErrInvalidInput) {
				t.Fatalf("invalid resume data accepted: %v", err)
			}
		})
	}
	for _, input := range []any{nil, "grant/a", []any{valid()}, map[string]any{"native/a": valid()}} {
		if _, err := decodePermissionResume(input, hooks); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatalf("arbitrary data accepted: %T %v", input, err)
		}
	}
	for _, missing := range []*ExecutionHooks{nil, {}, {Targets: map[string]PermissionResume{}}} {
		if _, err := decodePermissionResume(valid(), missing); !errors.Is(err, harness.ErrInvalidInput) {
			t.Fatalf("untrusted JSON resume accepted: %v", err)
		}
	}
}
