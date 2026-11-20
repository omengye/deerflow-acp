package websearch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestMigratedSeedSurvivesStaleVerificationAndRetry(t *testing.T) {
	isolatedEnvironment(t)
	const key = "fixture-original-service-key"
	const ref = "${secret:BRAVE_TOKEN}"
	p := filepath.Join(t.TempDir(), "search-providers.json")
	m := mustManager(t, p, harness.SearchConfig{Providers: map[string]harness.SearchProviderConfig{"brave": {APIKey: key}}})
	old, err := m.Snapshot(nil)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("search:\n  providers:\n    brave:\n      api_key: '" + ref + "'\n")
	ctx := context.Background()
	publish := func(f func() error) error {
		_, err := m.PublishMigration(ctx, nil, raw, map[string]string{key: "BRAVE_TOKEN"}, f)
		return err
	}
	if err = publish(func() error { return errors.New("fixture interruption") }); err == nil {
		t.Fatal("publication failure lost")
	}
	old.verified(ctx, "brave")
	if err = publish(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	old.verified(ctx, "brave")
	current, err := m.Snapshot(nil)
	if err != nil || current.providers["brave"].APIKey != ref || m.seed.Providers["brave"].APIKey != ref {
		t.Fatal("seed not updated", err)
	}
	body, err := os.ReadFile(p)
	if err != nil || strings.Contains(string(body), key) || !strings.Contains(string(body), ref) {
		t.Fatal("stale verification persisted plaintext", err)
	}
}
