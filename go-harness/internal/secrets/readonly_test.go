package secrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestReference(t *testing.T) {
	for _, value := range []string{"", "plain", "$API_KEY", "${API_KEY}", "${API_KEY:-x}", "prefix ${secret:A}", "${secret:A} suffix", "{{secret:A}}", "$secret:A"} {
		if name, ok, err := Reference(value); ok || err != nil || name != "" {
			t.Errorf("Reference(%q) = %q, %v, %v; want no reference", value, name, ok, err)
		}
	}
	for _, value := range []string{"${secret:lower}", "${secret:}", "${secret:A:-fallback}", "${secret:A}}", "${secret:1A}", "${secret: A}"} {
		if _, ok, err := Reference(value); !ok || !errors.Is(err, ErrInvalid) {
			t.Errorf("Reference(%q) = %v, %v; want an invalid reference", value, ok, err)
		}
	}
	if name, ok, err := Reference(ReferenceTo("API_KEY_2")); name != "API_KEY_2" || !ok || err != nil {
		t.Fatalf("Reference(ReferenceTo) = %q, %v, %v", name, ok, err)
	}
}

func TestReadCurrentReadsWithoutChangingTheVault(t *testing.T) {
	ctx := context.Background()
	v, dir, _ := openTestVault(t)
	vaultDir := filepath.Join(dir, "secrets")
	for _, req := range []SetRequest{
		{Name: "API_KEY", Kind: KindString, Value: []byte("first-value-0123456789")},
		{Name: "API_KEY", Kind: KindString, Value: []byte("current-value-0123456789")},
		{Name: "CERT", Kind: KindFile, Value: []byte{0, 1, 2, 0xff}, FileName: "cert.der"},
		{Name: "GONE", Kind: KindString, Value: []byte("deleted-value-0123456789")},
	} {
		if _, err := v.Set(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := v.Delete(ctx, "GONE", "test"); err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.OpenReadOnly(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, err := ReadCurrent(ctx, db, vaultDir, []string{"API_KEY", "CERT", "GONE", "ABSENT", "API_KEY"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || string(got["API_KEY"].Value) != "current-value-0123456789" || got["API_KEY"].Version != 2 || got["CERT"].Kind != KindFile || string(got["CERT"].Value) != "\x00\x01\x02\xff" || got["CERT"].FileName != "cert.der" {
		t.Fatalf("ReadCurrent = %+v", got)
	}
	if meta, err := v.Get(ctx, "API_KEY", false); err != nil || meta.LastUsedAt != nil {
		t.Fatalf("reading recorded a use: %+v, %v", meta.LastUsedAt, err)
	}
	if _, err := ReadCurrent(ctx, db, vaultDir, []string{"bad name"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid name = %v", err)
	}
	if got, err := ReadCurrent(ctx, db, vaultDir, nil); err != nil || len(got) != 0 {
		t.Fatalf("no names = %v, %v", got, err)
	}

	// A data directory without a vault holds no values and is left alone.
	absent := filepath.Join(t.TempDir(), "secrets")
	if got, err := ReadCurrent(ctx, db, absent, []string{"API_KEY"}); err != nil || len(got) != 0 {
		t.Fatalf("missing vault = %v, %v", got, err)
	}
	if _, err := os.Stat(absent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reading created the vault directory: %v", err)
	}

	// Values encrypted under another vault key are unavailable, not errors.
	other, _, _ := openTestVault(t)
	if got, err := ReadCurrent(ctx, db, other.Dir(), []string{"API_KEY"}); err != nil || len(got) != 0 {
		t.Fatalf("foreign key = %v, %v", got, err)
	}

	// A key file that cannot be unlocked is an error.
	broken := t.TempDir()
	if err := os.WriteFile(filepath.Join(broken, "master.key"), []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCurrent(ctx, db, broken, []string{"API_KEY"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unusable key = %v", err)
	}

	// A database without vault tables holds no values.
	fresh, err := sqlite.Open(filepath.Join(t.TempDir(), "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if got, err := ReadCurrent(ctx, fresh.DB(), vaultDir, []string{"API_KEY"}); err != nil || len(got) != 0 {
		t.Fatalf("database without vault tables = %v, %v", got, err)
	}
}
