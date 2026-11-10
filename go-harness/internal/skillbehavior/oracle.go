package skillbehavior

import (
	"regexp"
	"strings"
)

// A deterministic oracle is admitted only when the user supplied an explicit
// exact-output acceptance block. It is not inferred from an assistant answer.
// Other tasks remain without a trusted oracle until a host verifier is present.
var exactOutput = regexp.MustCompile("(?is)(?:expected (?:final )?output|预期(?:最终)?输出|预期结果)\\s*[:：]\\s*```[^\\n]*\\n(.*?)\\n```\\s*$")

func ExactOutputRequirement(text string) (prompt, expected string, ok bool) {
	m := exactOutput.FindStringSubmatchIndex(text)
	if m == nil || m[0] < 8 {
		return "", "", false
	}
	prompt = strings.TrimSpace(text[:m[0]])
	expected = text[m[2]:m[3]]
	if !Text(prompt, 32768) || !Text(expected, 65536) || strings.TrimSpace(expected) == "" {
		return "", "", false
	}
	return prompt, expected, true
}

// ExactArtifactRequirement recognizes an explicit user-owned final file oracle.
// The required path remains in the task; expected bytes stay in the host scorer.
var exactArtifact = regexp.MustCompile("(?is)(?:expected artifact|预期产物)\\s*`([^`]+)`\\s*[:：]\\s*```[^\\n]*\\n(.*?)\\n```\\s*$")

func ExactArtifactRequirement(text string) (prompt, path, expected string, ok bool) {
	m := exactArtifact.FindStringSubmatchIndex(text)
	if m == nil || m[0] < 8 {
		return
	}
	path, expected = text[m[2]:m[3]], text[m[4]:m[5]]
	prompt = strings.TrimSpace(text[:m[0]]) + "\nWrite the final artifact to " + path + "."
	ok = ValidPath(path) && Text(prompt, 32768) && Text(expected, 65536)
	return
}
