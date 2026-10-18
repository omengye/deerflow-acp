package skills

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
)

const RuntimeManifestFile = "skill-runtime.json"

type PythonRequirements struct {
	MinVersion  string   `json:"min_version"`
	Packages    []string `json:"packages"`
	Entrypoints []string `json:"entrypoints"`
}

type RuntimeManifest struct {
	Version int                `json:"version"`
	Python  PythonRequirements `json:"python"`
}

var distributionName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var distributionSeparator = regexp.MustCompile(`[-_.]+`)

// Dependencies are declarations, never install commands or import targets.
func ParseRuntimeManifest(files map[string][]byte) (RuntimeManifest, bool, error) {
	m := RuntimeManifest{Version: 1, Python: PythonRequirements{MinVersion: "3.8"}}
	b, present := files[RuntimeManifestFile]
	if !present {
		return m, false, nil
	}
	if len(b) > 16384 {
		return m, true, invalid("runtime manifest exceeds size limit")
	}
	value, err := parseEditJSON(b)
	if err != nil {
		return m, true, err
	}
	object, ok := value.(map[string]any)
	python, pythonOK := object["python"].(map[string]any)
	if !ok || !pythonOK || object["version"] == nil || python["min_version"] == nil || python["entrypoints"] == nil {
		return m, true, invalid("runtime manifest requires version and Python requirements")
	}
	for key, value := range object {
		if (key != "version" && key != "python") || value == nil {
			return m, true, invalid("invalid runtime manifest field")
		}
	}
	for key, value := range python {
		if (key != "min_version" && key != "packages" && key != "entrypoints") || value == nil {
			return m, true, invalid("invalid Python requirements field")
		}
	}
	m = RuntimeManifest{}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	var extra any
	if len(b) > 16384 || d.Decode(&m) != nil || d.Decode(&extra) != io.EOF || m.Version != 1 {
		return m, true, invalid("invalid skill-runtime.json")
	}
	if _, _, err := PythonVersion(m.Python.MinVersion); err != nil || len(m.Python.Packages) > 32 || len(m.Python.Entrypoints) == 0 || len(m.Python.Entrypoints) > 20 {
		return m, true, invalid("invalid Python runtime requirements")
	}
	seen := map[string]bool{}
	for _, p := range m.Python.Packages {
		key := strings.ToLower(distributionSeparator.ReplaceAllString(p, "-"))
		if !distributionName.MatchString(p) || seen[key] {
			return m, true, invalid("packages must be unique distribution names, not commands or URLs")
		}
		seen[key] = true
	}
	seen = map[string]bool{}
	for _, p := range m.Python.Entrypoints {
		_, exists := files[p]
		if !validRelative(p) || strings.ToLower(path.Ext(p)) != ".py" || !exists || seen[p] {
			return m, true, invalid("Python entrypoint must name a unique packaged .py source")
		}
		seen[p] = true
	}
	return m, true, nil
}

func PythonVersion(v string) (int, int, error) {
	p := strings.Split(v, ".")
	if len(p) != 2 {
		return 0, 0, fmt.Errorf("Python version requires major.minor")
	}
	a, e1 := strconv.Atoi(p[0])
	b, e2 := strconv.Atoi(p[1])
	if e1 != nil || e2 != nil || a != 3 || b < 8 || b > 99 || fmt.Sprintf("%d.%d", a, b) != v {
		return 0, 0, fmt.Errorf("Python 3.8 or newer is required")
	}
	return a, b, nil
}
