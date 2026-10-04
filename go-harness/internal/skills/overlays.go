package skills

import (
	"database/sql"
	"errors"
	"sort"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// BuiltinSources returns detached policy metadata, never installation paths.
func (r *Registry) BuiltinSources() ([]string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, ErrClosed
	}
	result := make([]string, 0)
	for id, source := range r.sources {
		if source.builtin {
			result = append(result, id)
		}
	}
	sort.Strings(result)
	return result, nil
}

func (r *Registry) ManagedSources() ([]string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return nil, ErrClosed
	}
	result := make([]string, 0)
	for id, source := range r.sources {
		if source.managed {
			result = append(result, id)
		}
	}
	sort.Strings(result)
	return result, nil
}

// OverlayBase identifies the imported content beneath this logical source/name.
// Candidates and overlays share the existing immutable version namespace, so
// roles, snapshots and source scope retain their exact pre-overlay identity.
// Merely asking for this reference does not activate an overlay or change files.
func (p *PublicationTx) OverlayBase() (*harness.SkillRef, error) {
	var version string
	err := p.tx.QueryRowContext(p.ctx, `SELECT base_version FROM df_skill_overlays WHERE source_key=? AND name=?`, p.source.identity, p.name).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		if !p.source.builtin {
			return nil, nil
		}
		err = p.tx.QueryRowContext(p.ctx, `SELECT last_import_version FROM df_skill_control WHERE source_key=? AND name=?`, p.source.identity, p.name).Scan(&version)
		if errors.Is(err, sql.ErrNoRows) || err == nil && version == "" {
			return nil, denied("builtin overlays require a previously imported skill; create new skills in a custom source")
		}
	}
	if err != nil {
		return nil, err
	}
	if _, err = p.ReadVersion(version); err != nil {
		return nil, err
	}
	return &harness.SkillRef{SourceID: p.source.config.ID, SourceIdentity: p.source.identity, Name: p.name, Version: version, Hash: version}, nil
}

func SameOverlayBase(a, b *harness.SkillRef) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func (p *PublicationTx) retainOverlay() error {
	base, err := p.OverlayBase()
	if err != nil || base == nil {
		return err
	}
	_, err = p.tx.ExecContext(p.ctx, `INSERT INTO df_skill_overlays(source_key,name,base_version,created_at) VALUES(?,?,?,?) ON CONFLICT(source_key,name) DO NOTHING`, p.source.identity, p.name, base.Version, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}
