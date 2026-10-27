package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func TestPersonalMigrationAtomicRepeatablePreservesProvenance(t *testing.T) {
	ctx := context.Background()
	s, global := personalStore(t)
	workspace := t.TempDir()
	origin, err := NewScope(WorkspaceScope, workspace, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	create := func(id, content, category string) Fact {
		f, err := s.Create(ctx, origin, Candidate{Content: content, Category: category, Confidence: 1, Source: Source{ID: id, Kind: "operator", RunID: "original-run", InputID: "original-input"}})
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	a := create("city", "用户在北京。", "profile")
	b := create("style", "语音回复保持简短。", "preference")
	item := func(f Fact, dest, key string) MigrationItem {
		hash := sha256.Sum256([]byte(f.Content))
		return MigrationItem{FactID: f.ID, Revision: f.Revision, ContentSHA: hex.EncodeToString(hash[:]), Workspace: workspace, Destination: dest, Key: key}
	}
	plan := []MigrationItem{item(a, "personal", "city"), item(b, "folotoy", "channel.response_style")}
	bad := append([]MigrationItem(nil), plan...)
	bad[1].Revision++
	if _, err = s.MigrateSingleUser(ctx, "invalid", bad); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("invalid manifest: %v", err)
	}
	page, err := s.List(ctx, global, "", 100)
	if err != nil || len(page.Facts) != 0 {
		t.Fatalf("partial migration: %+v %v", page, err)
	}
	report, err := s.MigrateSingleUser(ctx, "test-v1", plan)
	if err != nil || report.Personal != 1 || report.Channel != 1 {
		t.Fatalf("migration: %+v %v", report, err)
	}
	repeated, err := s.MigrateSingleUser(ctx, "test-v1", plan)
	if err != nil || repeated != report {
		t.Fatalf("repeat: %+v %v", repeated, err)
	}
	page, err = s.List(ctx, global, "", 100)
	if err != nil || len(page.Facts) != 1 {
		t.Fatalf("personal: %+v %v", page, err)
	}
	f := page.Facts[0]
	if f.Source.RunID != a.Source.RunID || f.Source.InputID != a.Source.InputID || !f.UpdatedAt.Equal(a.UpdatedAt) {
		t.Fatalf("provenance changed: %+v", f)
	}
	if _, err = s.Get(ctx, origin, a.ID); !errors.Is(err, harness.ErrNotFound) {
		t.Fatalf("source not retired: %v", err)
	}
	channel, err := s.ChannelPreferences(ctx, workspace)
	if err != nil || channel == "" {
		t.Fatalf("channel missing: %v", err)
	}
	other, err := s.ChannelPreferences(ctx, t.TempDir())
	if err != nil || other != "" {
		t.Fatalf("channel leaked: %v", err)
	}
}
