// Package diagnostics records bounded daemon lifecycle diagnostics. It never
// serializes configuration, endpoint tokens, user content or raw error text.
package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

type Daemon struct {
	mu     sync.Mutex
	file   *os.File
	path   string
	size   int64
	limit  int64
	failed bool
}

// Each process owns a separate file and at most two rotations; a competing
// daemon startup cannot truncate or rotate the active daemon's diagnostics.
func Open(directory string) (*Daemon, error) {
	directory, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(directory, fmt.Sprintf("deerflow-acpd-%d-*.jsonl", os.Getpid()))
	if err != nil {
		return nil, err
	}
	return &Daemon{file: file, path: file.Name(), limit: 2 << 20}, nil
}

func errorCode(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, os.ErrPermission), errors.Is(err, harness.ErrPermissionDenied):
		return "permission_denied"
	case errors.Is(err, os.ErrNotExist), errors.Is(err, harness.ErrNotFound):
		return "not_found"
	case errors.Is(err, harness.ErrInvalidInput):
		return "invalid_input"
	case errors.Is(err, harness.ErrBusy):
		return "busy"
	case errors.Is(err, harness.ErrResourcesWaiting):
		return "resources_waiting"
	case errors.Is(err, harness.ErrResourcesIncompatible):
		return "resources_incompatible"
	default:
		return "failed"
	}
}

// Record accepts host-defined phase names only. Raw errors may contain model
// responses or credentials, so only their known category is recorded.
func (d *Daemon) Record(phase string, err error) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.file == nil {
		return
	}
	level := "info"
	if err != nil {
		level = "error"
	}
	body, _ := json.Marshal(struct {
		Time  time.Time `json:"time"`
		PID   int       `json:"pid"`
		Level string    `json:"level"`
		Phase string    `json:"phase"`
		Code  string    `json:"code"`
	}{time.Now().UTC(), os.Getpid(), level, phase, errorCode(err)})
	body = append(body, '\n')
	if d.size+int64(len(body)) > d.limit {
		if e := d.rotate(); e != nil {
			d.failed = true
			return
		}
	}
	n, e := d.file.Write(body)
	d.size += int64(n)
	if e != nil {
		d.failed = true
	}
}

func (d *Daemon) rotate() error {
	if err := d.file.Close(); err != nil {
		d.file = nil
		return err
	}
	d.file = nil
	if err := os.Remove(d.path + ".2"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(d.path+".1", d.path+".2"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(d.path, d.path+".1"); err != nil {
		return err
	}
	file, err := os.OpenFile(d.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	d.file, d.size = file, 0
	return nil
}

func (d *Daemon) Status() map[string]any {
	if d == nil {
		return map[string]any{"enabled": false}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return map[string]any{"enabled": d.file != nil && !d.failed, "path": d.path, "format": "jsonl", "max_file_bytes": d.limit, "backup_count": 2}
}

func (d *Daemon) Close() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.file != nil {
		_ = d.file.Close()
		d.file = nil
	}
}
