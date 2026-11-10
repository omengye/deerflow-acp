package skills

import (
	"context"
	"database/sql"
	"errors"
	"os"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
)

// Automatic publications live in the immutable managed store. The original
// source files stay authoritative: any explicit file edit supersedes the route.
// This also prevents external file readers from receiving an observing version.
func (p *PublicationTx) PublishAutomatic(version string, expected SkillHead, enabled bool, workspace string) (SkillHead, error) {
	head, err := p.Current()
	if err != nil {
		return head, err
	}
	if head != expected {
		return head, ErrPublicationConflict
	}
	if workspace == "" || p.source.builtin {
		return head, harness.ErrPermissionDenied
	}
	if version != "" {
		if _, err = p.ReadVersion(version); err != nil {
			return head, err
		}
		if err = checkScopedVersion(p.ctx, p.tx, harness.SkillRef{SourceIdentity: p.source.identity, Name: p.name, Version: version}, workspace); err != nil {
			return head, err
		}
	}
	physical, scoped, err := p.physicalVersion(head)
	if err != nil {
		return head, err
	}
	if scoped {
		var priorScope string
		if err = p.tx.QueryRowContext(p.ctx, `SELECT workspace FROM df_skill_routes WHERE source_key=? AND name=?`, p.source.identity, p.name).Scan(&priorScope); err != nil {
			return head, err
		}
		if !session.SameWorkspace(priorScope, workspace) {
			return head, harness.ErrPermissionDenied
		}
	}
	dir := p.name
	if head.StateVersion > 0 {
		if err = p.tx.QueryRowContext(p.ctx, `SELECT import_directory FROM df_skill_control WHERE source_key=? AND name=?`, p.source.identity, p.name).Scan(&dir); err != nil {
			return head, err
		}
	}
	if dir == "" || dir == "." || !validRelative(dir) {
		return head, harness.ErrPermissionDenied
	}
	if physical == "" {
		if _, err = p.source.root.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
			return head, ErrPublicationConflict
		}
	} else {
		pkg, e := p.registry.readPackage(p.ctx, p.source, dir)
		if e != nil || (&Candidate{pkg: pkg}).Version() != physical {
			return head, ErrPublicationConflict
		}
	}
	if err = AdmitObservationTx(p.ctx, p.tx, p.Target(), workspace, head.Version, version); err != nil {
		return head, err
	}
	if version != "" && version != physical {
		_, err = p.tx.ExecContext(p.ctx, `INSERT OR IGNORE INTO df_skill_scoped_versions VALUES(?,?,?,?)`, p.source.identity, p.name, version, workspace)
	}
	if err != nil {
		return head, err
	}
	next, err := p.moveHead(head, version, enabled, true, physical, dir)
	if err != nil {
		return head, err
	}
	if err = p.recordSourceStatus(dir, "ready", version, ""); err != nil {
		return head, err
	}
	_, err = p.tx.ExecContext(p.ctx, `INSERT INTO df_skill_file_publication_results VALUES(?,?,?,'applied','automatic')`, p.source.identity, p.name, next.StateVersion)
	return next, err
}

func (p *PublicationTx) physicalMatches(head SkillHead, dir string) (bool, error) {
	version, _, err := p.physicalVersion(head)
	if err != nil {
		return false, err
	}
	if version == "" {
		_, err = p.source.root.Lstat(dir)
		return errors.Is(err, os.ErrNotExist), nil
	}
	pkg, err := p.registry.readPackage(p.ctx, p.source, dir)
	if err != nil {
		return false, err
	}
	return (&Candidate{pkg: pkg}).Version() == version, nil
}

func (p *PublicationTx) physicalVersion(head SkillHead) (string, bool, error) {
	var physical string
	err := p.tx.QueryRowContext(p.ctx, `SELECT public_version FROM df_skill_routes WHERE source_key=? AND name=? AND candidate_version=?`, p.source.identity, p.name, head.Version).Scan(&physical)
	if errors.Is(err, sql.ErrNoRows) {
		return head.Version, false, nil
	}
	return physical, err == nil, err
}

// Checkpoint possession alone is insufficient authority to move a scoped skill
// to a different workspace. Historical versions retain their original binding.
func checkScopedVersion(ctx context.Context, tx *sql.Tx, ref harness.SkillRef, workspace string) error {
	var scope string
	err := tx.QueryRowContext(ctx, `SELECT workspace FROM df_skill_scoped_versions WHERE source_key=? AND name=? AND version=?`, ref.SourceIdentity, ref.Name, ref.Version).Scan(&scope)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !session.SameWorkspace(scope, workspace) {
		return harness.ErrPermissionDenied
	}
	return nil
}
