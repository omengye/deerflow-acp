// Package skillbehavior validates host-owned behavioral fixtures. It never
// accepts executable assertions, discovers user files or calls a provider.
package skillbehavior

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

const Version = "skill-behavior-v1"
const MaxSuiteBytes = 1 << 20

var identifier = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)

func invalid(message string) error {
	return fmt.Errorf("%w: behavior %s", harness.ErrInvalidInput, message)
}
func Text(s string, max int) bool {
	return len(s) <= max && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}
func Builtin(name string) bool {
	return name == "read_skill" || name == "read_file" || name == "write_file" || name == "list_files"
}

// ValidPath applies one portable contract on Windows and Unix; fixture files
// cannot become drive paths, devices, alternate streams or traversal paths.
func ValidPath(p string) bool {
	if p == "" || len(p) > 256 || !Text(p, 256) || strings.ContainsAny(p, `\:`) || strings.HasPrefix(p, "/") || path.Clean(p) != p {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "." || part == ".." || strings.TrimRight(part, " .") != part || strings.ContainsAny(part, `<>"|?*`) {
			return false
		}
		stem := strings.TrimRight(strings.ToUpper(strings.SplitN(part, ".", 2)[0]), " ")
		devicePort := false
		if strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT") {
			tail := strings.TrimPrefix(strings.TrimPrefix(stem, "COM"), "LPT")
			devicePort = len(tail) == 1 && tail[0] >= '1' && tail[0] <= '9' || tail == "¹" || tail == "²" || tail == "³"
		}
		if stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" || stem == "CONIN$" || stem == "CONOUT$" || devicePort {
			return false
		}
		for _, r := range part {
			if r < 32 {
				return false
			}
		}
	}
	return true
}

// CanonicalJSON rejects duplicate keys at every depth, excessive nesting,
// nonfinite/ambiguous numeric coercions and trailing values.
func CanonicalJSON(data []byte) ([]byte, error) {
	if len(data) > MaxSuiteBytes || !utf8.Valid(data) {
		return nil, invalid("JSON exceeds text bounds")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	value, err := readJSON(dec, 0)
	if err != nil {
		return nil, err
	}
	if _, err = dec.Token(); err != io.EOF {
		return nil, invalid("trailing JSON")
	}
	result, err := json.Marshal(value)
	if err != nil {
		return nil, invalid("JSON cannot be encoded")
	}
	return result, nil
}
func readJSON(dec *json.Decoder, depth int) (any, error) {
	if depth > 16 {
		return nil, invalid("JSON nesting exceeds bounds")
	}
	token, err := dec.Token()
	if err != nil {
		return nil, invalid("invalid JSON")
	}
	switch v := token.(type) {
	case json.Delim:
		switch v {
		case '{':
			object := map[string]any{}
			for dec.More() {
				key, e := dec.Token()
				if e != nil {
					return nil, invalid("invalid JSON key")
				}
				name, ok := key.(string)
				if !ok || !Text(name, 512) {
					return nil, invalid("invalid JSON key")
				}
				if _, exists := object[name]; exists {
					return nil, invalid("duplicate JSON key")
				}
				item, e := readJSON(dec, depth+1)
				if e != nil {
					return nil, e
				}
				object[name] = item
			}
			end, e := dec.Token()
			if e != nil || end != json.Delim('}') {
				return nil, invalid("invalid JSON object")
			}
			return object, nil
		case '[':
			items := []any{}
			for dec.More() {
				item, e := readJSON(dec, depth+1)
				if e != nil {
					return nil, e
				}
				items = append(items, item)
			}
			end, e := dec.Token()
			if e != nil || end != json.Delim(']') {
				return nil, invalid("invalid JSON array")
			}
			return items, nil
		}
		return nil, invalid("invalid JSON delimiter")
	case string:
		if !Text(v, MaxSuiteBytes) {
			return nil, invalid("invalid JSON text")
		}
	case json.Number:
		f, e := v.Float64()
		if e != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return nil, invalid("invalid JSON number")
		}
	}
	return token, nil
}

func Parse(data []byte) (harness.SkillBehaviorSuite, error) {
	var s harness.SkillBehaviorSuite
	canonical, err := CanonicalJSON(data)
	if err != nil {
		return s, err
	}
	// Null is not an omitted field. Mandatory values also have semantic bounds.
	var tree any
	_ = json.Unmarshal(canonical, &tree)
	if containsNull(tree) {
		return s, invalid("explicit null is not supported")
	}
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&s); err != nil {
		return s, invalid("suite fields or types are invalid")
	}
	return s, Validate(s)
}
func containsNull(v any) bool {
	if v == nil {
		return true
	}
	switch x := v.(type) {
	case map[string]any:
		for _, v := range x {
			if containsNull(v) {
				return true
			}
		}
	case []any:
		for _, v := range x {
			if containsNull(v) {
				return true
			}
		}
	}
	return false
}
func Digest(s harness.SkillBehaviorSuite) (string, error) {
	if err := Validate(s); err != nil {
		return "", err
	}
	data, _ := json.Marshal(s)
	data, err := CanonicalJSON(data)
	if err != nil {
		return "", err
	}
	return Hash(data), nil
}
func Hash(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }

func Validate(s harness.SkillBehaviorSuite) error {
	if s.Version != Version || !identifier.MatchString(s.ID) || !harness.ValidSkillName(s.SourceID) || !harness.ValidSkillName(s.Name) || len(s.Cases) < 1 || len(s.Cases) > 16 {
		return invalid("suite identity or case count is invalid")
	}
	seen := map[string]bool{}
	targeted := false
	for _, c := range s.Cases {
		if seen[c.ID] {
			return invalid("duplicate case id")
		}
		seen[c.ID] = true
		if c.Kind == "targeted" {
			targeted = true
		}
		if err := ValidateCase(c); err != nil {
			return err
		}
	}
	if !targeted {
		return invalid("suite requires a targeted case")
	}
	data, err := json.Marshal(s)
	if err != nil || len(data) > MaxSuiteBytes {
		return invalid("suite exceeds bounds")
	}
	return nil
}
func ValidateCase(c harness.SkillBehaviorCase) error {
	if !identifier.MatchString(c.ID) || c.Kind != "targeted" && c.Kind != "regression" || strings.TrimSpace(c.Prompt) == "" || !Text(c.Prompt, 32768) || c.Repetitions < 1 || c.Repetitions > 3 || len(c.Files) > 64 || len(c.Fixtures) > 8 || len(c.Assertions) < 1 || len(c.Assertions) > 32 {
		return invalid("case identity or limits are invalid")
	}
	total := 0
	paths := map[string]bool{}
	for p, v := range c.Files {
		if !ValidPath(p) || !Text(v, 65536) || paths[strings.ToLower(p)] {
			return invalid("fixture file is invalid")
		}
		paths[strings.ToLower(p)] = true
		total += len(v)
	}
	if total > 256<<10 {
		return invalid("fixture files exceed bounds")
	}
	for p := range c.Files {
		for parent := path.Dir(p); parent != "."; parent = path.Dir(parent) {
			if paths[strings.ToLower(parent)] {
				return invalid("fixture file/directory collision")
			}
		}
	}
	names := map[string]bool{}
	for _, f := range c.Fixtures {
		if !identifier.MatchString(f.Name) || Builtin(f.Name) || names[f.Name] || !Text(f.Description, 1000) || strings.TrimSpace(f.Description) == "" || len(f.Parameters) > 16 || len(f.Exchanges) < 1 || len(f.Exchanges) > 32 {
			return invalid("fixture tool is invalid")
		}
		names[f.Name] = true
		for name, p := range f.Parameters {
			if !identifier.MatchString(name) || !Text(p.Description, 1000) || p.Type != "string" && p.Type != "number" && p.Type != "integer" && p.Type != "boolean" {
				return invalid("fixture parameter is invalid")
			}
		}
		for _, e := range f.Exchanges {
			if !Text(e.Output, 32768) {
				return invalid("fixture output exceeds bounds")
			}
			if _, err := Arguments(e.Arguments, f.Parameters); err != nil {
				return err
			}
		}
	}
	ids := map[string]bool{}
	for _, a := range c.Assertions {
		if !identifier.MatchString(a.ID) || ids[a.ID] || !Text(a.Expected, 65536) {
			return invalid("assertion identity or value is invalid")
		}
		ids[a.ID] = true
		switch a.Kind {
		case "output_equals", "output_contains":
			if a.Path != "" || a.Tool != "" || a.Count != 0 || a.Expected == "" {
				return invalid("output assertion fields are invalid")
			}
		case "file_equals":
			if !ValidPath(a.Path) || a.Tool != "" || a.Count != 0 {
				return invalid("file assertion fields are invalid")
			}
		case "file_absent":
			if !ValidPath(a.Path) || a.Tool != "" || a.Expected != "" || a.Count != 0 {
				return invalid("absence assertion fields are invalid")
			}
		case "tool_calls":
			if a.Path != "" || a.Expected != "" || !Builtin(a.Tool) && !names[a.Tool] || a.Count < 0 || a.Count > 128 {
				return invalid("tool assertion fields are invalid")
			}
		default:
			return invalid("unknown assertion kind")
		}
	}
	return nil
}

// Arguments returns a canonical primitive parameter object. Host exchanges and
// actual model calls follow the same strict decoder before any fixture effect.
func Arguments(data []byte, parameters map[string]harness.SkillBehaviorParameter) ([]byte, error) {
	if len(data) > 65536 {
		return nil, invalid("tool arguments exceed bounds")
	}
	canonical, err := CanonicalJSON(data)
	if err != nil {
		return nil, err
	}
	var object map[string]json.RawMessage
	if len(canonical) == 0 || canonical[0] != '{' || json.Unmarshal(canonical, &object) != nil {
		return nil, invalid("tool arguments must be an object")
	}
	for name, p := range parameters {
		if _, ok := object[name]; !ok && p.Required {
			return nil, invalid("missing tool argument")
		}
	}
	for name, raw := range object {
		p, ok := parameters[name]
		if !ok || bytes.Equal(raw, []byte("null")) {
			return nil, invalid("unknown or null tool argument")
		}
		switch p.Type {
		case "string":
			var v string
			if json.Unmarshal(raw, &v) != nil || !Text(v, 65536) {
				return nil, invalid("invalid string argument")
			}
		case "boolean":
			var v bool
			if json.Unmarshal(raw, &v) != nil {
				return nil, invalid("invalid boolean argument")
			}
		case "number", "integer":
			var v json.Number
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.UseNumber()
			var value any
			if dec.Decode(&value) != nil {
				return nil, invalid("invalid numeric argument")
			}
			v, ok = value.(json.Number)
			if !ok {
				return nil, invalid("invalid numeric argument")
			}
			n, e := v.Float64()
			if e != nil || math.IsInf(n, 0) || p.Type == "integer" && n != math.Trunc(n) {
				return nil, invalid("invalid numeric argument")
			}
		default:
			return nil, invalid("unsupported parameter type")
		}
	}
	return canonical, nil
}
