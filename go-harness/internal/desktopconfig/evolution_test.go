package desktopconfig

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func reviewDocument(t *testing.T) (Options, map[string]any) {
	t.Helper()
	options := setup(t)
	run(t, options, "init", nil)
	doc := run(t, options, "snapshot", nil)
	root := str(readObject(doc, "paths"), "skills", "")
	if err := os.MkdirAll(filepath.Join(root, "custom"), 0700); err != nil {
		t.Fatal(err)
	}
	review := readObject(doc, "skill_evolution")
	review["enabled"], review["moderation_model_name"], review["evaluation_model_name"] = true, "first", "second"
	review["validation"] = map[string]any{"timeout_seconds": 30, "max_input_bytes": 65536, "max_output_tokens": 512}
	return options, doc
}

func TestReviewConfigValidateSaveAndReload(t *testing.T) {
	options, doc := reviewDocument(t)
	before, err := os.ReadFile(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	if run(t, options, "validate", doc)["valid"] != true {
		t.Fatal("review config failed validation")
	}
	after, err := os.ReadFile(options.Config)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("validate wrote the config", err)
	}
	saved := run(t, options, "save", doc)
	// A fresh snapshot and the save response must both remain valid; model
	// credentials are restored by their identity, never by review model order.
	run(t, options, "save", saved)
	run(t, options, "validate", run(t, options, "snapshot", nil))
	raw, err := readYAML(options.Config)
	if err != nil {
		t.Fatal(err)
	}
	review := readObject(raw, "skill_evolution")
	if review["enabled"] != true || review["moderation_model_name"] != "first" || review["evaluation_model_name"] != "second" || readObject(review, "validation")["max_output_tokens"] != 512 {
		t.Fatalf("review settings lost: %+v", review)
	}
	for _, name := range []string{"first", "second"} {
		if modelByName(t, raw, name)["api_key"] != name+"-private-key" {
			t.Fatalf("%s credential changed", name)
		}
	}
}

func TestReviewConfigRejectsIneffectiveSettingsWithoutWriting(t *testing.T) {
	for _, mode := range []string{"unknown-model", "automatic", "discovery", "fail-open", "skills-disabled", "missing-external-custom", "file-custom", "missing-builtin"} {
		t.Run(mode, func(t *testing.T) {
			options, doc := reviewDocument(t)
			review := readObject(doc, "skill_evolution")
			switch mode {
			case "unknown-model":
				review["moderation_model_name"] = "missing"
			case "automatic":
				review["mode"] = "auto_patch"
			case "discovery":
				review["discovery"] = map[string]any{"enabled": true}
			case "fail-open":
				review["security_fail_closed"] = false
			case "skills-disabled":
				doc["skills_enabled"] = false
			case "missing-builtin":
				review["allow_builtin_overlays"] = true
			case "missing-external-custom":
				// Portable custom stores are repaired automatically; external
				// import roots still require an explicitly prepared directory.
				readObject(doc, "skill_sources")["import_path"] = t.TempDir()
			case "file-custom":
				path := filepath.Join(str(readObject(doc, "paths"), "skills", ""), "custom")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("not a directory"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(options.Config)
			if err != nil {
				t.Fatal(err)
			}
			for _, operation := range []string{"validate", "save"} {
				field := "skill_evolution"
				if mode == "file-custom" {
					field = "skills"
				}
				if _, err := Run(options, operation, mustJSON(t, doc)); err == nil || !strings.Contains(err.Error(), field) {
					t.Fatalf("%s accepted %s or lost field path: %v", operation, mode, err)
				}
			}
			after, err := os.ReadFile(options.Config)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("invalid review config changed disk", err)
			}
		})
	}
}
