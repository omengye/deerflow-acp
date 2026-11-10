package harness

// SkillAvailabilityError carries a bounded, non-secret reason. It never grants
// access to a directory or adds a skill to the execution's pinned snapshot.
type SkillAvailabilityError struct{ Reason string }

func (e *SkillAvailabilityError) Error() string {
	switch e.Reason {
	case "installation_missing":
		return "installation_missing: Skill files exist in an authorized source, but this runtime database has no installation registration. Restore the published installation through host skill recovery, then start a new ordinary turn."
	case "disabled":
		return "disabled: This skill is disabled in the authorized source; use host skill settings to enable it before starting a new ordinary turn."
	case "source_unavailable":
		return "source_unavailable: The installed source is missing, changed or awaiting reconciliation; inspect host skill diagnostics."
	default:
		return "not_in_snapshot: The requested skill is not in this execution's visible skill snapshot. Verify the target session's skill availability on the host; a workspace draft is not an installed skill."
	}
}

func (e *SkillAvailabilityError) Unwrap() error { return ErrNotFound }
