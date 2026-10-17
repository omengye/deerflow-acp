package skills

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"sort"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type filePublication struct {
	ID         string    `json:"id"`
	Directory  string    `json:"directory"`
	Base       SkillHead `json:"base"`
	Next       SkillHead `json:"next"`
	ProposalID string    `json:"proposal_id,omitempty"`
	Adopt      bool      `json:"adopt_existing,omitempty"`
}

// BindPublicationProposal defers the published state until filesystem commit.
// The journal keeps this completion action durable across host restarts.
func (p *PublicationTx) BindPublicationProposal(id string) error {
	if p.publication == nil || id == "" {
		return invalid("publication journal required")
	}
	p.publication.ProposalID = id
	data, err := json.Marshal(p.publication)
	if err != nil {
		return err
	}
	_, err = p.tx.ExecContext(p.ctx, `UPDATE df_skill_file_publications SET payload=?,payload_sha=? WHERE source_key=? AND name=?`, data, digest(data), p.source.identity, p.name)
	return err
}

func publicationPending(ctx context.Context, q fileQuery, source, name string) (bool, error) {
	var pending bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_file_publications WHERE source_key=? AND name=?)`, source, name).Scan(&pending)
	return pending, err
}

// PublicationPending is evidence for host result projections. A durable
// approval is not a completed file publication while its journal exists.
func PublicationPending(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, source, name string) (bool, error) {
	return publicationPending(ctx, q, source, name)
}

func (p *PublicationTx) ResumeFilePublication() error {
	rec, err := readFilePublication(p.ctx, p.tx, p.source.identity, p.name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	p.publication = &rec
	return nil
}

func (p *PublicationTx) preparePublication(version string, head SkillHead, enabled bool) (SkillHead, error) {
	if p.source.builtin {
		return head, denied("builtin files are read-only; create an explicit custom copy, not a database overlay")
	}
	if pending, err := editPending(p.ctx, p.tx, p.source.identity, p.name, ""); err != nil {
		return head, err
	} else if pending {
		return head, errors.Join(ErrPublicationConflict, harness.ErrReconciliationRequired)
	}
	if pending, err := publicationPending(p.ctx, p.tx, p.source.identity, p.name); err != nil {
		return head, err
	} else if pending {
		return head, errors.Join(ErrPublicationConflict, harness.ErrReconciliationRequired)
	}
	dir := p.name
	if head.StateVersion > 0 {
		if err := p.tx.QueryRowContext(p.ctx, `SELECT import_directory FROM df_skill_control WHERE source_key=? AND name=?`, p.source.identity, p.name).Scan(&dir); err != nil {
			return head, err
		}
		if dir == "" {
			return head, denied("legacy database-only skill requires an explicit file migration")
		}
	}
	if dir == "." || !validRelative(dir) {
		return head, invalid("publication requires a bound package subdirectory")
	}
	adopt := false
	if head.Exists {
		pkg, err := p.registry.readPackage(p.ctx, p.source, dir)
		if err != nil {
			return head, err
		}
		if (&Candidate{pkg: pkg}).Version() != head.Version {
			return head, ErrPublicationConflict
		}
	} else {
		var err error
		adopt, err = p.checkCreateTargetAt(dir, version)
		if err != nil {
			return head, err
		}
	}
	if version == "" && enabled {
		return head, invalid("deleted skill cannot be enabled")
	}
	if version != "" && !head.Exists {
		var count int
		if err := p.tx.QueryRowContext(p.ctx, `SELECT count(*) FROM df_skill_installs`).Scan(&count); err != nil {
			return head, err
		}
		if count >= p.limits.MaxSkills {
			return head, invalid("installed skill count limit exceeded")
		}
	}
	next := SkillHead{Version: version, StateVersion: head.StateVersion + 1, Exists: version != "", Enabled: enabled, Managed: true}
	// Preflight export/audit constraints in the same approval transaction.
	// Workers still reject this predicted head until real files are installed.
	if err := p.queueExport(next); err != nil {
		return head, err
	}
	rec := filePublication{ID: rand.Text(), Directory: dir, Base: head, Next: next, Adopt: adopt}
	data, _ := json.Marshal(rec)
	_, err := p.tx.ExecContext(p.ctx, `INSERT INTO df_skill_file_publications VALUES(?,?,?,?)`, p.source.identity, p.name, data, digest(data))
	if err != nil {
		return head, err
	}
	if _, err = p.tx.ExecContext(p.ctx, `INSERT INTO df_skill_file_publication_results VALUES(?,?,?,'pending','') ON CONFLICT(source_key,name,state_version) DO UPDATE SET status='pending',actor=''`, p.source.identity, p.name, next.StateVersion); err != nil {
		return head, err
	}
	p.publication = &rec
	return next, nil
}

func readFilePublication(ctx context.Context, q fileQuery, source, name string) (filePublication, error) {
	var rec filePublication
	var data []byte
	var sha string
	err := q.QueryRowContext(ctx, `SELECT payload,payload_sha FROM df_skill_file_publications WHERE source_key=? AND name=?`, source, name).Scan(&data, &sha)
	if err != nil {
		return rec, err
	}
	if digest(data) != sha || json.Unmarshal(data, &rec) != nil || !validEditID(rec.ID) || !validRelative(rec.Directory) || path.Base(rec.Directory) != name || rec.Next.StateVersion != rec.Base.StateVersion+1 || !rec.Next.Managed || rec.Next.Exists != (rec.Next.Version != "") || rec.Next.Exists && !validHash(rec.Next.Version) || !rec.Next.Exists && rec.Next.Enabled || rec.Base.Exists && !validHash(rec.Base.Version) {
		return rec, invalid("publication journal integrity failure")
	}
	if rec.Adopt && (rec.Base.Exists || !rec.Next.Exists) {
		return rec, invalid("invalid adoption journal")
	}
	return rec, nil
}

func (r *Registry) finishPublication(ctx context.Context, s *source, rec filePublication) error {
	if s.builtin {
		return errors.Join(harness.ErrReconciliationRequired, denied("builtin source policy now forbids this admitted publication"))
	}
	name := path.Base(rec.Directory)
	return r.withEditPublication(ctx, s, name, "", func(p *PublicationTx) error {
		fresh, err := readFilePublication(ctx, p.tx, s.identity, name)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if fresh != rec {
			return harness.ErrSkillConflict
		}
		head, err := p.Current()
		if err != nil {
			return err
		}
		if head != rec.Base {
			return harness.ErrSkillConflict
		}
		var candidate *Candidate
		if rec.Next.Exists {
			candidate, err = p.ReadVersion(rec.Next.Version)
			if err != nil {
				return err
			}
		}
		if err = r.writePublicationFiles(ctx, s, rec, candidate); err != nil {
			return errors.Join(harness.ErrReconciliationRequired, err)
		}
		if rec.Next.Exists {
			for range 2 {
				pkg, e := r.readPackage(ctx, s, rec.Directory)
				if e != nil || (&Candidate{pkg: pkg}).Version() != rec.Next.Version {
					return errors.Join(harness.ErrReconciliationRequired, publicationConflict("source_changed_while_reading"))
				}
			}
		}
		if rec.ProposalID != "" {
			// Proposal intent/diff are unchanged. Only the durable publication
			// phase changes, in the transaction that indexes verified files.
			result, e := p.tx.ExecContext(ctx, `UPDATE df_skill_proposals SET status='published',payload=json_set(CAST(payload AS TEXT),'$.status','published','$.file_publication_pending',json('false')) WHERE id=? AND source_key=? AND name=? AND status='publishing'`, rec.ProposalID, s.identity, name)
			if e != nil {
				return e
			}
			n, e := result.RowsAffected()
			if e != nil {
				return e
			}
			if n != 1 {
				return invalid("publication proposal completion mismatch")
			}
		}
		if _, err = p.tx.ExecContext(ctx, `DELETE FROM df_skill_file_publications WHERE source_key=? AND name=?`, s.identity, name); err != nil {
			return err
		}
		if _, err = p.tx.ExecContext(ctx, `UPDATE df_skill_file_publication_results SET status='applied' WHERE source_key=? AND name=? AND state_version=?`, s.identity, name, rec.Next.StateVersion); err != nil {
			return err
		}
		_, err = p.moveHead(head, rec.Next.Version, rec.Next.Enabled, true, rec.Next.Version, rec.Directory)
		if err == nil {
			err = p.recordSourceStatus(rec.Directory, "ready", rec.Next.Version, "")
		}
		if err == nil {
			_, err = p.tx.ExecContext(ctx, `DELETE FROM df_skill_creation_guards WHERE source_key=? AND name=?`, s.identity, name)
		}
		return err
	})
}

func (r *Registry) writePublicationFiles(ctx context.Context, s *source, rec filePublication, candidate *Candidate) error {
	if err := validateSourceRoot(s); err != nil {
		return err
	}
	versionAt := func(dir string) (string, error) {
		root, err := openExportDir(s.root, dir, false)
		if err != nil {
			return "", err
		}
		defer root.Close()
		pkg, err := r.readPackage(ctx, &source{root: root}, ".")
		if err != nil {
			return "", err
		}
		return (&Candidate{pkg: pkg}).Version(), nil
	}
	backup := ".deerflow-publication-backups/" + rec.ID
	stage := ".deerflow-publication-stage/" + rec.ID
	actual, statErr := s.root.Lstat(rec.Directory)
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if rec.Adopt && !exists {
		return publicationConflict("adoption_source_missing")
	}
	if exists {
		if !actual.IsDir() || actual.Mode()&os.ModeSymlink != 0 {
			return harness.ErrSkillConflict
		}
		v, err := versionAt(rec.Directory)
		if err != nil {
			return err
		}
		if rec.Next.Exists && v == rec.Next.Version {
			return nil
		}
		if !rec.Base.Exists || v != rec.Base.Version {
			return harness.ErrSkillConflict
		}
	}
	if !exists && rec.Base.Exists {
		v, err := versionAt(backup)
		if err != nil || v != rec.Base.Version {
			return harness.ErrSkillConflict
		}
	}
	if candidate != nil {
		staged, err := openExportDir(s.root, stage, true)
		if err != nil {
			return err
		}
		defer staged.Close()
		pending, err := openExportDir(s.root, stage+"-pending", true)
		if err != nil {
			return err
		}
		defer pending.Close()
		names := make([]string, 0, len(candidate.pkg.files))
		for name := range candidate.pkg.files {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if err := ctx.Err(); err != nil {
				return err
			}
			dir, err := openExportDir(staged, path.Dir(name), true)
			if err != nil {
				return err
			}
			data := candidate.pkg.files[name]
			_, err = dir.Lstat(path.Base(name))
			if err == nil {
				info, e := dir.Lstat(path.Base(name))
				if e != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
					_ = dir.Close()
					return harness.ErrSkillConflict
				}
				old, e := dir.ReadFile(path.Base(name))
				_ = dir.Close()
				if e != nil || digest(old) != digest(data) {
					return harness.ErrSkillConflict
				}
				continue
			}
			if !errors.Is(err, os.ErrNotExist) {
				_ = dir.Close()
				return err
			}
			tmp := rand.Text()
			f, err := pending.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				_ = dir.Close()
				return err
			}
			if rec.Base.Exists {
				origin := rec.Directory
				if !exists {
					origin = backup
				}
				if info, e := s.root.Lstat(path.Join(origin, name)); e == nil && info.Mode().IsRegular() {
					err = preserveEditSecurity(s.root, path.Join(origin, name), f)
				}
			}
			if err == nil {
				var n int
				n, err = f.Write(data)
				if err == nil && n != len(data) {
					err = io.ErrShortWrite
				}
			}
			if err == nil {
				err = f.Sync()
			}
			err = errors.Join(err, f.Close())
			if err == nil {
				err = s.root.Link(path.Join(stage+"-pending", tmp), path.Join(stage, name))
			}
			_ = pending.Remove(tmp)
			err = errors.Join(err, dir.Close())
			if err != nil {
				return err
			}
		}
		v, err := versionAt(stage)
		if err != nil || v != rec.Next.Version {
			return harness.ErrSkillConflict
		}
	}
	if exists {
		parent, err := openExportDir(s.root, path.Dir(backup), true)
		if err != nil {
			return err
		}
		_ = parent.Close()
		if _, err = s.root.Lstat(backup); !errors.Is(err, os.ErrNotExist) {
			return harness.ErrSkillConflict
		}
		v, err := versionAt(rec.Directory)
		if err != nil || v != rec.Base.Version {
			return harness.ErrSkillConflict
		}
		if err = renameNoReplace(s.root, rec.Directory, backup); err != nil {
			return err
		}
		v, err = versionAt(backup)
		if err != nil || v != rec.Base.Version {
			return harness.ErrSkillConflict
		}
	}
	if candidate == nil {
		return nil
	}
	if err := validateSourceRoot(s); err != nil {
		return err
	}
	parent, err := openExportDir(s.root, path.Dir(rec.Directory), true)
	if err != nil {
		return err
	}
	_ = parent.Close()
	if _, err = s.root.Lstat(rec.Directory); !errors.Is(err, os.ErrNotExist) {
		return harness.ErrSkillConflict
	}
	if err = renameNoReplace(s.root, stage, rec.Directory); err != nil {
		return err
	}
	v, err := versionAt(rec.Directory)
	if err != nil || v != rec.Next.Version {
		return harness.ErrSkillConflict
	}
	return nil
}

func (r *Registry) RecoverPublications(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	for _, s := range r.sources {
		rows, err := r.db.QueryContext(ctx, `SELECT name FROM df_skill_file_publications WHERE source_key=?`, s.identity)
		if err != nil {
			return err
		}
		var names []string
		for rows.Next() {
			var name string
			if err = rows.Scan(&name); err != nil {
				break
			}
			names = append(names, name)
		}
		if err == nil {
			err = rows.Err()
		}
		_ = rows.Close()
		if err != nil {
			return err
		}
		for _, name := range names {
			rec, err := readFilePublication(ctx, r.db, s.identity, name)
			if err != nil {
				return err
			}
			if err = r.finishPublication(ctx, s, rec); err != nil && !errors.Is(err, harness.ErrReconciliationRequired) {
				return err
			}
		}
	}
	return nil
}
