package skills

import (
	"context"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// ReadObservation contains identities and digests, never skill contents. A
// successful load is evidence of access, not proof that instructions were
// followed or that a subsequent task outcome was caused by this skill.
type ReadObservation struct {
	Ref   harness.SkillRef
	Scope harness.SkillScope
	File  harness.SkillFileInfo
	Kind  string // skill_load or support_read
}

type ReadObserver func(context.Context, ReadObservation) error

// WithReadObserver binds a run-owned observer to a copy. Registry/operator
// snapshots remain unobserved, and restricted role snapshots inherit it.
func (s *Snapshot) WithReadObserver(observer ReadObserver) *Snapshot {
	copy := *s
	copy.observe = observer
	return &copy
}

func (s *Snapshot) observeRead(ctx context.Context, name, file, kind string) error {
	if s.observe == nil {
		return nil
	}
	e := s.entries[name]
	for _, info := range e.files {
		if info.Path == file {
			return s.observe(ctx, ReadObservation{Ref: e.ref, Scope: e.scope, File: info, Kind: kind})
		}
	}
	return harness.ErrNotFound
}
