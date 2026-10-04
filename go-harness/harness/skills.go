package harness

import (
	"strings"
	"time"
)

// ValidSkillName validates the stable names shared by skills and source IDs.
func ValidSkillName(s string) bool {
	if len(s) < 1 || len(s) > 64 || s[0] == '-' || s[len(s)-1] == '-' || strings.Contains(s, "--") {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

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
	// BuiltinSources identifies immutable distribution sources. Reviewed changes
	// become managed SQLite overlays with retained import provenance. This policy
	// is separate from source identity so existing pinned refs remain valid.
	BuiltinSources []string `json:"builtinSources,omitempty"`
	// ManagedSources are explicitly registered SQLite-only sources. Their root
	// binds identity/scope; files under it are never imported as live skills.
	ManagedSources []string `json:"managedSources,omitempty"`
	// ExportRoot is an explicit existing directory for rebuildable managed
	// publication views. Export failures do not roll back a published skill.
	ExportRoot string `json:"exportRoot,omitempty"`
	// Install is an explicit operator startup action. No packages are scanned
	// or installed unless listed here or installed through the SDK.
	Install []SkillInstall `json:"install,omitempty"`
}

// SkillInstall installs one named relative package and applies its enabled
// state at startup. Repeated startup with identical content is idempotent.
// Once publications manage that installation, its durable release pointer,
// enablement and deletion state take precedence over this import profile.
type SkillInstall struct {
	SourceID  string `json:"sourceId"`
	Directory string `json:"directory"`
	Enabled   bool   `json:"enabled"`
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
	Ref          SkillRef       `json:"ref"`
	Description  string         `json:"description"`
	Scope        SkillScope     `json:"scope"`
	Workspace    string         `json:"workspace,omitempty"`
	Enabled      bool           `json:"enabled"`
	FileCount    int            `json:"fileCount"`
	TotalBytes   int64          `json:"totalBytes"`
	InstalledAt  time.Time      `json:"installedAt"`
	Findings     []SkillFinding `json:"findings,omitempty"`
	Builtin      bool           `json:"builtin,omitempty"`
	Managed      bool           `json:"managed,omitempty"`
	StateVersion int64          `json:"stateVersion"`
	OverlayBase  *SkillRef      `json:"overlayBase,omitempty"`
}

// SkillExportState is separate from publication success. Directory is relative
// to the configured export root and becomes usable only when status is synced.
// A view contains publication.json and, for a live head, the files/ package.
type SkillExportState struct {
	Target     SkillTarget `json:"target"`
	Head       SkillHead   `json:"head"`
	StateToken string      `json:"stateToken"` // Binds export root and saved worker state.
	Status     string      `json:"status"`     // pending, synced, failed
	Directory  string      `json:"directory,omitempty"`
	Attempts   int         `json:"attempts"`
	ErrorCode  string      `json:"errorCode,omitempty"`
	UpdatedAt  time.Time   `json:"updatedAt"`
	RetryAfter *time.Time  `json:"retryAfter,omitempty"`
}

// SkillImportObservation reports the last bounded read of a managed skill's
// original import directory. It is advisory; it never changes a publication or
// grants permission to use the observed files. Tombstones can have observations.
type SkillImportObservation struct {
	Target          SkillTarget             `json:"target"`
	ImportedVersion string                  `json:"importedVersion"`
	ObservedVersion string                  `json:"observedVersion,omitempty"`
	Status          string                  `json:"status"` // unchanged, changed, missing, invalid, unavailable
	CheckedAt       time.Time               `json:"checkedAt"`
	Changes         []SkillImportFileChange `json:"changes,omitempty"`
	Truncated       bool                    `json:"truncated,omitempty"`
}

// SkillImportFileChange contains paths and digests, never source file contents.
type SkillImportFileChange struct {
	Path       string `json:"path"`
	Change     string `json:"change"` // added, modified, removed
	BeforeHash string `json:"beforeHash,omitempty"`
	AfterHash  string `json:"afterHash,omitempty"`
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
