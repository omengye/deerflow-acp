package assets_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func commitReadArtifact(t *testing.T, f *assetFixture, data []byte) harness.AssetRef {
	t.Helper()
	dir := filepath.Join(f.session.CWD, ".deerflow", "outputs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "binary.dat"), data, 0600); err != nil {
		t.Fatal(err)
	}
	return publishReadArtifact(t, f, "binary.dat")
}

func publishReadArtifact(t *testing.T, f *assetFixture, name string) harness.AssetRef {
	t.Helper()
	ctx := context.Background()
	run, call := f.startTool(t, "present_files")
	staged, err := f.assets.StageArtifacts(ctx, f.session, run, call, []string{name})
	if err != nil {
		t.Fatal(err)
	}
	ref := *staged[0].Asset
	if data, err := f.assets.ReadArtifactRange(ctx, f.session.ID, ref, 0, 16); !errors.Is(err, harness.ErrNotFound) || len(data) != 0 {
		t.Fatalf("uncommitted read %x: %v", data, err)
	}
	if _, err = f.runtime.Append(ctx, harness.RunEvent{SessionID: f.session.ID, RunID: run, ToolCallID: call, ToolName: "present_files", Kind: "tool_end", Status: "completed"}, f.assets); err != nil {
		t.Fatal(err)
	}
	return ref
}

func TestArtifactReadRangesAndImmutableSnapshot(t *testing.T) {
	f := newAssetFixture(t)
	const block = 1024 * 1024
	data := make([]byte, 2*block+31)
	for i := range data {
		data[i] = byte(i * 7)
	}
	ref := commitReadArtifact(t, f, data)
	if err := os.WriteFile(filepath.Join(f.session.CWD, ".deerflow", "outputs", "binary.dat"), []byte("changed workspace output"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct {
		offset int64
		limit  int
	}{{0, 19}, {16, 127}, {block - 13, 31}, {block, block}, {int64(len(data) - 12), 100}, {int64(len(data)), 32}} {
		got, err := f.assets.ReadArtifactRange(context.Background(), f.session.ID, ref, r.offset, r.limit)
		want := data[r.offset:min(r.offset+int64(r.limit), int64(len(data)))]
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("range %+v size=%d want=%d: %v", r, len(got), len(want), err)
		}
	}
	for _, r := range []struct {
		offset int64
		limit  int
	}{{-1, 16}, {int64(len(data) + 1), 16}, {0, 0}, {0, -1}, {0, block + 1}} {
		if got, err := f.assets.ReadArtifactRange(context.Background(), f.session.ID, ref, r.offset, r.limit); !errors.Is(err, harness.ErrInvalidInput) || len(got) != 0 {
			t.Fatalf("invalid %+v: %v", r, err)
		}
	}
	if _, err := f.assets.ReadArtifactRange(context.Background(), "other-session", ref, 0, 16); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatalf("ownership: %v", err)
	}
	for _, kind := range []string{harness.AssetFile, harness.AssetImage} {
		changed := ref
		changed.Kind = kind
		if _, err := f.assets.ReadArtifactRange(context.Background(), f.session.ID, changed, 0, 16); !errors.Is(err, harness.ErrPermissionDenied) {
			t.Fatalf("kind=%s: %v", kind, err)
		}
	}
	changed := ref
	changed.Name = "forged.dat"
	if _, err := f.assets.ReadArtifactRange(context.Background(), f.session.ID, changed, 0, 16); !errors.Is(err, harness.ErrPermissionDenied) {
		t.Fatalf("ref metadata: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := f.assets.ReadArtifactRange(ctx, f.session.ID, ref, 0, 16); !errors.Is(err, context.Canceled) || len(got) != 0 {
		t.Fatalf("cancelled: %v", err)
	}
}

func TestArtifactReadRejectsSnapshotTampering(t *testing.T) {
	const block = 1024 * 1024
	for _, tc := range []struct {
		name           string
		warm           bool
		mutate, offset int64
		resize         bool
	}{
		{"cold-outside-range", false, block + 15, 0, false},
		{"cached-block-outside-range", true, block - 1, 0, false},
		{"cached-other-block", true, block + 15, block, false},
		{"cached-cross-boundary", true, block + 20, block - 4, false},
		{"cached-size", true, 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAssetFixture(t)
			ref := commitReadArtifact(t, f, bytes.Repeat([]byte{7}, 2*block+7))
			if tc.warm {
				if _, err := f.assets.ReadArtifactRange(context.Background(), f.session.ID, ref, 0, 16); err != nil {
					t.Fatal(err)
				}
			}
			file, err := os.OpenFile(filepath.Join(f.dir, ref.ID+".blob"), os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			if tc.resize {
				err = file.Truncate(ref.Size + 1)
			} else {
				_, err = file.WriteAt([]byte{8}, tc.mutate)
			}
			if closeErr := file.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := f.assets.ReadArtifactRange(context.Background(), f.session.ID, ref, tc.offset, 16)
			if err == nil || len(got) != 0 {
				t.Fatalf("tampered snapshot returned %x: %v", got, err)
			}
		})
	}
}

func TestArtifactReadEmpty(t *testing.T) {
	f := newAssetFixture(t)
	ref := commitReadArtifact(t, f, nil)
	for range 2 {
		data, err := f.assets.ReadArtifactRange(context.Background(), f.session.ID, ref, 0, 16)
		if err != nil || len(data) != 0 || data == nil {
			t.Fatalf("empty read=%v %v", data, err)
		}
	}
}

func TestArtifactReadLargeSequential(t *testing.T) {
	if testing.Short() {
		t.Skip("100 MiB sequential artifact measurement")
	}
	f := newAssetFixture(t)
	dir := filepath.Join(f.session.CWD, ".deerflow", "outputs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	file, err := os.Create(filepath.Join(dir, "large.dat"))
	if err != nil {
		t.Fatal(err)
	}
	block := bytes.Repeat([]byte{71}, 1024*1024)
	for range 100 {
		if _, err = file.Write(block); err != nil {
			t.Fatal(err)
		}
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	ref := publishReadArtifact(t, f, "large.dat")
	start := time.Now()
	if _, err = f.assets.ReadArtifactRange(context.Background(), f.session.ID, ref, 0, len(block)); err != nil {
		t.Fatal(err)
	}
	cold := time.Since(start)
	start = time.Now()
	for offset := int64(0); offset < ref.Size; offset += int64(len(block)) {
		got, err := f.assets.ReadArtifactRange(context.Background(), f.session.ID, ref, offset, len(block))
		if err != nil || !bytes.Equal(got, block) {
			t.Fatalf("offset=%d: %v", offset, err)
		}
	}
	t.Logf("100 MiB artifact: initial full verification plus 1 MiB read=%s; 100 cached 1 MiB chunks=%s; disk reads for cold full download approximately 200 MiB (previous full-scan approach 10000 MiB)", cold, time.Since(start))
}
