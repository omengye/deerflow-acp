package skills

import (
	"bytes"
	"regexp"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

var reviewRules = []struct {
	code, message string
	pattern       *regexp.Regexp
}{
	{"instruction_override", "Contains language that may ask to override surrounding instructions; review its intended scope.", regexp.MustCompile(`(?i)(ignore\s+(all\s+)?(previous|prior|system)\s+instructions|忽略.{0,8}(系统|之前|以上).{0,4}指令)`)},
	{"remote_shell", "Contains a download-to-shell pattern; installation does not authorize its execution.", regexp.MustCompile(`(?i)(curl|wget)[^\r\n]{0,512}\|\s*(ba)?sh\b`)},
	{"possible_credential", "Contains a credential-like literal assignment; review before enabling or sharing this snapshot.", regexp.MustCompile(`(?i)(api[_-]?key|access[_-]?token|private[_-]?key|password)[ \t]*[:=][ \t]*["'][^"'\r\n]{8,}["']`)},
}

// scan emits bounded deterministic review hints. It does not classify arbitrary
// prose/code as safe, remove secrets, execute scripts, or fetch remote content.
func scan(files map[string][]byte, manifest []harness.SkillFileInfo) []harness.SkillFinding {
	var findings []harness.SkillFinding
	for _, entry := range manifest {
		data := files[entry.Path]
		if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			findings = append(findings, harness.SkillFinding{Code: "binary_unscanned", Path: entry.Path, Message: "Binary support file retained without content scanning; text tool will not render it."})
			continue
		}
		for _, rule := range reviewRules {
			if rule.pattern.Match(data) {
				findings = append(findings, harness.SkillFinding{Code: rule.code, Path: entry.Path, Message: rule.message})
			}
		}
	}
	return findings
}
