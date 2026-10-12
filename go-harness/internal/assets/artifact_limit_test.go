package assets_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/assets"
)

func setArtifactLimit(t *testing.T, f *assetFixture, limit int64) {
	t.Helper()
	if err := f.assets.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.assets, err = assets.NewStoreWithArtifactLimit(context.Background(), f.dir, f.db.DB(), limit)
	if err != nil {
		t.Fatal(err)
	}
	store := f.assets
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
}

func TestArtifactConfiguredLimit(t *testing.T) {
	f := newAssetFixture(t)
	ctx := context.Background()
	if f.assets.MaxArtifactBytes() != 500*1024*1024 {
		t.Fatal("wrong default")
	}
	setArtifactLimit(t, f, 1024*1024)
	// The exact boundary is accepted and is readable through both APIs.
	ref := commitReadArtifact(t, f, make([]byte, 1024*1024))
	if _, err := f.assets.Resolve(ctx, f.session.ID, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := f.assets.ReadArtifactRange(ctx, f.session.ID, ref, ref.Size-1, 1); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.session.CWD, ".deerflow", "outputs", "too-big.bin")
	if err := os.WriteFile(path, make([]byte, 1024*1024+1), 0600); err != nil {
		t.Fatal(err)
	}
	run, call := f.startTool(t, "present_files")
	_, err := f.assets.StageArtifacts(ctx, f.session, run, call, []string{path})
	if !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatalf("err=%v", err)
	}
	for _, text := range []string{"too-big.bin", "1048577", "1048576", "max_artifact_size_mib"} {
		if !strings.Contains(err.Error(), text) {
			t.Fatalf("missing %q: %v", text, err)
		}
	}
	if f.assets.TakeArtifacts(f.session.ID, run, call) != nil {
		t.Fatal("failed artifact retained")
	}
	setArtifactLimit(t, f, 512*1024)
	if _, err := f.assets.Resolve(ctx, f.session.ID, ref); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := f.assets.ReadArtifactRange(ctx, f.session.ID, ref, 0, 1); !errors.Is(err, harness.ErrInvalidInput) {
		t.Fatalf("range: %v", err)
	}
	// Output policy must not reduce the independent input-file allowance.
	in := f.accept(t, []harness.Content{{Type: "resource_link", URI: fileURI(path), Name: "input.bin"}})
	if _, err := f.assets.Resolve(ctx, f.session.ID, *in[0].Asset); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactAboveLegacyLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("large snapshot regression")
	}
	f := newAssetFixture(t)
	// A custom limit above the default must flow through staging and both reads.
	setArtifactLimit(t, f, 501*1024*1024)
	dir := filepath.Join(f.session.CWD, ".deerflow", "outputs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "large.bin")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	err = file.Truncate(500*1024*1024 + 1)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		t.Fatal(errors.Join(err, closeErr))
	}
	run, call := f.startTool(t, "present_files")
	contents, err := f.assets.StageArtifacts(context.Background(), f.session, run, call, []string{path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.runtime.Append(context.Background(), harness.RunEvent{SessionID: f.session.ID, RunID: run, ToolCallID: call, ToolName: "present_files", Kind: "tool_end", Status: "completed"}, f.assets); err != nil {
		t.Fatal(err)
	}
	ref := *contents[0].Asset
	if b, err := f.assets.ReadArtifactRange(context.Background(), f.session.ID, ref, ref.Size-1, 1); err != nil || len(b) != 1 {
		t.Fatalf("range: %v", err)
	}
	if b, err := f.assets.Resolve(context.Background(), f.session.ID, ref); err != nil || int64(len(b)) != ref.Size {
		t.Fatalf("resolve: %v", err)
	}
}
