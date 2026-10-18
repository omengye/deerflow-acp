package skills

import (
	"strings"
	"testing"
)

func TestRuntimeManifestStrictSourceOnly(t *testing.T) {
	valid := `{"version":1,"python":{"min_version":"3.10","packages":["paramiko"],"entrypoints":["scripts/run.py"]}}`
	files := map[string][]byte{"scripts/run.py": []byte("print('ok')")}
	if m, declared, err := ParseRuntimeManifest(files); err != nil || declared || m.Python.MinVersion != "3.8" {
		t.Fatal(m, declared, err)
	}
	for _, tc := range []struct {
		name, value string
		valid       bool
	}{
		{"valid", valid, true},
		{"missing", `{}`, false}, {"null", `null`, false},
		{"duplicate", strings.Replace(valid, `"version":1`, `"version":2,"version":1`, 1), false},
		{"nested_duplicate", strings.Replace(valid, `"packages":`, `"packages":[],"packages":`, 1), false},
		{"unknown", strings.Replace(valid, `"version":1`, `"version":1,"install":"pip"`, 1), false},
		{"case_alias", strings.Replace(valid, `"version"`, `"Version"`, 1), false},
		{"null_python", `{"version":1,"python":null}`, false},
		{"null_packages", strings.Replace(valid, `["paramiko"]`, `null`, 1), false},
		{"package_url", strings.ReplaceAll(valid, "paramiko", "https://example.com/wheel"), false},
		{"package_command", strings.ReplaceAll(valid, "paramiko", "foo;whoami"), false},
		{"package_alias", strings.Replace(valid, `["paramiko"]`, `["a_b","a..b"]`, 1), false},
		{"old_version", strings.Replace(valid, "3.10", "3.7", 1), false},
		{"path_escape", strings.Replace(valid, "scripts/run.py", "../run.py", 1), false},
		{"missing_entry", strings.Replace(valid, "scripts/run.py", "absent.py", 1), false},
		{"trailing", valid + `{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files[RuntimeManifestFile] = []byte(tc.value)
			_, declared, err := ParseRuntimeManifest(files)
			if !declared || (err == nil) != tc.valid {
				t.Fatal(declared, err)
			}
		})
	}
}
