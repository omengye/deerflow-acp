package secrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func openTestVault(t *testing.T) (*Vault, string, *sqlite.Store) {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	v, err := Open(context.Background(), store.DB(), filepath.Join(dir, "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { v.Close() })
	return v, dir, store
}

func TestVaultRoundTripAndCiphertextAtRest(t *testing.T) {
	ctx := context.Background()
	v, dir, store := openTestVault(t)
	description := "GitHub token"
	meta, err := v.Set(ctx, SetRequest{Name: "GITHUB_TOKEN", Kind: KindString, Value: []byte("ghp_supersecretvalue123"), Description: &description, Actor: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if meta.Version != 1 || meta.Kind != KindString || meta.Size != 23 || !strings.HasPrefix(meta.Fingerprint, "hmac-sha256:") || meta.Marker != "[secret:GITHUB_TOKEN]" {
		t.Fatalf("metadata = %+v", meta)
	}
	resolved, err := v.Resolve(ctx, []string{"GITHUB_TOKEN"})
	if err != nil || string(resolved["GITHUB_TOKEN"].Value) != "ghp_supersecretvalue123" {
		t.Fatalf("Resolve = %v, %v", resolved, err)
	}
	if got := Mask("token=ghp_supersecretvalue123"); got != "token=[secret:GITHUB_TOKEN]" {
		t.Fatalf("registry Mask = %q", got)
	}
	if !ContainsKnown("x ghp_supersecretvalue123") {
		t.Fatal("ContainsKnown = false")
	}
	if _, err := store.DB().ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"harness.db", "harness.db-wal"} {
		data, _ := os.ReadFile(filepath.Join(dir, name))
		if strings.Contains(string(data), "supersecretvalue") {
			t.Fatalf("plaintext found in %s", name)
		}
	}
	list, err := v.List(ctx, false)
	if err != nil || len(list) != 1 || list[0].Description != description || list[0].LastUsedAt == nil {
		t.Fatalf("List = %+v, %v", list, err)
	}
}

func TestVaultVersionsDeleteRestore(t *testing.T) {
	ctx := context.Background()
	v, _, _ := openTestVault(t)
	for i := 1; i <= RetainedVersions+2; i++ {
		if _, err := v.Set(ctx, SetRequest{Name: "API_KEY", Kind: KindString, Value: []byte("value-number-" + string(rune('a'+i)))}); err != nil {
			t.Fatal(err)
		}
	}
	meta, err := v.Get(ctx, "API_KEY", true)
	if err != nil || meta.Version != RetainedVersions+2 || len(meta.Versions) != RetainedVersions {
		t.Fatalf("history = %+v, %v", meta, err)
	}
	if got := v.Masker().Mask("old value-number-f new value-number-h"); got != "old [secret:API_KEY@v5] new [secret:API_KEY]" {
		t.Fatalf("version markers = %q", got)
	}
	if _, err = v.Delete(ctx, "API_KEY", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err = v.Resolve(ctx, []string{"API_KEY"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve deleted = %v", err)
	}
	if got := v.Masker().Mask("value-number-h"); got != "[secret:API_KEY@v7]" {
		t.Fatalf("deleted value marker = %q", got)
	}
	if list, _ := v.List(ctx, false); len(list) != 0 {
		t.Fatalf("deleted secret listed: %+v", list)
	}
	if list, _ := v.List(ctx, true); len(list) != 1 || list[0].PurgeAfter == nil {
		t.Fatalf("include_deleted list = %+v", list)
	}
	if _, err = v.Restore(ctx, "API_KEY", 0, "test"); err != nil {
		t.Fatal(err)
	}
	restored, err := v.Restore(ctx, "API_KEY", 5, "test")
	if err != nil || restored.Version != 8 || restored.Source != "restore:v5" {
		t.Fatalf("rollback = %+v, %v", restored, err)
	}
	resolved, err := v.Resolve(ctx, []string{"API_KEY"})
	if err != nil || string(resolved["API_KEY"].Value) != "value-number-f" {
		t.Fatalf("rolled back value = %v, %v", resolved, err)
	}
}

func TestVaultPurgesExpiredDeletes(t *testing.T) {
	ctx := context.Background()
	v, dir, store := openTestVault(t)
	if _, err := v.Set(ctx, SetRequest{Name: "OLD", Kind: KindString, Value: []byte("old-secret-value")}); err != nil {
		t.Fatal(err)
	}
	v.now = func() time.Time { return time.Now().Add(-(DeletedRetentionDays + 1) * 24 * time.Hour) }
	if _, err := v.Delete(ctx, "OLD", ""); err != nil {
		t.Fatal(err)
	}
	v.Close()
	reopened, err := Open(ctx, store.DB(), filepath.Join(dir, "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err = reopened.Get(ctx, "OLD", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired delete survived: %v", err)
	}
	if reopened.Masker().Contains("old-secret-value") {
		t.Fatal("purged value still masked")
	}
}

func TestVaultKeyLossMarksValuesUnavailable(t *testing.T) {
	ctx := context.Background()
	v, dir, store := openTestVault(t)
	if _, err := v.Set(ctx, SetRequest{Name: "TOKEN", Kind: KindString, Value: []byte("token-value-123")}); err != nil {
		t.Fatal(err)
	}
	v.Close()
	keyPath := filepath.Join(dir, "secrets", "master.key")
	if err := os.WriteFile(keyPath, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	// An unreadable/corrupted master.key must fail closed and stay untouched
	// instead of being silently moved aside on a transient DPAPI failure.
	if _, err := Open(ctx, store.DB(), filepath.Join(dir, "secrets")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Open with unreadable key = %v; want ErrUnavailable", err)
	}
	if raw, err := os.ReadFile(keyPath); err != nil || string(raw) != "garbage" {
		t.Fatalf("master.key was modified or moved: %q, %v", raw, err)
	}
	// After an explicit manual key reset (removing master.key), Open creates a
	// fresh key and old rows report Unavailable until overwritten.
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, store.DB(), filepath.Join(dir, "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	meta, err := reopened.Get(ctx, "TOKEN", false)
	if err != nil || !meta.Unavailable {
		t.Fatalf("metadata after key loss = %+v, %v", meta, err)
	}
	if _, err = reopened.Resolve(ctx, []string{"TOKEN"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Resolve after key loss = %v", err)
	}
	if _, err = reopened.Set(ctx, SetRequest{Name: "TOKEN", Kind: KindString, Value: []byte("token-value-456")}); err != nil {
		t.Fatal(err)
	}
	if resolved, err := reopened.Resolve(ctx, []string{"TOKEN"}); err != nil || string(resolved["TOKEN"].Value) != "token-value-456" {
		t.Fatalf("Resolve after reset = %v, %v", resolved, err)
	}
}

func TestVaultKeyFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("DPAPI protects the key on Windows")
	}
	_, dir, _ := openTestVault(t)
	for path, want := range map[string]os.FileMode{filepath.Join(dir, "secrets"): 0o700, filepath.Join(dir, "secrets", "master.key"): 0o600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("%s mode = %v, %v; want %v", path, info.Mode().Perm(), err, want)
		}
	}
}

func TestSealBindsToCall(t *testing.T) {
	v, _, _ := openTestVault(t)
	sealed, err := v.Seal("call-1\x00NAME", []byte("sealed-value"))
	if err != nil {
		t.Fatal(err)
	}
	again, _ := v.Seal("call-1\x00NAME", []byte("sealed-value"))
	if sealed != again || strings.Contains(sealed, "sealed-value") {
		t.Fatalf("seal not deterministic or leaks: %q %q", sealed, again)
	}
	if value, err := v.Unseal("call-1\x00NAME", sealed); err != nil || string(value) != "sealed-value" {
		t.Fatalf("Unseal = %q, %v", value, err)
	}
	if _, err := v.Unseal("call-2\x00NAME", sealed); err == nil {
		t.Fatal("sealed value replayed under another call")
	}
}

func TestPrepareMaterializesAndCollects(t *testing.T) {
	ctx := context.Background()
	v, _, _ := openTestVault(t)
	if _, err := v.Set(ctx, SetRequest{Name: "TOKEN", Kind: KindString, Value: []byte("token-value-123")}); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Set(ctx, SetRequest{Name: "SSH_KEY", Kind: KindFile, Value: []byte("-----BEGIN KEY-----\nabc\n"), FileName: "id_ed25519"}); err != nil {
		t.Fatal(err)
	}
	in, err := v.Prepare(ctx, map[string]string{"API_TOKEN": "TOKEN", "KEY_FILE": "SSH_KEY"}, true)
	if err != nil {
		t.Fatal(err)
	}
	env := in.HostEnv()
	if env["API_TOKEN"] != "token-value-123" || filepath.Base(env["KEY_FILE"]) != "id_ed25519" || env[OutEnv] == "" {
		t.Fatalf("HostEnv = %v", env)
	}
	if data, err := os.ReadFile(env["KEY_FILE"]); err != nil || string(data) != "-----BEGIN KEY-----\nabc\n" {
		t.Fatalf("materialized file = %q, %v", data, err)
	}
	if err := os.WriteFile(filepath.Join(env[OutEnv], "TOKEN"), []byte("token-value-789\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env[OutEnv], "bad-name"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	updates, err := in.Collect(ctx, v, "command", nil)
	if err != nil || len(updates) != 2 || updates[0].Name != "TOKEN" || updates[0].Status != "updated" || updates[0].Version != 2 || updates[1].Status != "rejected" {
		t.Fatalf("Collect = %+v, %v", updates, err)
	}
	if resolved, _ := v.Resolve(ctx, []string{"TOKEN"}); string(resolved["TOKEN"].Value) != "token-value-789" {
		t.Fatalf("written back value = %q", resolved["TOKEN"].Value)
	}
	if err := in.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(in.FilesDir)); !os.IsNotExist(err) {
		t.Fatalf("material not removed: %v", err)
	}
	if _, err := v.Prepare(ctx, map[string]string{"X": "MISSING"}, false); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "MISSING") {
		t.Fatalf("missing secret error = %v", err)
	}
	if _, err := v.Prepare(ctx, map[string]string{"PATH": "TOKEN"}, false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("reserved env accepted: %v", err)
	}
}

func TestValidation(t *testing.T) {
	ctx := context.Background()
	v, _, _ := openTestVault(t)
	for _, req := range []SetRequest{
		{Name: "lower", Kind: KindString, Value: []byte("x")},
		{Name: "OK", Kind: KindString, Value: []byte("a\x00b")},
		{Name: "OK", Kind: KindString, Value: nil},
		{Name: "OK", Kind: KindFile, Value: []byte("x"), FileName: "../escape"},
		{Name: "OK", Kind: KindFile, Value: []byte("x"), FileName: "con.txt"},
		{Name: "OK", Kind: "other", Value: []byte("x")},
	} {
		if _, err := v.Set(ctx, req); !errors.Is(err, ErrInvalid) {
			t.Errorf("Set(%+v) = %v, want ErrInvalid", req, err)
		}
	}
}
