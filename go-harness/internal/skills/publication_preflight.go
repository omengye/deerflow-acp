package skills

import (
	"errors"
	"os"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// PublicationConflict keeps actionable, sanitized causes across the host API.
type PublicationConflict struct{ Code string }

func (e *PublicationConflict) Error() string { return "skill publication blocked: " + e.Code }
func (e *PublicationConflict) Unwrap() error { return ErrPublicationConflict }

func publicationConflict(code string) error { return &PublicationConflict{Code: code} }

// SourceCandidate reads only this transaction's configured destination. It does
// not install, enable or execute the package, and never returns historical bytes.
func (p *PublicationTx) SourceCandidate() (*Candidate, error) {
	return p.sourceCandidateAt(p.name)
}

func (p *PublicationTx) sourceCandidateAt(dir string) (*Candidate, error) {
	if err := validateSourceRoot(p.source); err != nil {
		return nil, publicationConflict("source_unavailable")
	}
	first, err := p.registry.readPackage(p.ctx, p.source, dir)
	if err != nil || first.meta.Name != p.name {
		if p.ctx.Err() != nil {
			return nil, p.ctx.Err()
		}
		return nil, publicationConflict("source_invalid_or_unavailable")
	}
	second, err := p.registry.readPackage(p.ctx, p.source, dir)
	candidate := &Candidate{pkg: first}
	if err != nil || (&Candidate{pkg: second}).Version() != candidate.Version() {
		return nil, publicationConflict("source_changed_while_reading")
	}
	return candidate, nil
}

// CheckCreateTarget admits only absence or the entire candidate package. Extra
// support files matter just as much as SKILL.md; no content is overwritten.
func (p *PublicationTx) CheckCreateTarget(version string) (bool, error) {
	return p.checkCreateTargetAt(p.name, version)
}

func (p *PublicationTx) checkCreateTargetAt(dir, version string) (bool, error) {
	if p.source.builtin {
		return false, harness.ErrPermissionDenied
	}
	if err := validateSourceRoot(p.source); err != nil {
		return false, publicationConflict("source_unavailable")
	}
	_, err := p.source.root.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, publicationConflict("source_unavailable")
	}
	candidate, err := p.sourceCandidateAt(dir)
	if err != nil {
		return false, err
	}
	if candidate.Version() != version {
		return false, publicationConflict("target_content_differs")
	}
	return true, nil
}

// GuardCreate prevents startup discovery from installing an unapproved draft.
// Successful publication removes the guard in its installation transaction.
func (p *PublicationTx) GuardCreate() error {
	_, err := p.tx.ExecContext(p.ctx, `INSERT INTO df_skill_creation_guards(source_key,name) VALUES(?,?) ON CONFLICT DO NOTHING`, p.source.identity, p.name)
	return err
}
