package secrets

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func TestConcurrentCredentialFormsAcrossVaultConnections(t *testing.T) {
	first, dir, _ := openTestVault(t)
	store, err := sqlite.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := t.Context()
	second, err := Open(ctx, store.DB(), filepath.Join(dir, "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	scope, err := NewCredentialScope("tool:web_fetch", "https://fixture.test", "default")
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	done := make(chan error, 2)
	for _, vault := range []*Vault{first, second} {
		go func() {
			<-start
			done <- vault.saveCredential(ctx, scope, BothFields, CredentialPair{[]byte("fixture-user"), []byte("fixture-token")}, "test")
		}()
	}
	close(start)
	if a, b := <-done, <-done; (a == nil) == (b == nil) {
		t.Fatalf("expected one enrollment to commit, got %v and %v", a, b)
	}
	for _, name := range BothFields.Names(scope) {
		m, err := first.Get(ctx, name, true)
		if err != nil || m.Version != 1 || len(m.Versions) != 1 {
			t.Fatalf("credential changed during competing enrollment: version %d, %v", m.Version, err)
		}
	}
}

func TestRemoteCredentialFieldUpgradePreservesExistingKey(t *testing.T) {
	ctx := WithRemoteCredentialClient(WithCredentialGrant(context.Background()))
	v, _, _ := openTestVault(t)
	scope, err := NewCredentialScope("tool:web_fetch", "https://fixture.test", "default")
	if err != nil {
		t.Fatal(err)
	}
	if err = v.saveCredential(ctx, scope, KeyField, CredentialPair{Key: []byte("fixture-token")}, "test"); err != nil {
		t.Fatal(err)
	}
	if enrollment, err := v.NewEnrollment(ctx, scope, KeyField, 0); err != nil || enrollment != nil {
		t.Fatal("an already saved token required enrollment", err)
	}
	authorized, err := v.CredentialContext(ctx, scope, KeyField)
	if err != nil {
		t.Fatal(err)
	}
	username, key := scope.Names()
	if _, err := v.Resolve(authorized, []string{username}); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("token grant exposed username", err)
	}
	form, err := v.NewEnrollment(ctx, scope, BothFields, 2)
	if err != nil || form == nil || !slices.Equal(form.Form.RequiredFields, []string{"username"}) {
		t.Fatal("upgrade must request only the missing username", err)
	}
	if err := v.saveCredential(ctx, scope, UsernameField, CredentialPair{Username: []byte("fixture-user")}, "test"); err != nil {
		t.Fatal(err)
	}
	if err := v.saveCredential(ctx, scope, UsernameField, CredentialPair{Username: []byte("competing-user")}, "test"); !errors.Is(err, errCredentialExists) {
		t.Fatal("stale enrollment was not rejected", err)
	}
	authorized, err = v.CredentialContext(ctx, scope, BothFields)
	if err != nil {
		t.Fatal(err)
	}
	values, err := v.Resolve(authorized, []string{username, key})
	if err != nil || string(values[key].Value) != "fixture-token" || values[key].Version != 1 || string(values[username].Value) != "fixture-user" || values[username].Version != 1 {
		t.Fatal("credential field changed during upgrade", err)
	}
}

func TestCredentialScopeAndEnrollmentAtomicity(t *testing.T) {
	v, _, _ := openTestVault(t)
	s, _ := NewCredentialScope("skill:sample", "https://EXAMPLE.test:443/", "")
	same, _ := NewCredentialScope("skill:sample", "https://example.test", "default")
	if s != same {
		t.Fatal("origin normalization")
	}
	for _, params := range [][3]string{{"tool:sample", s.Service, ""}, {"skill:sample", "https://else.test", ""}, {"skill:sample", s.Service, "admin"}, {"skill:sample", "http://example.test", ""}} {
		other, _ := NewCredentialScope(params[0], params[1], params[2])
		a, _ := s.Names()
		b, _ := other.Names()
		if a == b {
			t.Fatal("scope collision")
		}
	}
	calls := 0
	ctx := WithCredentialPrompt(context.Background(), func(context.Context, CredentialScope, CredentialFields) (CredentialPair, error) {
		calls++
		return CredentialPair{[]byte("sample-user"), nil}, nil
	})
	if _, err := v.CredentialContext(ctx, s, BothFields); !errors.Is(err, harness.ErrPermissionDenied) || calls != 0 {
		t.Fatal("unconfirmed prompt")
	}
	if _, err := v.CredentialContext(WithCredentialGrant(ctx), s, BothFields); err == nil {
		t.Fatal("empty key accepted")
	}
	u, k := s.Names()
	missing, err := v.Missing(ctx, []string{u, k})
	if err != nil || len(missing) != 2 {
		t.Fatal("partial enrollment")
	}
	ctx = WithCredentialPrompt(WithCredentialGrant(ctx), func(context.Context, CredentialScope, CredentialFields) (CredentialPair, error) {
		return CredentialPair{[]byte("sample-user"), []byte("sample-key")}, nil
	})
	allowed, err := v.CredentialContext(ctx, s, BothFields)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = v.Resolve(ctx, []string{k}); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("generic access outside scope")
	}
	if _, err = v.Resolve(allowed, []string{u, k}); err != nil {
		t.Fatal(err)
	}
	if _, err = v.Set(ctx, SetRequest{Name: k, Kind: KindString, Value: []byte("another-key")}); err == nil {
		t.Fatal("generic overwrite accepted")
	}
	if _, err = v.CredentialContext(Disallow(ctx), s, BothFields); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatal("delegated access")
	}
}

func TestImportChecksCreateAndReuseInOneTransaction(t *testing.T) {
	v, _, _ := openTestVault(t)
	ctx := context.Background()
	set := func(n, value string) SetRequest { return SetRequest{Name: n, Kind: KindString, Value: []byte(value)} }
	old, err := v.Set(ctx, set("EXISTING", "old-service-key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = v.Set(ctx, set("EXISTING", "rotated-service-key")); err != nil {
		t.Fatal(err)
	}
	w, end := v.BeginImport()
	if err = w.Store(ctx, []SetRequest{set("UNPUBLISHED", "first-key")}, map[string]Metadata{"EXISTING": old}); !errors.Is(err, ErrImportConflict) {
		end()
		t.Fatal("stale reference accepted")
	}
	if err = w.Store(ctx, []SetRequest{set("UNPUBLISHED", "first-key"), set("EXISTING", "clashing-key")}, nil); !errors.Is(err, ErrImportConflict) {
		end()
		t.Fatal("name conflict accepted")
	}
	done := make(chan error, 1)
	go func() { _, e := v.Set(ctx, set("CONCURRENT", "another-key")); done <- e }()
	select {
	case <-done:
		end()
		t.Fatal("writer passed publication fence")
	case <-time.After(20 * time.Millisecond):
	}
	end()
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if _, err = v.Get(ctx, "UNPUBLISHED", false); !errors.Is(err, ErrNotFound) {
		t.Fatal("partial transaction")
	}
	meta, _ := v.Get(ctx, "EXISTING", false)
	if meta.Version != 2 {
		t.Fatal("changed conflicting secret")
	}
}

func TestLargeFileMaskRangeTruncationStreamAndRestore(t *testing.T) {
	for _, size := range []int{65535, 65536, 65537, MaxFileBytes} {
		buf := make([]byte, size)
		if _, err := rand.Read(buf); err != nil {
			t.Fatal(err)
		}
		v, _, _ := openTestVault(t)
		if _, err := v.Set(context.Background(), SetRequest{Name: "FILE", Kind: KindFile, Value: buf}); err != nil {
			t.Fatal(err)
		}
		for _, value := range []string{string(buf), base64.StdEncoding.EncodeToString(buf), base64.RawURLEncoding.EncodeToString(buf)} {
			m := v.Masker()
			marker := m.Mask(value)
			if !strings.HasPrefix(marker, "[secret:FILE") || len(marker) > 40 {
				t.Fatal("missing complete redaction", size)
			}
			if got := MaskRange("pre"+value+"post", size/2, size/2+3); got != "[secret:FILE:partial]" {
				t.Fatal("range inside credential", size)
			}
			if got := m.MaskTruncated("pre" + value[:len(value)/2]); got != "pre[secret:FILE:partial]" {
				t.Fatal("unsafe truncated result", size)
			}
			if got, e := m.RestoreInFile(marker, value); e != nil || got != value {
				t.Fatal("file restoration", size)
			}
			s := m.Stream()
			var out strings.Builder
			for i := 0; i < len(value); i += 2021 {
				out.WriteString(s.Push(value[i:min(i+2021, len(value))]))
			}
			out.WriteString(s.Flush())
			if out.String() != marker {
				t.Fatal("streamed credential", size)
			}
		}
		v.Close()
	}
	repeated := strings.Repeat("a", 65537)
	m := testMasker(repeated)
	if g := m.MaskTruncated(repeated); g != "[secret:SA]" {
		t.Fatal("partial overlap split a complete credential")
	}
	if g := m.MaskTruncated("cut " + repeated[:3]); g != "cut [secret:SA:partial]" {
		t.Fatal("short prefix of large credential")
	}
}

func TestLargeTextCredentialAllEncodingsAndPartialStream(t *testing.T) {
	value := []byte("  " + strings.Repeat("aB<&/中\n", 8200) + "\n")
	m := testMasker(string(value))
	for _, v := range variants(value) {
		marker := m.Mask(v.text)
		if !strings.HasPrefix(marker, "[secret:SA") || len(marker) > 40 {
			t.Fatal("missing redaction for", v.suffix)
		}
		if restored, e := m.RestoreInFile(marker, "start"+v.text+"end"); e != nil || restored != v.text {
			t.Fatal("could not preserve the original encoding", v.suffix, e)
		}
	}
	s := m.Stream()
	if got := s.Push(string(value[:12000])) + s.Flush(); got != "[secret:SA:partial]" {
		t.Fatal("partial stream not redacted")
	}
}
