package secrets

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// Use records which version an injection exposed to a command.
type Use struct {
	Env     string `json:"env"`
	Name    string `json:"name"`
	Version int64  `json:"version"`
	Kind    Kind   `json:"kind"`
}

// Injection is host-side secret material for one command. String values live
// in Env; file values are private files under FilesDir, outside the workspace.
// The owner must call Cleanup after the command has terminated.
type Injection struct {
	Env      map[string]string // variable -> plaintext of a string secret
	Files    map[string]string // variable -> slash path relative to FilesDir
	FilesDir string
	OutDir   string // write-back directory; empty when disabled
	Used     []Use
	root     string
}

// Prepare resolves bindings (variable -> secret name) and materializes file
// secrets. writeBack additionally creates the DEERFLOW_SECRET_OUT directory.
// Files are private to the daemon user; container commands therefore must run
// as that user, which is the sandbox default.
func (v *Vault) Prepare(ctx context.Context, bindings map[string]string, writeBack bool) (*Injection, error) {
	if err := v.usable(); err != nil {
		return nil, err
	}
	if err := ValidateBindings(bindings); err != nil {
		return nil, err
	}
	if len(bindings) == 0 && !writeBack {
		return nil, nil
	}
	names := make([]string, 0, len(bindings))
	for _, name := range bindings {
		names = append(names, name)
	}
	resolved, err := v.Resolve(ctx, names)
	if err != nil {
		return nil, err
	}
	defer func() {
		for _, secret := range resolved {
			clear(secret.Value)
		}
	}()
	root := filepath.Join(v.dir, "run", rand.Text())
	if err = os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	in := &Injection{Env: map[string]string{}, Files: map[string]string{}, FilesDir: filepath.Join(root, "files"), root: root}
	fail := func(err error) (*Injection, error) {
		_ = in.Cleanup()
		return nil, err
	}
	envs := make([]string, 0, len(bindings))
	for env := range bindings {
		envs = append(envs, env)
	}
	sort.Strings(envs)
	for _, env := range envs {
		secret := resolved[bindings[env]]
		in.Used = append(in.Used, Use{Env: env, Name: secret.Name, Version: secret.Version, Kind: secret.Kind})
		if secret.Kind == KindString {
			in.Env[env] = string(secret.Value)
			continue
		}
		name := secret.FileName
		if name == "" {
			name = secret.Name
		}
		dir := filepath.Join(in.FilesDir, env)
		if err = os.MkdirAll(dir, 0o700); err != nil {
			return fail(err)
		}
		if err = writePrivate(filepath.Join(dir, name), secret.Value); err != nil {
			return fail(err)
		}
		in.Files[env] = env + "/" + name
	}
	if writeBack {
		in.OutDir = filepath.Join(root, "out")
		if err = os.MkdirAll(in.OutDir, 0o700); err != nil {
			return fail(err)
		}
	}
	return in, nil
}

func writePrivate(name string, data []byte) error {
	file, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = file.Write(data)
	return errors.Join(err, file.Close())
}

// Command returns the material in the form command backends inject.
func (in *Injection) Command() *harness.CommandSecrets {
	if in == nil {
		return nil
	}
	return &harness.CommandSecrets{Env: in.Env, Files: in.Files, FilesDir: in.FilesDir, OutDir: in.OutDir}
}

// HostEnv returns variables for a process that sees host paths directly.
func (in *Injection) HostEnv() map[string]string {
	return in.Command().Environment(in.FilesDir, in.OutDir, filepath.Join)
}

// Cleanup removes all material for this command. A failure leaves files for
// the next vault Open sweep.
func (in *Injection) Cleanup() error {
	if in == nil || in.root == "" {
		return nil
	}
	err := os.RemoveAll(in.root)
	if err == nil {
		in.root = ""
	}
	return err
}

type Update struct {
	Name    string `json:"name"`
	Version int64  `json:"version,omitempty"`
	Kind    Kind   `json:"kind,omitempty"`
	Status  string `json:"status"` // updated, created, unchanged, rejected
	Reason  string `json:"reason,omitempty"`
}

// Collect ingests files a command wrote into its write-back directory.
// Callers invoke it only after the process termination is confirmed, whatever
// its exit code, so a refreshed credential is not lost when a later step fails.
// A non-nil allow limits which secret names may be written.
func (in *Injection) Collect(ctx context.Context, v *Vault, actor string, allow func(name string) bool) ([]Update, error) {
	if in == nil || in.OutDir == "" {
		return nil, nil
	}
	root, err := os.OpenRoot(in.OutDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	entries, err := dir.ReadDir(65)
	dir.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var updates []Update
	for i, entry := range entries {
		name := entry.Name()
		if i >= 32 {
			updates = append(updates, Update{Name: name, Status: "rejected", Reason: "at most 32 secrets may be written back per command"})
			continue
		}
		if !ValidName(name) {
			updates = append(updates, Update{Name: name, Status: "rejected", Reason: "file name must be an uppercase secret name [A-Z_][A-Z0-9_]*"})
			continue
		}
		if allow != nil && !allow(name) {
			updates = append(updates, Update{Name: name, Status: "rejected", Reason: "this command may not write that secret"})
			continue
		}
		info, err := root.Lstat(name)
		if err != nil || !info.Mode().IsRegular() {
			updates = append(updates, Update{Name: name, Status: "rejected", Reason: "not a regular file"})
			continue
		}
		if info.Size() == 0 || info.Size() > MaxFileBytes {
			updates = append(updates, Update{Name: name, Status: "rejected", Reason: fmt.Sprintf("size must be 1..%d bytes", MaxFileBytes)})
			continue
		}
		file, err := root.Open(name)
		if err != nil {
			updates = append(updates, Update{Name: name, Status: "rejected", Reason: "cannot open file"})
			continue
		}
		value, err := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
		file.Close()
		if err != nil || len(value) > MaxFileBytes {
			clear(value)
			updates = append(updates, Update{Name: name, Status: "rejected", Reason: "cannot read file"})
			continue
		}
		update, err := ingest(ctx, v, name, value, actor)
		clear(value)
		if err != nil {
			update = Update{Name: name, Status: "rejected", Reason: err.Error()}
		}
		updates = append(updates, update)
	}
	return updates, nil
}

func ingest(ctx context.Context, v *Vault, name string, value []byte, actor string) (Update, error) {
	kind := KindString
	existing, err := v.Get(ctx, name, false)
	exists := err == nil
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Update{}, err
	}
	if exists {
		kind = existing.Kind
	} else if !utf8.Valid(value) || bytes.IndexByte(value, 0) >= 0 || len(value) > MaxStringBytes {
		kind = KindFile
	}
	if kind == KindString {
		value = trimNewline(value)
	}
	if exists && existing.DeletedAt == nil && !existing.Unavailable && existing.Fingerprint == v.Fingerprint(value) {
		return Update{Name: name, Version: existing.Version, Kind: kind, Status: "unchanged"}, nil
	}
	meta, err := v.Set(ctx, SetRequest{Name: name, Kind: kind, Value: value, FileName: existing.FileName, Source: "command", Actor: actor})
	if err != nil {
		return Update{}, err
	}
	status := "created"
	if exists {
		status = "updated"
	}
	return Update{Name: name, Version: meta.Version, Kind: kind, Status: status}, nil
}

// trimNewline drops one trailing line ending, as written by echo or editors.
func trimNewline(value []byte) []byte {
	if bytes.HasSuffix(value, []byte("\r\n")) {
		return value[:len(value)-2]
	}
	return bytes.TrimSuffix(value, []byte("\n"))
}

// TrimNewline is the import rule for string secrets read from files.
func TrimNewline(value []byte) []byte { return trimNewline(value) }
