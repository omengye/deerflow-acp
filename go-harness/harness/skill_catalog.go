package harness

// SkillCatalogEntry is operator metadata, including deleted installations.
// Import/export states are advisory snapshots and do not authorize execution.
type SkillCatalogEntry struct {
	Target           SkillTarget             `json:"target"`
	Head             SkillHead               `json:"head"`
	Record           *SkillRecord            `json:"record,omitempty"`
	Builtin          bool                    `json:"builtin"`
	ManagedSource    bool                    `json:"managed_source"`
	OverlayBase      *SkillRef               `json:"overlay_base,omitempty"`
	Import           *SkillImportObservation `json:"import_observation,omitempty"`
	Export           *SkillExportState       `json:"export,omitempty"`
	CanRefreshImport bool                    `json:"can_refresh_import"`
	SourceDirectory  string                  `json:"source_directory,omitempty"`
	SourceStatus     string                  `json:"source_status"`
	SourceError      string                  `json:"source_error,omitempty"`
}

type SkillCatalogSource struct {
	Target        SkillTarget `json:"target"` // Name is empty for source policy.
	Builtin       bool        `json:"builtin"`
	ManagedSource bool        `json:"managed_source"`
}

type SkillCatalogPage struct {
	Diagnostics    []SkillFinding       `json:"diagnostics,omitempty"`
	Skills         []SkillCatalogEntry  `json:"skills"`
	Sources        []SkillCatalogSource `json:"sources"`
	Next           string               `json:"next_cursor,omitempty"`
	CatalogVersion int64                `json:"catalog_version"`
}

type SkillCatalogDetail struct {
	Skill          SkillCatalogEntry `json:"skill"`
	CatalogVersion int64             `json:"catalog_version"`
}

type SkillFilePublication struct {
	ID              string      `json:"id"`
	Target          SkillTarget `json:"target"`
	SourceDirectory string      `json:"source_directory"`
	BackupRelative  string      `json:"backup_relative"`
	Base            SkillHead   `json:"base"`
	Next            SkillHead   `json:"next"`
	Current         SkillHead   `json:"current"`
	ProposalID      string      `json:"proposal_id,omitempty"`
	Status          string      `json:"status"`
}

// SkillEnablement is an operator command, never a model tool. A stable key
// identifies the complete intent; reusing it with different fields conflicts.
type SkillEnablement struct {
	Target     SkillTarget `json:"target"`
	Expected   SkillHead   `json:"expected_head"`
	Enabled    bool        `json:"enabled"`
	RequestKey string      `json:"request_key"`
	Actor      string      `json:"actor"`
}

type SkillExportRetry struct {
	Target        SkillTarget `json:"target"`
	Expected      SkillHead   `json:"expected_head"`
	ExpectedToken string      `json:"expected_export_token"`
	RequestKey    string      `json:"request_key"`
	Actor         string      `json:"actor"`
}

// SkillManagementResult keeps the originally applied head on replay, while
// Skill and CatalogVersion describe the current transaction's snapshot.
type SkillManagementResult struct {
	SkillCatalogDetail
	AppliedHead SkillHead `json:"applied_head"`
	Replayed    bool      `json:"replayed"`
}
