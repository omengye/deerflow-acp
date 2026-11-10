package skills

import (
	"path/filepath"
	"runtime"
	"strings"
)

// WorkspaceSkillName gives independently scoped packages a durable, readable
// storage name. It is not used to evade a local deletion or name exclusion.
func WorkspaceSkillName(name, workspace string) string {
	workspace = filepath.Clean(workspace)
	if runtime.GOOS == "windows" {
		workspace = strings.ToLower(workspace)
	}
	name = strings.TrimRight(name[:min(len(name), 40)], "-")
	return name + "-ws" + digest([]byte(workspace))[:12]
}
