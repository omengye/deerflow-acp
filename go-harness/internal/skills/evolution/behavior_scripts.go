package evolution

import (
	"context"
	"database/sql"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillbehavior"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
	"strings"
)

func scriptsCovered(files map[string][]byte, results []harness.SkillBehaviorResult) bool {
	required := map[string]bool{}
	for p := range files {
		if skillbehavior.HasScripts(map[string][]byte{p: nil}) {
			required[p] = false
		}
	}
	advertised := map[string]bool{}
	for p := range required {
		if strings.Contains(string(files["SKILL.md"]), p) {
			advertised[p] = false
		}
	}
	if len(advertised) > 0 {
		required = advertised
	}
	if len(required) == 0 {
		return false
	}
	for _, r := range results {
		if r.Variant != "candidate" {
			continue
		}
		for _, execution := range r.Scripts {
			if !hexDigest(execution.Digest) || execution.Runtime != skillbehavior.RuntimeVersion || execution.ExitCode != 0 {
				return false
			}
			if _, ok := required[execution.Path]; !ok {
				continue
			}
			required[execution.Path] = true
		}
	}
	for _, ran := range required {
		if !ran {
			return false
		}
	}
	return true
}

func inputFilesForReport(ctx context.Context, tx *sql.Tx, p Proposal) map[string][]byte {
	rows, err := tx.QueryContext(ctx, `SELECT path FROM df_skill_files WHERE source_key=? AND name=? AND version=?`, p.Target.SourceIdentity, p.Target.Name, p.CandidateVersion)
	if err != nil {
		return map[string][]byte{"scripts/unavailable": nil}
	}
	defer rows.Close()
	files := map[string][]byte{}
	for rows.Next() {
		var path string
		if rows.Scan(&path) != nil {
			return map[string][]byte{"scripts/unavailable": nil}
		}
		files[path] = nil
	}
	if rows.Err() != nil {
		return map[string][]byte{"scripts/unavailable": nil}
	}
	data, e := skills.ReadStoredFile(ctx, tx, p.Target.SourceIdentity, p.Target.Name, p.CandidateVersion, "SKILL.md", 512<<10)
	if e != nil {
		return map[string][]byte{"scripts/unavailable": nil}
	}
	files["SKILL.md"] = data
	return files
}

func operationalMeasurement(code string) bool { return code != "" && code != "assertion_failed" }

// Only the host interpreter's outcome is accepted; model prose cannot set this.
func validScriptExecution(s harness.SkillScriptExecution) bool {
	return skillbehavior.ValidPath(s.Path) && hexDigest(s.Digest) && strings.HasPrefix(s.Runtime, "oci-python312-v1/") && s.Runtime == skillbehavior.RuntimeVersion && s.ExitCode >= -127 && s.ExitCode <= 255
}
