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
}

type SkillCatalogSource struct {
	Target        SkillTarget `json:"target"` // Name is empty for source policy.
	Builtin       bool        `json:"builtin"`
	ManagedSource bool        `json:"managed_source"`
}

type SkillCatalogPage struct {
	Skills         []SkillCatalogEntry  `json:"skills"`
	Sources        []SkillCatalogSource `json:"sources"`
	Next           string               `json:"next_cursor,omitempty"`
	CatalogVersion int64                `json:"catalog_version"`
}

type SkillCatalogDetail struct {
	Skill          SkillCatalogEntry `json:"skill"`
	CatalogVersion int64             `json:"catalog_version"`
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
