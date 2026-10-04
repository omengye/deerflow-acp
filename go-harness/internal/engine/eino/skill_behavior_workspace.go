package eino

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/cloudwego/eino/schema"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillbehavior"
)

type behaviorWorkspace struct {
	directory         string
	root              *os.Root
	identity          os.FileInfo
	files             map[string][]byte
	fixtures          map[string]harness.SkillBehaviorFixture
	positions, counts map[string]int
	written           int
}

func validateBehaviorPackage(files map[string][]byte) error {
	if len(files) > 256 {
		return fmt.Errorf("%w: behavior package exceeds bounds", harness.ErrInvalidInput)
	}
	total := 0
	for name, data := range files {
		if !skillbehavior.ValidPath(name) || !skillbehavior.Text(string(data), 512<<10) {
			return fmt.Errorf("%w: behavior package has invalid file", harness.ErrInvalidInput)
		}
		total += len(data)
	}
	if total > 2<<20 {
		return fmt.Errorf("%w: behavior package exceeds bounds", harness.ErrInvalidInput)
	}
	if len(files) > 0 && len(files["SKILL.md"]) == 0 {
		return fmt.Errorf("%w: behavior package has no SKILL.md", harness.ErrInvalidInput)
	}
	return nil
}
func newBehaviorWorkspace(files map[string][]byte, c harness.SkillBehaviorCase) (*behaviorWorkspace, error) {
	directory, err := os.MkdirTemp("", "deerflow-skill-behavior-")
	if err != nil {
		return nil, err
	}
	w := &behaviorWorkspace{directory: directory, files: map[string][]byte{}, fixtures: map[string]harness.SkillBehaviorFixture{}, positions: map[string]int{}, counts: map[string]int{}}
	w.identity, err = os.Lstat(directory)
	if err == nil {
		w.root, err = os.OpenRoot(directory)
	}
	if err != nil {
		_ = w.close()
		return nil, err
	}
	for p, v := range files {
		w.files[p] = slices.Clone(v)
	}
	for _, f := range c.Fixtures {
		w.fixtures[f.Name] = f
	}
	for p, v := range c.Files {
		if err = w.write(p, v); err != nil {
			_ = w.close()
			return nil, err
		}
	}
	w.written = 0
	return w, nil
}
func (w *behaviorWorkspace) close() error {
	var closeErr error
	if w.root != nil {
		closeErr = w.root.Close()
	}
	// This path was allocated by MkdirTemp. Recheck its identity before removing
	// the owned tree; never follow a replaced cleanup target outside this case.
	current, err := os.Lstat(w.directory)
	if os.IsNotExist(err) {
		return closeErr
	}
	if err != nil {
		return errors.Join(closeErr, err)
	}
	if w.identity == nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(current, w.identity) {
		return errors.Join(closeErr, errors.New("behavior cleanup identity changed"))
	}
	return errors.Join(closeErr, os.RemoveAll(w.directory))
}
func stringParam() harness.SkillBehaviorParameter {
	return harness.SkillBehaviorParameter{Type: "string", Required: true}
}
func builtinBehaviorParameters(name string) map[string]harness.SkillBehaviorParameter {
	switch name {
	case "read_skill", "read_file":
		return map[string]harness.SkillBehaviorParameter{"path": stringParam()}
	case "write_file":
		return map[string]harness.SkillBehaviorParameter{"path": stringParam(), "content": stringParam()}
	default:
		return map[string]harness.SkillBehaviorParameter{}
	}
}
func behaviorInfo(name, description string, parameters map[string]harness.SkillBehaviorParameter) *schema.ToolInfo {
	params := map[string]*schema.ParameterInfo{}
	for name, p := range parameters {
		params[name] = &schema.ParameterInfo{Type: schema.DataType(p.Type), Desc: p.Description, Required: p.Required}
	}
	return &schema.ToolInfo{Name: name, Desc: description, ParamsOneOf: schema.NewParamsOneOfByParams(params)}
}
func (w *behaviorWorkspace) infos() []*schema.ToolInfo {
	infos := []*schema.ToolInfo{}
	for _, name := range []string{"read_skill", "read_file", "write_file", "list_files"} {
		infos = append(infos, behaviorInfo(name, map[string]string{"read_skill": "Read a named immutable skill package file.", "read_file": "Read a UTF-8 task fixture file using its relative path.", "write_file": "Write a UTF-8 task fixture file using its relative path. Up to 64 KiB.", "list_files": "List task fixture file paths."}[name], builtinBehaviorParameters(name)))
	}
	names := []string{}
	for name := range w.fixtures {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		f := w.fixtures[name]
		infos = append(infos, behaviorInfo(f.Name, f.Description, f.Parameters))
	}
	return infos
}
func (w *behaviorWorkspace) invoke(ctx context.Context, name, raw string) (string, string) {
	if ctx.Err() != nil {
		return "", behaviorErrorCode(ctx.Err())
	}
	fixture, exists := w.fixtures[name]
	if !exists && !skillbehavior.Builtin(name) {
		return "", "unknown_tool"
	}
	parameters := fixture.Parameters
	if skillbehavior.Builtin(name) {
		parameters = builtinBehaviorParameters(name)
	}
	args, err := skillbehavior.Arguments([]byte(raw), parameters)
	if err != nil {
		return "", "invalid_tool_arguments"
	}
	if exists {
		position := w.positions[name]
		if position >= len(fixture.Exchanges) {
			return "", "fixture_mismatch"
		}
		expected, err := skillbehavior.Arguments(fixture.Exchanges[position].Arguments, parameters)
		if err != nil || string(args) != string(expected) {
			return "", "fixture_mismatch"
		}
		w.positions[name]++
		w.counts[name]++
		return fixture.Exchanges[position].Output, ""
	}
	var input map[string]string
	if json.Unmarshal(args, &input) != nil {
		return "", "invalid_tool_arguments"
	}
	w.counts[name]++
	if name != "list_files" && !skillbehavior.ValidPath(input["path"]) {
		return "", "invalid_tool_arguments"
	}
	switch name {
	case "read_skill":
		value, ok := w.files[input["path"]]
		if !ok {
			return "", "tool_error"
		}
		if len(value) > 65536 {
			return "", "output_limit"
		}
		return string(value), ""
	case "read_file":
		value, err := w.read(input["path"])
		if err != nil {
			return "", "tool_error"
		}
		return value, ""
	case "write_file":
		if err := w.write(input["path"], input["content"]); err != nil {
			return "", "tool_error"
		}
		return "File written.", ""
	case "list_files":
		names, err := w.list()
		if err != nil {
			return "", "tool_error"
		}
		encoded, _ := json.Marshal(names)
		return string(encoded), ""
	}
	return "", "unknown_tool"
}
func (w *behaviorWorkspace) read(path string) (string, error) {
	f, err := w.root.Open(filepath.FromSlash(path))
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("fixture is not a file")
	}
	data, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil {
		return "", err
	}
	if !skillbehavior.Text(string(data), 65536) {
		return "", errors.New("fixture file exceeds text bounds")
	}
	return string(data), nil
}
func (w *behaviorWorkspace) write(path, body string) error {
	if !skillbehavior.ValidPath(path) || !skillbehavior.Text(body, 65536) || w.written+len(body) > 512<<10 {
		return errors.New("fixture write exceeds bounds")
	}
	native := filepath.FromSlash(path)
	if err := w.root.MkdirAll(filepath.Dir(native), 0700); err != nil {
		return err
	}
	f, err := w.root.OpenFile(native, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	_, writeErr := io.WriteString(f, body)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	w.written += len(body)
	return nil
}
func (w *behaviorWorkspace) list() ([]string, error) {
	files := []string{}
	err := fs.WalkDir(w.root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("fixture contains link")
		}
		if !d.IsDir() {
			files = append(files, path)
		}
		if len(files) > 256 {
			return errors.New("fixture contains too many files")
		}
		return nil
	})
	slices.Sort(files)
	return files, err
}
func (w *behaviorWorkspace) check(a harness.SkillBehaviorAssertion, output string) (harness.SkillBehaviorCheck, error) {
	result := harness.SkillBehaviorCheck{ID: a.ID}
	actual := ""
	switch a.Kind {
	case "output_equals":
		actual = output
		result.Passed = actual == a.Expected
	case "output_contains":
		actual = output
		result.Passed = strings.Contains(actual, a.Expected)
	case "file_equals":
		value, err := w.read(a.Path)
		if os.IsNotExist(err) {
			actual = "file:absent"
			break
		}
		if err != nil {
			return result, err
		}
		actual = value
		result.Passed = value == a.Expected
	case "file_absent":
		_, err := w.root.Lstat(filepath.FromSlash(a.Path))
		if os.IsNotExist(err) {
			actual = "file:absent"
			result.Passed = true
		} else if err != nil {
			return result, err
		} else {
			actual = "file:present"
		}
	case "tool_calls":
		actual = strconv.Itoa(w.counts[a.Tool])
		result.Passed = w.counts[a.Tool] == a.Count
	default:
		return result, errors.New("unknown behavior assertion")
	}
	result.ActualDigest = skillbehavior.Hash([]byte(actual))
	return result, nil
}
