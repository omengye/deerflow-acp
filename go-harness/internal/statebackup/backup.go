// Package statebackup snapshots an offline harness database and immutable assets.
// Configuration, installed skill sources and desktop history remain separate.
package statebackup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	_ "modernc.org/sqlite"
)

const version = "deerflow-state-v1"
const manifestName = "manifest.json"

type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type Manifest struct {
	Version   string    `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	Files     []File    `json:"files"`
}

func realDir(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return "", errors.New("directory must be regular")
	}
	return resolved, nil
}

func newDestination(path, source string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	parent, err := realDir(filepath.Dir(abs))
	if err != nil {
		return "", fmt.Errorf("destination parent must exist: %w", err)
	}
	abs = filepath.Join(parent, filepath.Base(abs))
	rel, err := filepath.Rel(source, abs)
	if err != nil && strings.EqualFold(filepath.VolumeName(source), filepath.VolumeName(abs)) {
		return "", err
	}
	if err == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel)) {
		return "", errors.New("destination cannot be inside source")
	}
	if err = os.Mkdir(abs, 0700); err != nil {
		return "", fmt.Errorf("destination must be a new directory: %w", err)
	}
	return abs, nil
}

func regular(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("snapshot files must be regular; links are rejected")
	}
	return nil
}

func openDB(ctx context.Context, path string) (*sql.DB, error) {
	if err := regular(path); err != nil {
		return nil, err
	}
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	u := url.URL{Scheme: "file", Path: p}
	q := url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(5000)"}}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func integrity(ctx context.Context, path string) error {
	db, err := openDB(ctx, path)
	if err != nil {
		return err
	}
	defer db.Close()
	var result string
	if err = db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return errors.New("SQLite integrity check failed")
	}
	return nil
}

func digest(ctx context.Context, path, relative string) (File, error) {
	if err := regular(path); err != nil {
		return File{}, err
	}
	in, err := os.Open(path)
	if err != nil {
		return File{}, err
	}
	defer in.Close()
	h := sha256.New()
	n, err := io.Copy(h, &contextReader{ctx: ctx, reader: in})
	if err != nil {
		return File{}, err
	}
	return File{Path: filepath.ToSlash(relative), Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func copyFile(ctx context.Context, source, destination, relative string) (File, error) {
	if err := regular(source); err != nil {
		return File{}, err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return File{}, err
	}
	in, err := os.Open(source)
	if err != nil {
		return File{}, err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return File{}, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), &contextReader{ctx: ctx, reader: in})
	if err == nil {
		err = out.Sync()
	}
	err = errors.Join(err, out.Close())
	if err != nil {
		return File{}, err
	}
	return File{Path: relative, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// Backup holds the same process lock as the native host, so a stopped daemon
// cannot restart during snapshotting. VACUUM INTO also incorporates a leftover WAL.
func Backup(ctx context.Context, dataDir, destination string) (Manifest, error) {
	source, err := realDir(dataDir)
	if err != nil {
		return Manifest{}, err
	}
	if err = regular(filepath.Join(source, "harness.db")); err != nil {
		return Manifest{}, err
	}
	lock := flock.New(filepath.Join(source, "runtime.lock"))
	locked, err := lock.TryLock()
	if err != nil {
		lock.Close()
		return Manifest{}, err
	}
	if !locked {
		lock.Close()
		return Manifest{}, errors.New("harness is running; drain and stop it before backup")
	}
	defer lock.Close()
	assets := filepath.Join(source, "assets")
	if _, err = os.Lstat(assets); err == nil {
		info, e := os.Lstat(assets)
		if e != nil || !info.IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return Manifest{}, errors.New("assets must be a regular directory")
		}
		assetLock := flock.New(filepath.Join(assets, ".store.lock"))
		locked, e := assetLock.TryLock()
		if e != nil || !locked {
			assetLock.Close()
			return Manifest{}, errors.New("asset store is active")
		}
		defer assetLock.Close()
	} else if !errors.Is(err, os.ErrNotExist) {
		return Manifest{}, err
	}
	out, err := newDestination(destination, source)
	if err != nil {
		return Manifest{}, err
	}
	db, err := openDB(ctx, filepath.Join(source, "harness.db"))
	if err != nil {
		return Manifest{}, err
	}
	_, err = db.ExecContext(ctx, "VACUUM INTO ?", filepath.Join(out, "harness.db"))
	err = errors.Join(err, db.Close())
	if err != nil {
		return Manifest{}, err
	}
	if err = os.Chmod(filepath.Join(out, "harness.db"), 0600); err != nil {
		return Manifest{}, err
	}
	m := Manifest{Version: version, CreatedAt: time.Now().UTC()}
	entry, err := digest(ctx, filepath.Join(out, "harness.db"), "harness.db")
	if err != nil {
		return Manifest{}, err
	}
	m.Files = append(m.Files, entry)
	err = filepath.WalkDir(assets, func(path string, d fs.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) && path == assets {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if d.Type()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return errors.New("linked assets are rejected")
		}
		if d.IsDir() {
			return nil
		}
		if path == filepath.Join(assets, ".store.lock") {
			return nil
		}
		if len(m.Files) >= 100000 {
			return errors.New("snapshot file limit exceeded")
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		entry, err := copyFile(ctx, path, filepath.Join(out, rel), filepath.ToSlash(rel))
		if err == nil {
			m.Files = append(m.Files, entry)
		}
		return err
	})
	if err != nil {
		return Manifest{}, err
	}
	if err = integrity(ctx, filepath.Join(out, "harness.db")); err != nil {
		return Manifest{}, err
	}
	manifest, err := os.OpenFile(filepath.Join(out, manifestName), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return Manifest{}, err
	}
	err = json.NewEncoder(manifest).Encode(m)
	if err == nil {
		err = manifest.Sync()
	}
	err = errors.Join(err, manifest.Close())
	return m, err
}

func validPath(path string) bool {
	return path == "harness.db" || strings.HasPrefix(path, "assets/") && fs.ValidPath(path) && !strings.ContainsAny(path, `\:`)
}

// Verify requires every snapshot file to match its checksum and rejects extras.
func Verify(ctx context.Context, directory string) (Manifest, error) {
	root, err := realDir(directory)
	if err != nil {
		return Manifest{}, err
	}
	if err = regular(filepath.Join(root, manifestName)); err != nil {
		return Manifest{}, err
	}
	in, err := os.Open(filepath.Join(root, manifestName))
	if err != nil {
		return Manifest{}, err
	}
	dec := json.NewDecoder(io.LimitReader(in, 16<<20))
	dec.DisallowUnknownFields()
	var m Manifest
	err = dec.Decode(&m)
	if err == nil {
		var extra any
		if dec.Decode(&extra) != io.EOF {
			err = errors.New("invalid trailing manifest")
		}
	}
	in.Close()
	if err != nil {
		return Manifest{}, err
	}
	if m.Version != version || m.CreatedAt.IsZero() || len(m.Files) < 1 || len(m.Files) > 100000 {
		return Manifest{}, errors.New("unsupported snapshot manifest")
	}
	expected := map[string]File{}
	for _, entry := range m.Files {
		key := strings.ToLower(entry.Path)
		_, duplicate := expected[key]
		digestBytes, e := hex.DecodeString(entry.SHA256)
		if !validPath(entry.Path) || duplicate || entry.Size < 0 || e != nil || len(digestBytes) != 32 {
			return Manifest{}, errors.New("invalid snapshot entry")
		}
		expected[key] = entry
	}
	if _, exists := expected["harness.db"]; !exists {
		return Manifest{}, errors.New("snapshot database missing")
	}
	seen := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.Type()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return errors.New("linked snapshot entries are rejected")
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == manifestName {
			return nil
		}
		want, exists := expected[strings.ToLower(rel)]
		if !exists || want.Path != rel {
			return errors.New("unexpected snapshot file")
		}
		actual, err := digest(ctx, path, rel)
		if err != nil {
			return err
		}
		if actual != want {
			return errors.New("snapshot checksum mismatch")
		}
		seen++
		return nil
	})
	if err != nil {
		return Manifest{}, err
	}
	if seen != len(expected) {
		return Manifest{}, errors.New("snapshot files missing")
	}
	if err = integrity(ctx, filepath.Join(root, "harness.db")); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// Restore never overwrites an existing directory or active database.
func Restore(ctx context.Context, backupDir, destination string) (Manifest, error) {
	m, err := Verify(ctx, backupDir)
	if err != nil {
		return Manifest{}, err
	}
	source, err := realDir(backupDir)
	if err != nil {
		return Manifest{}, err
	}
	out, err := newDestination(destination, source)
	if err != nil {
		return Manifest{}, err
	}
	for _, entry := range m.Files {
		actual, err := copyFile(ctx, filepath.Join(source, filepath.FromSlash(entry.Path)), filepath.Join(out, filepath.FromSlash(entry.Path)), entry.Path)
		if err != nil {
			return Manifest{}, err
		}
		if actual != entry {
			return Manifest{}, errors.New("snapshot changed during restore")
		}
	}
	if err = integrity(ctx, filepath.Join(out, "harness.db")); err != nil {
		return Manifest{}, err
	}
	return m, nil
}
