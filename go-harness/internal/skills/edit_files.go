package skills

import (
	"context"
	"errors"
	"io"
	"os"
	"path"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// Staging names are outside the registered package, never part of a snapshot.
// Every final directory is pinned after rejecting links. Unexpected disk bytes
// always stop recovery rather than overwriting a later user's edit.
func (r *Registry) applyEditFiles(ctx context.Context, s *source, rec editRecord, base, candidate *Candidate) error {
	if err := validateSourceRoot(s); err != nil {
		return err
	}
	if rec.Dir == "." || !validRelative(rec.Dir) || !validEditID(rec.Review.ID) {
		return invalid("source editing requires a registered package subdirectory")
	}
	// A crash between preserving the old file and linking the replacement can
	// leave a target absent. Recover only a journal-owned, verified old inode.
	for _, change := range rec.Review.Changes {
		parent := path.Join(rec.Dir, path.Dir(change.Path))
		target := path.Join(parent, path.Base(change.Path))
		if _, err := s.root.Lstat(target); errors.Is(err, os.ErrNotExist) {
			dir, err := openExportDir(s.root, parent, false)
			if err != nil {
				return err
			}
			err = r.replaceEditFile(ctx, s, dir, parent, path.Base(change.Path), rec.Review.ID, base.pkg.files[change.Path], candidate.pkg.files[change.Path])
			_ = dir.Close()
			if err != nil {
				return err
			}
		}
	}
	pkg, err := r.readPackage(ctx, s, rec.Dir)
	if err != nil {
		return err
	}
	if len(pkg.files) != len(base.pkg.files) {
		return harness.ErrSkillConflict
	}
	for name, old := range base.pkg.files {
		actual, exists := pkg.files[name]
		if !exists || digest(actual) != digest(old) && digest(actual) != digest(candidate.pkg.files[name]) {
			return harness.ErrSkillConflict
		}
	}
	for _, change := range rec.Review.Changes {
		if err := ctx.Err(); err != nil {
			return err
		}
		parent := path.Join(rec.Dir, path.Dir(change.Path))
		dir, err := openExportDir(s.root, parent, false)
		if err != nil {
			return err
		}
		err = r.replaceEditFile(ctx, s, dir, parent, path.Base(change.Path), rec.Review.ID, base.pkg.files[change.Path], candidate.pkg.files[change.Path])
		_ = dir.Close()
		if err != nil {
			return err
		}
	}
	pkg, err = r.readPackage(ctx, s, rec.Dir)
	if err != nil {
		return err
	}
	if (&Candidate{pkg: pkg}).Version() != rec.Review.Candidate {
		return harness.ErrSkillConflict
	}
	return nil
}

func (r *Registry) replaceEditFile(ctx context.Context, s *source, dir *os.Root, parent, name, id string, before, after []byte) error {
	read := func() ([]byte, os.FileInfo, error) {
		info, err := dir.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, err
		}
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, nil, denied("edit target is not a regular file")
		}
		f, err := dir.Open(name)
		if err != nil {
			return nil, nil, err
		}
		defer f.Close()
		actual, err := f.Stat()
		if err != nil || !os.SameFile(info, actual) {
			return nil, nil, harness.ErrSkillConflict
		}
		data, err := io.ReadAll(io.LimitReader(f, r.limits.MaxFileBytes+1))
		if err != nil {
			return nil, nil, err
		}
		return data, actual, nil
	}
	current, info, err := read()
	missing := errors.Is(err, os.ErrNotExist)
	if err != nil && !missing {
		return err
	}
	filename := digest([]byte(parent + "/" + name))
	backupName := ".deerflow-edit-backups/" + id
	backup, err := openExportDir(s.root, backupName, true)
	if err != nil {
		return err
	}
	defer backup.Close()
	verifyBackup := func() error {
		st, err := backup.Lstat(filename)
		if err != nil || !st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0 {
			return harness.ErrSkillConflict
		}
		data, err := backup.ReadFile(filename)
		if err != nil || digest(data) != digest(before) {
			return harness.ErrSkillConflict
		}
		return nil
	}
	if !missing && digest(current) == digest(after) {
		if _, err := backup.Lstat(filename); err == nil {
			return verifyBackup()
		}
		return nil
	}
	if missing {
		if err := verifyBackup(); err != nil {
			return err
		}
		info, err = backup.Stat(filename)
		if err != nil {
			return err
		}
	} else if digest(current) != digest(before) {
		return harness.ErrSkillConflict
	}
	stageName := ".deerflow-edit-stage/" + id
	stage, err := openExportDir(s.root, stageName, true)
	if err != nil {
		return err
	}
	defer stage.Close()
	f, err := stage.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if errors.Is(err, os.ErrExist) {
		// An incomplete staged file is disposable; never remove a user path.
		st, e := stage.Lstat(filename)
		if e != nil || !st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0 {
			return harness.ErrSkillConflict
		}
		if err = stage.Remove(filename); err != nil {
			return err
		}
		f, err = stage.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	}
	if err != nil {
		return err
	}
	securityRoot, securityName := dir, name
	if missing {
		securityRoot, securityName = backup, filename
	}
	if err = preserveEditSecurity(securityRoot, securityName, f); err != nil {
		_ = f.Close()
		return err
	}
	n, err := f.Write(after)
	if err == nil && n != len(after) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if !missing {
		current, latest, err := read()
		if err != nil || !os.SameFile(info, latest) || digest(current) != digest(before) {
			return harness.ErrSkillConflict
		}
	}
	// Revalidate the rooted path against the pinned directory before rename.
	check, err := openExportDir(s.root, parent, false)
	if err != nil {
		return err
	}
	a, ea := check.Stat(".")
	b, eb := dir.Stat(".")
	_ = check.Close()
	if ea != nil || eb != nil || !os.SameFile(a, b) {
		return harness.ErrSkillConflict
	}
	if !missing {
		if _, err := backup.Lstat(filename); !errors.Is(err, os.ErrNotExist) {
			return harness.ErrSkillConflict
		}
		// Preserve the exact displaced file (including a racing editor's bytes)
		// instead of replacing/deleting it. Link below is exclusive/no-clobber.
		if err := renameNoReplace(s.root, path.Join(parent, name), path.Join(backupName, filename)); err != nil {
			return err
		}
		if err := verifyBackup(); err != nil {
			_ = s.root.Link(path.Join(backupName, filename), path.Join(parent, name))
			return err
		}
	}
	if err := s.root.Link(path.Join(stageName, filename), path.Join(parent, name)); err != nil {
		return err
	}
	if err := verifyBackup(); err != nil {
		return err
	}
	_ = stage.Remove(filename)
	// Only remove our now-empty staging directory; never recursively delete.
	_ = stage.Close()
	_ = s.root.Remove(stageName)
	return nil
}
