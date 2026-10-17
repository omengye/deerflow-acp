package skills

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type editRecord struct {
	Review       harness.SkillEditReview `json:"review"`
	Owner        string                  `json:"owner"`
	Key          string                  `json:"key"`
	Intent       string                  `json:"intent"`
	Dir          string                  `json:"directory"`
	Status       string                  `json:"status"`
	Applied      harness.SkillHead       `json:"applied_head"`
	Error        string                  `json:"error_code"`
	ReconciledBy string                  `json:"reconciled_by,omitempty"`
}

func (r *Registry) CheckEdit(ctx context.Context, ref harness.SkillRef, edits []harness.SkillFileEdit) error {
	return r.WithPublication(ctx, ref.SourceID, ref.Name, func(p *PublicationTx) error {
		if p.source.identity != ref.SourceIdentity {
			return harness.ErrPermissionDenied
		}
		base, err := p.ReadVersion(ref.Version)
		if err != nil {
			return err
		}
		files, _, err := applyFileEdits(base.Files(), edits)
		if err != nil {
			return err
		}
		_, err = packageFromFiles(ref.Name, files, r.limits)
		return err
	})
}

func (r *Registry) migrateEdits(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS df_skill_edits (
 id TEXT PRIMARY KEY, source_key TEXT NOT NULL, name TEXT NOT NULL, owner TEXT NOT NULL,
 request_key TEXT NOT NULL, status TEXT NOT NULL, payload BLOB NOT NULL, payload_sha TEXT NOT NULL,
 UNIQUE(owner,request_key))`)
	return err
}

func readEdit(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (editRecord, error) {
	var rec editRecord
	var body []byte
	var sha, source, name, owner, key, status string
	err := q.QueryRowContext(ctx, `SELECT payload,payload_sha,source_key,name,owner,request_key,status FROM df_skill_edits WHERE id=?`, id).Scan(&body, &sha, &source, &name, &owner, &key, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return rec, harness.ErrNotFound
	}
	if err != nil {
		return rec, err
	}
	if digest(body) != sha || json.Unmarshal(body, &rec) != nil || rec.Review.ID != id || rec.Review.Target.SourceIdentity != source || rec.Review.Target.Name != name || rec.Owner != owner || rec.Key != key || rec.Status != status || !validHash(rec.Review.Candidate) || !validManagementHead(rec.Review.Base) {
		return rec, errors.New("skill edit journal is inconsistent")
	}
	return rec, nil
}

func saveEdit(ctx context.Context, tx *sql.Tx, rec editRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO df_skill_edits VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET status=excluded.status,payload=excluded.payload,payload_sha=excluded.payload_sha`, rec.Review.ID, rec.Review.Target.SourceIdentity, rec.Review.Target.Name, rec.Owner, rec.Key, rec.Status, data, digest(data))
	return err
}

func (p *PublicationTx) editStorage(head SkillHead) (mode, directory string, err error) {
	if p.source.builtin {
		return "builtin", "", nil
	}
	err = p.tx.QueryRowContext(p.ctx, `SELECT import_directory FROM df_skill_control WHERE source_key=? AND name=?`, p.source.identity, p.name).Scan(&directory)
	if err != nil {
		return "", "", err
	}
	if directory != "" {
		if directory == "." {
			return "unknown", directory, nil
		}
		return "source", directory, nil
	}
	return "unknown", "", nil
}

func (r *Registry) InspectSkill(ctx context.Context, ref harness.SkillRef) (out harness.SkillInspection, err error) {
	err = r.WithPublication(ctx, ref.SourceID, ref.Name, func(p *PublicationTx) error {
		if p.source.identity != ref.SourceIdentity {
			return harness.ErrPermissionDenied
		}
		head, err := p.Current()
		if err != nil || !head.Exists {
			if err == nil {
				err = harness.ErrNotFound
			}
			return err
		}
		pkg, err := p.ReadVersion(ref.Version)
		if err != nil {
			return err
		}
		mode, dir, err := p.editStorage(head)
		if err != nil {
			return err
		}
		out = harness.SkillInspection{Target: p.target, PinnedVersion: ref.Version, Head: head, Storage: mode, Files: pkg.pkg.manifest, Editable: mode == "source", NextAction: "The bound directory is authoritative. Use skill_edit prepare/apply, not a workspace copy. This execution and explicit resume retain historical snapshots; new ordinary turns re-read the directory."}
		if dir != "" {
			out.SourceDirectory = filepath.Join(p.source.config.Root, filepath.FromSlash(dir))
		}
		if mode == "builtin" {
			out.BlockedReason = "Builtin files are read-only; create and explicitly bind a custom copy. Hidden overlays are forbidden."
		}
		if mode == "unknown" {
			out.BlockedReason = "No verified editable source origin is registered."
		}
		if ref.Version != head.Version {
			out.Editable = false
			out.BlockedReason = "Current run pins an older version; start an ordinary new turn before editing."
		}
		if mode == "source" {
			actual, e := r.readPackage(ctx, p.source, dir)
			if e != nil || (&Candidate{pkg: actual}).Version() != head.Version {
				out.Editable = false
				out.BlockedReason = "Source files changed or became invalid; start a new ordinary turn to refresh, or repair the directory."
			}
		}
		if pending, e := editPending(ctx, p.tx, p.source.identity, ref.Name, ""); e != nil {
			return e
		} else if pending {
			out.Editable = false
			out.BlockedReason = "An interrupted edit needs host reconciliation; only explicit historical resume may use the old snapshot."
		}
		if pending, e := publicationPending(ctx, p.tx, p.source.identity, ref.Name); e != nil {
			return e
		} else if pending {
			out.Editable = false
			out.BlockedReason = "An interrupted publication needs host reconciliation."
		}
		return nil
	})
	return
}

// PrepareEdit stages immutable bytes only. It never writes a source directory,
// changes a live pointer, runs scripts, or grants permission to apply.
func (r *Registry) PrepareEdit(ctx context.Context, ref harness.SkillRef, owner, key string, edits []harness.SkillFileEdit) (review harness.SkillEditReview, err error) {
	if !validManagementText(owner, 256) || !validManagementText(key, 256) {
		return review, invalid("edit owner and request key are required")
	}
	intent, err := json.Marshal([]any{ref, edits})
	if err != nil || len(intent) > 256<<10 {
		return review, invalid("edit request exceeds limit")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return review, ErrClosed
	}
	s := r.sources[ref.SourceID]
	if s == nil || s.identity != ref.SourceIdentity {
		return review, harness.ErrPermissionDenied
	}
	err = r.withPublication(ctx, s, ref.Name, func(p *PublicationTx) error {
		var existing string
		lookup := p.tx.QueryRowContext(ctx, `SELECT id FROM df_skill_edits WHERE owner=? AND request_key=?`, owner, key).Scan(&existing)
		if lookup == nil {
			rec, err := readEdit(ctx, p.tx, existing)
			if err != nil {
				return err
			}
			if rec.Intent != digest(intent) {
				return harness.ErrSkillConflict
			}
			review = rec.Review
			return nil
		}
		if !errors.Is(lookup, sql.ErrNoRows) {
			return lookup
		}
		var retained int
		if err := p.tx.QueryRowContext(ctx, `SELECT count(*) FROM df_skill_edits WHERE source_key=? AND name=?`, s.identity, ref.Name).Scan(&retained); err != nil {
			return err
		}
		if retained >= 256 {
			return invalid("retained edit journal limit reached; host maintenance required")
		}
		if pending, e := publicationPending(ctx, p.tx, s.identity, ref.Name); e != nil {
			return e
		} else if pending {
			return harness.ErrReconciliationRequired
		}
		if pending, e := editPending(ctx, p.tx, s.identity, ref.Name, ""); e != nil {
			return e
		} else if pending {
			return harness.ErrReconciliationRequired
		}
		head, err := p.Current()
		if err != nil {
			return err
		}
		if !head.Exists || head.Version != ref.Version {
			return harness.ErrSkillConflict
		}
		mode, dir, err := p.editStorage(head)
		if err != nil {
			return err
		}
		if mode != "source" {
			return denied("skill has no authorized editable installation")
		}
		base, err := p.ReadVersion(head.Version)
		if err != nil {
			return err
		}
		if mode == "source" {
			disk, err := r.readPackage(ctx, s, dir)
			if err != nil {
				return err
			}
			if (&Candidate{pkg: disk}).Version() != head.Version {
				return harness.ErrSkillConflict
			}
		}
		files, changes, err := applyFileEdits(base.Files(), edits)
		if err != nil {
			return err
		}
		pkg, err := packageFromFiles(ref.Name, files, r.limits)
		if err != nil {
			return err
		}
		candidate := &Candidate{pkg: pkg}
		if _, err = p.Stage(candidate); err != nil {
			return err
		}
		review = harness.SkillEditReview{ID: rand.Text(), Target: p.target, Base: head, Candidate: candidate.Version(), Storage: mode, Changes: changes, Behavior: "not_run", Effective: "next ordinary turn; current execution and resume stay pinned"}
		if mode == "source" {
			review.SourceDirectory = filepath.Join(s.config.Root, filepath.FromSlash(dir))
		}
		return saveEdit(ctx, p.tx, editRecord{Review: review, Owner: owner, Key: key, Intent: digest(intent), Dir: dir, Status: "prepared"})
	})
	return
}

func (r *Registry) EditReview(ctx context.Context, id, owner string) (harness.SkillEditReview, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return harness.SkillEditReview{}, ErrClosed
	}
	rec, err := readEdit(ctx, r.db, id)
	if err != nil {
		return rec.Review, err
	}
	s := r.sources[rec.Review.Target.SourceID]
	if rec.Owner != owner || s == nil || sourceTarget(s, rec.Review.Target.Name) != rec.Review.Target {
		return harness.SkillEditReview{}, harness.ErrPermissionDenied
	}
	return rec.Review, nil
}

func (r *Registry) EditStatus(ctx context.Context, id, owner string) (out harness.SkillEditResult, err error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return out, ErrClosed
	}
	rec, err := readEdit(ctx, r.db, id)
	if err != nil {
		return out, err
	}
	s := r.sources[rec.Review.Target.SourceID]
	if s == nil || rec.Owner != owner || sourceTarget(s, rec.Review.Target.Name) != rec.Review.Target {
		return out, harness.ErrPermissionDenied
	}
	return r.editResult(ctx, s, rec)
}

func (r *Registry) editResult(ctx context.Context, s *source, rec editRecord) (out harness.SkillEditResult, err error) {
	out = harness.SkillEditResult{Review: rec.Review, Status: rec.Status, AppliedHead: rec.Applied, ErrorCode: rec.Error, BackupRelative: "registry-version:" + rec.Review.Base.Version}
	if rec.Review.Storage == "source" {
		out.BackupRelative = ".deerflow-edit-backups/" + rec.Review.ID
	}
	err = r.withEditPublication(ctx, s, rec.Review.Target.Name, rec.Review.ID, func(p *PublicationTx) error {
		var err error
		out.CurrentHead, err = p.Current()
		return err
	})
	if err != nil {
		return out, err
	}
	out.LiveMatches = rec.Status == "applied" && out.CurrentHead == rec.Applied
	if rec.Review.Storage == "source" {
		pkg, e := r.readPackage(ctx, s, rec.Dir)
		out.SourceMatches = e == nil && (&Candidate{pkg: pkg}).Version() == rec.Review.Candidate
	}
	return
}

// ApplyEdit is a host API. Model-facing callers must bind this exact immutable
// review to an approved tool invocation before entering it.
func (r *Registry) ApplyEdit(ctx context.Context, id, owner string, approved harness.SkillEditReview) (out harness.SkillEditResult, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return out, ErrClosed
	}
	rec, err := readEdit(ctx, r.db, id)
	if err != nil {
		return out, err
	}
	a, _ := json.Marshal(approved)
	b, _ := json.Marshal(rec.Review)
	s := r.sources[rec.Review.Target.SourceID]
	if rec.Owner != owner || string(a) != string(b) || s == nil || sourceTarget(s, rec.Review.Target.Name) != rec.Review.Target {
		return out, harness.ErrPermissionDenied
	}
	if rec.Status == "applied" || rec.Status == "conflict" {
		return r.editResult(ctx, s, rec)
	}
	if rec.Status != "prepared" {
		return r.editResult(ctx, s, rec)
	}
	err = r.withEditPublication(ctx, s, rec.Review.Target.Name, id, func(p *PublicationTx) error {
		fresh, err := readEdit(ctx, p.tx, id)
		if err != nil {
			return err
		}
		rec = fresh
		if rec.Status != "prepared" {
			return nil
		}
		if pending, e := publicationPending(ctx, p.tx, s.identity, rec.Review.Target.Name); e != nil {
			return e
		} else if pending {
			return harness.ErrReconciliationRequired
		}
		if pending, err := editPending(ctx, p.tx, s.identity, rec.Review.Target.Name, id); err != nil {
			return err
		} else if pending {
			return harness.ErrReconciliationRequired
		}
		head, err := p.Current()
		if err != nil {
			return err
		}
		if head != rec.Review.Base {
			return harness.ErrSkillConflict
		}
		mode, dir, err := p.editStorage(head)
		if err != nil {
			return err
		}
		if mode != rec.Review.Storage || dir != rec.Dir {
			return harness.ErrSkillConflict
		}
		if mode == "source" {
			pkg, err := r.readPackage(ctx, s, dir)
			if err != nil {
				return err
			}
			if (&Candidate{pkg: pkg}).Version() != head.Version {
				return harness.ErrSkillConflict
			}
		}
		rec.Status = "applying"
		return saveEdit(ctx, p.tx, rec)
	})
	if err != nil {
		return out, err
	}
	if rec.Status != "applying" {
		return r.editResult(ctx, s, rec)
	}
	if err = r.finishEdit(ctx, s, &rec); err != nil {
		// Preserve the durable applying journal; startup recovery must not import
		// a partially written directory or silently replay an unapproved edit.
		return out, err
	}
	return r.editResult(ctx, s, rec)
}

func (r *Registry) finishEdit(ctx context.Context, s *source, rec *editRecord) error {
	err := r.withEditPublication(ctx, s, rec.Review.Target.Name, rec.Review.ID, func(p *PublicationTx) error {
		// Hold the catalog write lock across filesystem I/O, also excluding a
		// second Registry/process recovering the same admitted edit.
		fresh, err := readEdit(ctx, p.tx, rec.Review.ID)
		if err != nil {
			return err
		}
		*rec = fresh
		if rec.Status != "applying" {
			return nil
		}
		if s.builtin {
			return denied("builtin source policy now forbids this admitted edit")
		}
		head, err := p.Current()
		if err != nil {
			return err
		}
		if head != rec.Review.Base {
			return harness.ErrSkillConflict
		}
		base, err := p.ReadVersion(rec.Review.Base.Version)
		if err != nil {
			return err
		}
		candidate, err := p.ReadVersion(rec.Review.Candidate)
		if err != nil {
			return err
		}
		if rec.Review.Storage == "source" {
			if err = r.applyEditFiles(ctx, s, *rec, base, candidate); err != nil {
				return err
			}
			rec.Applied, err = p.moveHead(head, rec.Review.Candidate, head.Enabled, head.Managed, rec.Review.Candidate, rec.Dir)
		} else {
			return denied("database-only edits are no longer supported")
		}
		if err != nil {
			return err
		}
		rec.Status, rec.Error = "applied", ""
		if err = p.recordSourceStatus(rec.Dir, "ready", rec.Review.Candidate, ""); err != nil {
			return err
		}
		return saveEdit(ctx, p.tx, *rec)
	})
	if err != nil {
		persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return r.withEditPublication(persist, s, rec.Review.Target.Name, rec.Review.ID, func(p *PublicationTx) error {
			fresh, e := readEdit(persist, p.tx, rec.Review.ID)
			if e != nil {
				return e
			}
			*rec = fresh
			if rec.Status != "applying" {
				return nil
			}
			rec.Status, rec.Error = "needs_reconciliation", "source_or_head_conflict"
			return saveEdit(persist, p.tx, *rec)
		})
	}
	return nil
}

// RecoverEdits runs before startup imports. Only operations already admitted
// for application can be recovered; prepared drafts never write files.
func (r *Registry) RecoverEdits(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM df_skill_edits WHERE status='applying' ORDER BY id`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		rec, err := readEdit(ctx, r.db, id)
		if err != nil {
			return err
		}
		s := r.sources[rec.Review.Target.SourceID]
		if s == nil || sourceTarget(s, rec.Review.Target.Name) != rec.Review.Target {
			continue
		}
		if err = r.finishEdit(ctx, s, &rec); err != nil {
			return err
		}
	}
	return nil
}

func editPending(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, source, name, allowedID string) (bool, error) {
	var pending bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_edits WHERE source_key=? AND name=? AND status IN ('applying','needs_reconciliation') AND id<>?)`, source, name, allowedID).Scan(&pending)
	return pending, err
}

func validEditID(id string) bool {
	return len(id) == 26 && strings.IndexFunc(id, func(r rune) bool { return (r < 'A' || r > 'Z') && (r < '2' || r > '7') }) < 0
}
