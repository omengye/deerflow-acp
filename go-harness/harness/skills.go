package harness

import "time"

type SkillScope string

const (
	SkillScopeWorkspace SkillScope = "workspace"
	SkillScopeGlobal    SkillScope = "global"
)

// SkillSource explicitly authorizes one local installation source. Workspace
// sources must be inside the named canonical workspace. No default paths are
// searched, including HOME, the current process directory, or environment vars.
type SkillSource struct {
	ID        string     `json:"id"`
	Root      string     `json:"root"`
	Scope     SkillScope `json:"scope"`
	Workspace string     `json:"workspace,omitempty"`
}

type SkillLimits struct {
	MaxSkills           int
	MaxFilesPerSkill    int
	MaxVersionsPerSkill int
	MaxFileBytes        int64
	MaxSkillBytes       int64
	MaxTotalBytes       int64
}

type SkillsConfig struct {
	Sources []SkillSource
	Limits  SkillLimits
}

// SkillSelection is host policy, never an argument supplied by the model.
// Nil Names selects all enabled visible skills; an empty non-nil slice selects
// none. Global scope is opt-in even when global installation sources exist.
type SkillSelection struct {
	Workspace     string
	IncludeGlobal bool
	Names         []string
}

// SkillRef pins one immutable installed version for checkpoint restoration.
// SourceIdentity includes the configured root and workspace, preventing reuse
// of a source ID from granting access to versions from its previous scope.
type SkillRef struct {
	SourceID       string `json:"sourceId"`
	SourceIdentity string `json:"sourceIdentity"`
	Name           string `json:"name"`
	Version        string `json:"version"`
	Hash           string `json:"hash"`
}

type SkillRecord struct {
	Ref         SkillRef       `json:"ref"`
	Description string         `json:"description"`
	Scope       SkillScope     `json:"scope"`
	Workspace   string         `json:"workspace,omitempty"`
	Enabled     bool           `json:"enabled"`
	FileCount   int            `json:"fileCount"`
	TotalBytes  int64          `json:"totalBytes"`
	InstalledAt time.Time      `json:"installedAt"`
	Findings    []SkillFinding `json:"findings,omitempty"`
}

// SkillFinding is a deterministic review hint, not a safety classification.
// Messages never reproduce matched file contents or suspected secret values.
type SkillFinding struct {
	Code    string `json:"code"`
	Path    string `json:"path"`
	Message string `json:"message"`
}

type SkillFileInfo struct {
	Path  string `json:"path"`
	Hash  string `json:"hash"`
	Bytes int64  `json:"bytes"`
}
