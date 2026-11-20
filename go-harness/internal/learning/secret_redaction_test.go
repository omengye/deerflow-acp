package learning

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
	"github.com/omengye/deerflow-acp/go-harness/internal/storage/sqlite"
)

func learningTestVault(t *testing.T, name, value string) {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	v, err := secrets.Open(context.Background(), store.DB(), filepath.Join(dir, "secrets"))
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		v.Close()
		store.Close()
	})
	if _, err := v.Set(context.Background(), secrets.SetRequest{Name: name, Kind: secrets.KindString, Value: []byte(value), Actor: "test"}); err != nil {
		t.Fatal(err)
	}
}

func TestLearningRedactsVaultValuesAndKeepsMarkers(t *testing.T) {
	const value = "plainvault7f3a9c"
	learningTestVault(t, "DEPLOY_KEY", value)
	for _, text := range []string{
		"deploy with " + value + " now",
		"deploy with plainvault\u200b7f3a9c now",
		"deploy with plainvault\uE0007f3a9c now",
		`{"note":"deploy with ` + value + ` now"}`,
		"deploy with [secret:DEPLOY_KEY] now",
	} {
		got := redact(text, 4096)
		if strings.Contains(got, "plainvault") || !strings.Contains(got, "deploy with [secret:DEPLOY_KEY] now") {
			t.Fatalf("redact(%q) = %q", text, got)
		}
	}
	// A marker that is a credential assignment's value goes with it.
	if got := redact("token: [secret:DEPLOY_KEY] and [secret:OTHER@v2:json]", 4096); got != "[redacted] and [secret:OTHER@v2:json]" {
		t.Fatalf("assignment = %q", got)
	}
	if got := redact("x \uE0000\uE001 y", 4096); got != "x 0 y" {
		t.Fatalf("sentinel characters in input = %q", got)
	}
}

func TestPromotionInstructionsAcceptMarkersAndRejectVaultValues(t *testing.T) {
	const value = "plainvault7f3a9c"
	learningTestVault(t, "DEPLOY_KEY", value)
	learningTestVault(t, "ODD_VALUE", "ab[secret:X]cd")
	for _, text := range []string{
		"Run the deploy with " + value + " as the key.",
		"Run the deploy with plainvault\u200b7f3a9c as the key.",
		`Example: {"note":"plainvault\u0037f3a9c"}`,
		// A value shaped like a marker is checked before markers become
		// placeholders.
		"Use ab[secret:\u200bX]cd here.",
	} {
		if err := ValidatePromotionInstructions(text); err == nil {
			t.Fatalf("accepted %q", text)
		}
	}
	for _, text := range []string{
		"Pass secrets={\"GH_TOKEN\":\"GITHUB_TOKEN\"} to execute; output shows [secret:GITHUB_TOKEN].",
		"Set GITHUB_TOKEN: [secret:GITHUB_TOKEN] before running.",
		"Send Authorization: Bearer [secret:API_TOKEN].",
		`password = "[secret:DB_PASSWORD]"`,
		"token=[secret:API_TOKEN@v2] or https://api.example.com/?token=[secret:API_TOKEN]",
	} {
		if err := ValidatePromotionInstructions(text); err != nil {
			t.Fatalf("rejected %q: %v", text, err)
		}
	}
	if err := ValidatePromotionInstructions("token: [secret:API_TOKEN]x"); err == nil {
		t.Fatal("a marker with trailing text was treated as a placeholder")
	}
}
