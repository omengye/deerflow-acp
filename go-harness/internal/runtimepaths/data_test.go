package runtimepaths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPortableBindingAcrossLaunchers(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "user-data")
	old := filepath.Join(data, "config", "go-harness-state")
	canonical := filepath.Join(data, "data", "go-harness")
	if got, err := ResolvePortable(data, ""); err != nil || got != canonical {
		t.Fatalf("fresh: %s %v", got, err)
	}
	for _, dir := range []string{old, canonical} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "harness.db"), []byte("history"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ResolvePortable(data, ""); err == nil {
		t.Fatal("missing binding silently selected another existing database")
	}
	binding := filepath.Join(data, "config", "runtime-data.json")
	if err := os.WriteFile(binding, []byte(`{"data_dir":"config/go-harness-state"}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, requested := range []string{"", old} {
		if got, err := ResolvePortable(data, requested); err != nil || got != old {
			t.Fatalf("restore: %s %v", got, err)
		}
	}
	if _, err := ResolvePortable(data, canonical); err == nil {
		t.Fatal("environment/flag override switched database")
	}
	if err := os.Remove(filepath.Join(old, "harness.db")); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolvePortable(data, ""); err == nil {
		t.Fatal("missing bound database fell back to other store")
	}
}

func TestBindingValidationAndPortableMove(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "before")
	if err := os.MkdirAll(filepath.Join(data, "config", "state"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "config", "state", "harness.db"), []byte("history"), 0600); err != nil {
		t.Fatal(err)
	}
	binding := filepath.Join(data, "config", "runtime-data.json")
	for _, raw := range []string{`{}`, `{"data_dir":"../escape"}`, `{"data_dir":"/absolute"}`, `{"data_dir":"config/missing"}`, `{"data_dir":"config/state","extra":true}`, `{"data_dir":"config/state","data_dir":"config/state"}`, `{"DATA_DIR":"config/state"}`, `{"data_dir":"config/state"} {}`, strings.Repeat(" ", 8193)} {
		if err := os.WriteFile(binding, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ResolvePortable(data, ""); err == nil {
			t.Fatalf("accepted invalid binding: %.80s", raw)
		}
	}
	if err := os.WriteFile(binding, []byte(`{"data_dir":"config/state"}`), 0600); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(root, "after")
	if err := os.Rename(data, moved); err != nil {
		t.Fatal(err)
	}
	if got, err := ResolvePortable(moved, ""); err != nil || got != filepath.Join(moved, "config", "state") {
		t.Fatalf("moved profile: %s %v", got, err)
	}
}

func TestPortableDiscoveryAndStandaloneCompatibility(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "user-data", "config", "config.yaml")
	for _, portable := range []string{"", root} {
		got, err := UserData(config, portable, filepath.Join(root, "elsewhere", "daemon"))
		if err != nil || got != filepath.Join(root, "user-data") {
			t.Fatalf("profile: %s %v", got, err)
		}
	}
	if _, err := UserData(config, filepath.Join(root, "wrong"), "daemon"); err == nil {
		t.Fatal("inherited foreign profile was accepted")
	}
	t.Setenv("DEER_FLOW_PORTABLE_ROOT", "")
	standalone := filepath.Join(root, "config.yaml")
	if got, err := Resolve(standalone, ""); err != nil || got != filepath.Join(root, "go-harness-state") {
		t.Fatalf("standalone default: %s %v", got, err)
	}
	if got, err := Resolve(standalone, "explicit-state"); err != nil || got != "explicit-state" {
		t.Fatalf("standalone override: %s %v", got, err)
	}
}
