package client

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

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
	if rel, relErr := filepath.Rel(rootAbs, baseAbs); relErr != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
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
