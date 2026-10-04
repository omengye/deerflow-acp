package evolution

// ValidateAuthoringFiles applies the candidate delivery profile to a local
// author's immutable file snapshot. It neither evaluates behavior nor grants
// publication. The caller must first validate paths and frontmatter through
// skills.ReadAuthoringPackage, as the normal candidate service does.
func ValidateAuthoringFiles(files map[string][]byte) error { return validateFiles(files) }
