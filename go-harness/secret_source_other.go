//go:build !windows

package deerflow

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// Claim a private sibling before reading. A producer can rotate the original
// pathname at any point. Rollback links back only into an unoccupied name.
func claimImportSource(parent *os.Root, base string) (s *importSource, retErr error) {
	defer func() {
		if retErr != nil {
			parent.Close()
		}
	}()
	before, err := parent.Lstat(base)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: delete_source requires an ordinary file", harness.ErrInvalidInput)
	}
	dir := ".deerflow-import-" + rand.Text()
	if err = parent.Mkdir(dir, 0700); err != nil {
		return nil, err
	}
	held := filepath.Join(dir, base)
	if err = parent.Rename(base, held); err != nil {
		parent.Remove(dir)
		return nil, err
	}
	restore := func() error {
		if err := parent.Link(held, base); err != nil {
			return fmt.Errorf("source retained at %s: %w", held, err)
		}
		if err := parent.Remove(held); err != nil {
			return err
		}
		return parent.Remove(dir)
	}
	claimed, err := parent.Lstat(held)
	if err != nil || !claimed.Mode().IsRegular() || !os.SameFile(before, claimed) {
		return nil, errors.Join(errors.New("source changed during import"), restore())
	}
	f, err := parent.Open(held)
	if err != nil {
		return nil, errors.Join(err, restore())
	}
	info, err := f.Stat()
	if err != nil || !os.SameFile(before, info) {
		f.Close()
		return nil, errors.Join(errors.New("source changed during import"), restore())
	}
	return &importSource{f, base, func(commit bool) error {
		defer parent.Close()
		final, statErr := f.Stat()
		closeErr := f.Close()
		if !commit || statErr != nil || final.Size() != info.Size() || !final.ModTime().Equal(info.ModTime()) {
			if commit {
				return errors.Join(errors.New("source changed; kept for recovery"), restore(), closeErr)
			}
			return errors.Join(restore(), closeErr)
		}
		if err := parent.Remove(held); err != nil {
			return errors.Join(err, closeErr)
		}
		return errors.Join(parent.Remove(dir), closeErr)
	}}, nil
}
