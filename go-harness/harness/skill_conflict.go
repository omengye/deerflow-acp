package harness

import "fmt"

// SkillNameConflictError identifies catalog ambiguity without exposing skill bodies.
type SkillNameConflictError struct {
	Name, FirstSource, SecondSource string
}

func (e *SkillNameConflictError) Error() string {
	return fmt.Sprintf("skills duplicate enabled skill %q in sources %q and %q", e.Name, e.FirstSource, e.SecondSource)
}

func (e *SkillNameConflictError) Unwrap() error { return ErrInvalidInput }
