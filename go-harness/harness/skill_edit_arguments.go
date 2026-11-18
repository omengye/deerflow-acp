package harness

// SkillEditArgumentError exposes only fixed host guidance. Rejected arguments
// may contain credentials, so neither their values nor decoder errors belong in
// the model-facing message or tool receipt.
type SkillEditArgumentError struct{ Reason string }

func (e *SkillEditArgumentError) Error() string {
	switch e.Reason {
	case "json":
		return "skill_edit arguments must be one JSON object using only action, name, edits and edit_id. Check field names and value types."
	case "prepare_edit_id":
		return "skill_edit prepare requires name and edits; omit edit_id. Use the edit_id returned by prepare for apply or status."
	case "reference_name":
		return "skill_edit apply/status accepts only action and edit_id. Remove name; the prepared edit_id already identifies the skill. Retry the same edit_id without name or edits; do not change the patch."
	case "reference_edits":
		return "skill_edit apply/status accepts only action and edit_id. Remove edits; file changes belong in prepare. Retry the same edit_id without name or edits."
	case "reference_edit_id":
		return "skill_edit apply/status requires edit_id from a successful prepare. Send only action and edit_id."
	default:
		return "skill_edit action must be prepare, apply or status. For prepare send name and edits; for apply/status send only action and edit_id."
	}
}

func (e *SkillEditArgumentError) Unwrap() error { return ErrInvalidInput }
