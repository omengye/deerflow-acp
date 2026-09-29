package client

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var privateSessionDir = regexp.MustCompile(`^[a-f0-9]{64}$`)

// CleanupOrphans repairs a crash between the parent session's SQL deletion
// and its private filesystem cleanup. Database session IDs are authoritative.
func CleanupOrphans(ctx context.Context, root string, db *sql.DB) error {
	if db == nil {
		return errors.New("external ACP orphan scan requires a database")
	}
	rows, err := db.QueryContext(ctx, `SELECT id FROM harness_sessions`)
	if err != nil {
		return err
	}
	known := make(map[string]bool)
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		identity := sha256.Sum256([]byte(id))
		known[fmt.Sprintf("%x", identity[:])] = true
	}
	if err == nil {
		err = rows.Err()
	}
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	for _, category := range []string{"acp-workspaces", "acp-agent-sessions"} {
		base := filepath.Join(root, category)
		if _, statErr := os.Stat(base); errors.Is(statErr, os.ErrNotExist) {
			continue
		} else if statErr != nil {
			return statErr
		}
		resolvedRoot, rootErr := filepath.EvalSymlinks(root)
		resolvedBase, baseErr := filepath.EvalSymlinks(base)
		if rootErr != nil {
			return rootErr
		}
		if baseErr != nil {
			return baseErr
		}
		rel, relErr := filepath.Rel(resolvedRoot, resolvedBase)
		if relErr != nil || rel != category {
			return errors.New("external ACP private directory escapes state root")
		}
		directory, openErr := os.Open(base)
		if openErr != nil {
			return openErr
		}
		for {
			entries, readErr := directory.ReadDir(256)
			for _, entry := range entries {
				if !privateSessionDir.MatchString(entry.Name()) || known[entry.Name()] {
					continue
				}
				if err = ctx.Err(); err != nil {
					break
				}
				if err = removePrivateTree(base, filepath.Join(base, entry.Name())); err != nil {
					break
				}
			}
			if err != nil || errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				err = readErr
				break
			}
		}
		closeErr := directory.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

// CleanupSession removes only this host's private external ACP session state
// after the parent session was durably deleted and all of its runs joined.
func CleanupSession(root, sessionID string) error {
	if sessionID == "" {
		return errors.New("external ACP cleanup requires a session ID")
	}
	identity := sha256.Sum256([]byte(sessionID))
	segment := fmt.Sprintf("%x", identity[:])
	var result error
	for _, category := range []string{"acp-workspaces", "acp-agent-sessions"} {
		base := filepath.Join(root, category)
		target := filepath.Join(base, segment)
		if err := removePrivateTree(base, target); err != nil {
			result = errors.Join(result, err)
		}
	}
	if result == nil {
		prefix := filepath.Join(root, "acp-agent-sessions", segment) + string(filepath.Separator)
		invocationLocks.Range(func(key, _ any) bool {
			if path, ok := key.(string); ok && strings.HasPrefix(path, prefix) {
				invocationLocks.Delete(key)
			}
			return true
		})
	}
	return result
}

func removePrivateTree(base, target string) error {
	rootAbs, err := filepath.Abs(filepath.Dir(base))
	if err != nil {
		return err
	}
	rootAbs, err = filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return err
	}
	baseAbs, err := filepath.Abs(base)
	if err != nil {
		return err
	}
	if _, err = os.Stat(baseAbs); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	baseAbs, err = filepath.EvalSymlinks(baseAbs)
	if err != nil {
		return err
	}
	if rel, relErr := filepath.Rel(rootAbs, baseAbs); relErr != nil || rel != filepath.Base(base) {
		return errors.New("external ACP private directory escapes state root")
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	if _, err = os.Lstat(targetAbs); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	targetAbs, err = filepath.EvalSymlinks(targetAbs)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(baseAbs, targetAbs)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return errors.New("external ACP cleanup target escapes its private directory")
	}
	return os.RemoveAll(targetAbs)
}
