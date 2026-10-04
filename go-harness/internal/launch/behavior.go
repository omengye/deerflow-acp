package launch

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillbehavior"
)

// LoadBehaviorSuite is shared by save/validate and launch. It reads an explicit
// host file once; later worker calls use this immutable snapshot. The candidate
// package is never consulted for its own fixtures or expected answers.
func LoadBehaviorSuite(configPath, suitePath string, destinations []string) (*harness.SkillBehaviorOptions, error) {
	if strings.TrimSpace(suitePath) == "" || len(suitePath) > 4096 || strings.ContainsRune(suitePath, 0) {
		return nil, fmt.Errorf("skill_evolution.behavior.suite_path: requires a bounded host suite path")
	}
	path := resolveConfigPath(configPath, suitePath)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > skillbehavior.MaxSuiteBytes {
		return nil, fmt.Errorf("skill_evolution.behavior.suite_path: requires a regular suite file of at most 1 MiB")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("skill_evolution.behavior.suite_path: cannot open host suite file")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
		return nil, fmt.Errorf("skill_evolution.behavior.suite_path: file changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, skillbehavior.MaxSuiteBytes+1))
	if err != nil || len(data) > skillbehavior.MaxSuiteBytes {
		return nil, fmt.Errorf("skill_evolution.behavior.suite_path: cannot read bounded suite file")
	}
	suite, err := skillbehavior.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("skill_evolution.behavior.suite_path: invalid host fixture suite: %w", err)
	}
	if !slices.Contains(destinations, suite.SourceID) {
		return nil, fmt.Errorf("skill_evolution.behavior.suite_path: suite target is not an authorized candidate source")
	}
	return &harness.SkillBehaviorOptions{Suites: []harness.SkillBehaviorSuite{suite}}, nil
}
