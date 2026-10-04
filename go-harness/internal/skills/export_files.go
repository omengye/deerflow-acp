package skills

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type exportManifest struct {
	Schema int                 `json:"schema"`
	Target harness.SkillTarget `json:"target"`
	Head   harness.SkillHead   `json:"head"`
}

func exportDirectoryPrefix(target harness.SkillTarget, head harness.SkillHead) string {
	return fmt.Sprintf(".deerflow-exports/v1/%s/%s/%020d-", target.SourceIdentity, target.Name, head.StateVersion)
}

func validExportDirectory(target harness.SkillTarget, head harness.SkillHead, directory string) bool {
	prefix := exportDirectoryPrefix(target, head)
	if !validRelative(directory) || !strings.HasPrefix(directory, prefix) {
		return false
	}
	suffix := strings.TrimPrefix(directory, prefix)
	return len(suffix) >= 20 && len(suffix) <= 64 && strings.IndexFunc(suffix, func(c rune) bool {
		return !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9')
	}) < 0
}

// openExportDir pins every component after rejecting links. Only callers that
// create a fresh random view may write package files. Existing views are never
// edited in place, so a user's disk changes cannot be imported or overwritten.
func openExportDir(root *os.Root, relative string, create bool) (*os.Root, error) {
	if relative != "." && !validRelative(relative) {
		return nil, denied("invalid export directory")
	}
	current, err := root.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	if relative == "." {
		return current, nil
	}
	for _, part := range strings.Split(relative, "/") {
		if create {
			if err = current.Mkdir(part, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				_ = current.Close()
				return nil, err
			}
		}
		info, statErr := current.Lstat(part)
		if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			_ = current.Close()
			return nil, denied("export directory contains a link or invalid component")
		}
		next, openErr := current.OpenRoot(part)
		_ = current.Close()
		if openErr != nil {
			return nil, openErr
		}
		actual, statErr := next.Stat(".")
		if statErr != nil || !os.SameFile(info, actual) {
			_ = next.Close()
			return nil, denied("export directory changed during open")
		}
		current = next
	}
	return current, nil
}

func (r *Registry) materializeExport(ctx context.Context, state harness.SkillExportState, candidate *Candidate) (directory string, err error) {
	manifest, _ := json.Marshal(exportManifest{Schema: 1, Target: state.Target, Head: state.Head})
	if state.Directory != "" && r.verifyExport(ctx, state.Directory, manifest, candidate) == nil {
		return state.Directory, nil
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	directory = exportDirectoryPrefix(state.Target, state.Head) + rand.Text()
	if !validExportDirectory(state.Target, state.Head, directory) {
		return "", invalid("generated export directory is invalid")
	}
	parent, err := openExportDir(r.export.root, path.Dir(directory), true)
	if err != nil {
		return "", err
	}
	defer parent.Close()
	if err = parent.Mkdir(path.Base(directory), 0700); err != nil {
		return "", err
	}
	view, err := openExportDir(parent, path.Base(directory), false)
	if err != nil {
		return "", err
	}
	created := directory
	defer func() {
		_ = view.Close()
		if err != nil {
			// The checked random child is resolved through its pinned parent,
			// including if an ancestor path was renamed during materialization.
			// No import directory is ever removed here.
			_ = parent.RemoveAll(path.Base(created))
		}
	}()
	if candidate != nil {
		for _, entry := range candidate.pkg.manifest {
			if err = ctx.Err(); err != nil {
				return "", err
			}
			if err = writeExportFile(view, "files/"+entry.Path, candidate.pkg.files[entry.Path]); err != nil {
				return "", err
			}
		}
	}
	if err = writeExportFile(view, "publication.json", manifest); err != nil {
		return "", err
	}
	// Only a complete verified view can be attached to the current SQL head.
	if err = r.verifyExport(ctx, directory, manifest, candidate); err != nil {
		return "", err
	}
	return directory, nil
}

func writeExportFile(root *os.Root, name string, data []byte) error {
	if !validRelative(name) {
		return denied("invalid export file path")
	}
	dir, err := openExportDir(root, path.Dir(name), true)
	if err != nil {
		return err
	}
	defer dir.Close()
	f, err := dir.OpenFile(path.Base(name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	n, err := f.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

func (r *Registry) verifyExport(ctx context.Context, directory string, manifest []byte, candidate *Candidate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	view, err := openExportDir(r.export.root, directory, false)
	if err != nil {
		return err
	}
	defer view.Close()
	entries, err := view.Open(".")
	if err != nil {
		return err
	}
	names, readErr := entries.Readdirnames(3)
	_ = entries.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) || len(names) != 1 && candidate == nil || len(names) != 2 && candidate != nil {
		return errors.New("export view has unexpected entries")
	}
	info, err := view.Lstat("publication.json")
	if err != nil || !info.Mode().IsRegular() || info.Size() != int64(len(manifest)) {
		return errors.New("export manifest is missing or changed")
	}
	f, err := view.Open("publication.json")
	if err != nil {
		return err
	}
	actual, statErr := f.Stat()
	if statErr != nil || !os.SameFile(info, actual) {
		_ = f.Close()
		return errors.New("export manifest changed during open")
	}
	data, readErr := io.ReadAll(io.LimitReader(f, int64(len(manifest)+1)))
	_ = f.Close()
	if readErr != nil || !bytes.Equal(data, manifest) {
		return errors.New("export manifest does not match publication")
	}
	if candidate == nil {
		return nil
	}
	files, err := openExportDir(view, "files", false)
	if err != nil {
		return err
	}
	defer files.Close()
	pkg, err := r.readPackage(ctx, &source{root: files}, ".")
	if err != nil {
		return err
	}
	if (&Candidate{pkg: pkg}).Version() != candidate.Version() {
		return errors.New("export content does not match publication")
	}
	return nil
}
