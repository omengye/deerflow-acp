package skills

import (
	"context"
	"encoding/json"
	"path/filepath"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// AuthoringPackage is a bounded in-memory snapshot for local authoring tools.
// Reading it performs the same path, frontmatter and file checks as Registry
// installation, without creating a registry or touching its database.
type AuthoringPackage struct {
	Name, Description, Version string
	Files                      map[string][]byte
	Manifest                   []harness.SkillFileInfo
	Findings                   []harness.SkillFinding
	Bytes                      int64
}

func ReadAuthoringPackage(ctx context.Context, directory string) (AuthoringPackage, error) {
	var out AuthoringPackage
	abs, err := filepath.Abs(directory)
	if err != nil {
		return out, err
	}
	s, err := openSource(harness.SkillSource{ID: "authoring", Root: filepath.Dir(abs), Scope: harness.SkillScopeGlobal})
	if err != nil {
		return out, err
	}
	defer s.root.Close()
	r := &Registry{limits: harness.SkillLimits{MaxFilesPerSkill: 20, MaxFileBytes: 128 << 10, MaxSkillBytes: 512 << 10}}
	p, err := r.readPackage(ctx, s, filepath.Base(abs))
	if err != nil {
		return out, err
	}
	manifest, err := json.Marshal(p.manifest)
	if err != nil {
		return out, err
	}
	return AuthoringPackage{Name: p.meta.Name, Description: p.meta.Description, Version: digest(manifest), Files: p.files, Manifest: p.manifest, Findings: p.meta.Findings, Bytes: p.bytes}, nil
}
