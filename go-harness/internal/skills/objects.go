package skills

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func (r *Registry) openObjects(ctx context.Context, directory string) error {
	var saved string
	err := r.db.QueryRowContext(ctx, `SELECT directory FROM df_skill_object_root WHERE id=1`).Scan(&saved)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if directory == "" {
		directory = saved
	}
	if directory == "" {
		var seq int
		var name, file string
		if err = r.db.QueryRowContext(ctx, `PRAGMA database_list`).Scan(&seq, &name, &file); err != nil {
			return err
		}
		if file == "" {
			return invalid("in-memory registries require snapshotRoot")
		}
		directory = filepath.Join(filepath.Dir(file), "skill-snapshots-v1")
	}
	abs, err := filepath.Abs(directory)
	if err != nil {
		return err
	}
	if saved != "" && filepath.Clean(saved) != abs {
		return invalid("snapshotRoot changed; move historical objects explicitly before changing it")
	}
	if err = os.MkdirAll(abs, 0700); err != nil {
		return err
	}
	if err = noSymlinkPath(abs); err != nil {
		return err
	}
	r.objects, err = os.OpenRoot(abs)
	if err != nil {
		return err
	}
	r.objectsPath = abs
	_, err = r.db.ExecContext(ctx, `INSERT OR IGNORE INTO df_skill_object_root VALUES(1,?)`, abs)
	return err
}

func objectName(sha string) string { return "objects/" + sha }

func (r *Registry) storeObject(data []byte) error {
	sha := digest(data)
	dir, err := openExportDir(r.objects, "objects", true)
	if err != nil {
		return err
	}
	defer dir.Close()
	if info, err := dir.Lstat(sha); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return denied("snapshot object is not a regular file")
		}
		got, err := dir.ReadFile(sha)
		if err != nil {
			return err
		}
		if digest(got) != sha {
			return invalid("snapshot object integrity failure")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp := ".stage-" + rand.Text()
	f, err := dir.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err = secureSnapshotFile(f); err != nil {
		_ = f.Close()
		_ = dir.Remove(tmp)
		return err
	}
	n, err := f.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	defer dir.Remove(tmp)
	if err != nil {
		return err
	}
	if err = dir.Link(tmp, sha); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	got, err := dir.ReadFile(sha)
	if err != nil {
		return err
	}
	if digest(got) != sha {
		return invalid("snapshot object integrity failure")
	}
	return nil
}

type fileQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// ReadStoredFile reads only an explicitly pinned historical/candidate version.
// New file bodies live in the object directory, not SQL. Legacy SQL bytes are
// readable for old checkpoints, never selected as a fallback for missing files.
func ReadStoredFile(ctx context.Context, q fileQuery, source, name, version, path string, max int64) ([]byte, error) {
	var data []byte
	var sha string
	var size int64
	var object bool
	err := q.QueryRowContext(ctx, `SELECT f.data,f.hash,f.size,EXISTS(SELECT 1 FROM df_skill_file_objects o WHERE o.source_key=f.source_key AND o.name=f.name AND o.version=f.version AND o.path=f.path) FROM df_skill_files f WHERE f.source_key=? AND f.name=? AND f.version=? AND f.path=?`, source, name, version, path).Scan(&data, &sha, &size, &object)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, harness.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !validHash(sha) || size < 0 || size > max {
		return nil, invalid("snapshot file exceeds bounds")
	}
	if object {
		if len(data) != 0 {
			return nil, invalid("file-object index contains unexpected inline content")
		}
		var directory string
		if err = q.QueryRowContext(ctx, `SELECT directory FROM df_skill_object_root WHERE id=1`).Scan(&directory); err != nil {
			return nil, err
		}
		if err = noSymlinkPath(directory); err != nil {
			return nil, err
		}
		root, err := os.OpenRoot(directory)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		dir, err := openExportDir(root, "objects", false)
		if err != nil {
			return nil, err
		}
		defer dir.Close()
		info, err := dir.Lstat(sha)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, invalid("snapshot object missing or invalid")
		}
		f, err := dir.Open(sha)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		data, err = io.ReadAll(io.LimitReader(f, max+1))
		if err != nil {
			return nil, err
		}
	}
	if int64(len(data)) != size || digest(data) != sha {
		return nil, invalid("snapshot file integrity failure")
	}
	return data, nil
}
