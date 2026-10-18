package skillauthor

import (
	"archive/zip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

func fixture(t *testing.T, body string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "sample-skill")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: sample-skill\ndescription: 用于核验报告。\n---\n"+body+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestInitializeRefusesOverwriteAndRequiresCompletedInstructions(t *testing.T) {
	ctx, parent := context.Background(), t.TempDir()
	dir, err := Initialize(ctx, parent, "sample-skill", "用于核验报告。")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Validate(ctx, dir); err == nil {
		t.Fatal("unfinished scaffold validated")
	}
	if _, err := Initialize(ctx, parent, "sample-skill", "replacement"); err == nil {
		t.Fatal("initializer overwrote existing directory")
	}
	for _, name := range []string{"../escape", "Bad", "a--b", strings.Repeat("a", 65)} {
		if _, err := Initialize(ctx, parent, name, "A description"); err == nil {
			t.Fatalf("invalid name %q accepted", name)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "SKILL.md" {
		t.Fatalf("unexpected scaffold resources: %v %v", entries, err)
	}
}

func TestInitializeRollsBackPartialWriteAndCancellation(t *testing.T) {
	for _, cancelAfterWrite := range []bool{false, true} {
		t.Run(fmtTestName(cancelAfterWrite), func(t *testing.T) {
			parent := t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failure := errors.New("injected partial write failure")
			_, err := initialize(ctx, parent, "sample-skill", "Read reports.", func(file *os.File, content []byte) error {
				if _, err := file.Write(content[:5]); err != nil {
					return err
				}
				if cancelAfterWrite {
					cancel()
					return nil
				}
				return failure
			})
			want := failure
			if cancelAfterWrite {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatalf("failure lost: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(parent, "sample-skill")); !os.IsNotExist(err) {
				t.Fatalf("failed initializer left partial directory: %v", err)
			}
		})
	}
}

func fmtTestName(canceled bool) string {
	if canceled {
		return "canceled_after_write"
	}
	return "partial_write"
}

func TestInitializeFailurePreservesAdditionalUserFiles(t *testing.T) {
	parent := t.TempDir()
	userFile := filepath.Join(parent, "sample-skill", "user-note.md")
	_, err := initialize(context.Background(), parent, "sample-skill", "Read reports.", func(file *os.File, _ []byte) error {
		if err := os.WriteFile(userFile, []byte("keep this"), 0600); err != nil {
			return err
		}
		return io.ErrShortWrite
	})
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("expected injected error: %v", err)
	}
	data, err := os.ReadFile(userFile)
	if err != nil || string(data) != "keep this" {
		t.Fatalf("user file changed: %q %v", data, err)
	}
	if _, err := os.Lstat(filepath.Join(parent, "sample-skill", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("partial scaffold remains: %v", err)
	}
}

func TestInitializeCleanupRefusesReplacedDirectoryOrLink(t *testing.T) {
	for _, useLink := range []bool{false, true} {
		name := "directory"
		if useLink {
			name = "link"
		}
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			original := filepath.Join(parent, "sample-skill")
			if err := os.Mkdir(original, 0700); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(parent)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			// Use the production identity capture. Path-based os.Lstat defers
			// file IDs on Windows until SameFile, after the path is replaced.
			expected, err := root.Lstat("sample-skill")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(original, filepath.Join(parent, "original-skill")); err != nil {
				t.Fatal(err)
			}
			if useLink {
				if err := os.Symlink(t.TempDir(), original); err != nil {
					t.Skip("symlink capability unavailable:", err)
				}
			} else if err := os.Mkdir(original, 0700); err != nil {
				t.Fatal(err)
			}
			if err := removeCreatedDirectory(root, "sample-skill", expected); err == nil {
				t.Fatal("cleanup removed a replacement")
			}
			if _, err := os.Lstat(original); err != nil {
				t.Fatalf("replacement was not retained: %v", err)
			}
		})
	}
}

func TestInitializeFailurePreservesReplacedDirectory(t *testing.T) {
	parent := t.TempDir()
	created := filepath.Join(parent, "sample-skill")
	moved := filepath.Join(parent, "moved-skill")
	replacementDenied := false
	_, err := initialize(context.Background(), parent, "sample-skill", "Read reports.", func(file *os.File, content []byte) error {
		if _, err := file.Write(content[:5]); err != nil {
			return err
		}
		if err := os.Rename(created, moved); err != nil {
			if os.IsPermission(err) {
				// Windows can prevent directory replacement while the pinned
				// root/file handles remain open. Exercise rollback in that case.
				replacementDenied = true
				return io.ErrShortWrite
			}
			return err
		}
		if err := os.Mkdir(created, 0700); err != nil {
			return err
		}
		return io.ErrShortWrite
	})
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("expected injected error: %v", err)
	}
	if replacementDenied {
		for _, dir := range []string{created, moved} {
			if _, err := os.Lstat(dir); !os.IsNotExist(err) {
				t.Fatalf("denied replacement left a partial directory: %s %v", dir, err)
			}
		}
		t.Log("operating system denied replacement while directory handles were open")
		return
	}
	for _, dir := range []string{created, moved} {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Fatalf("replacement or moved directory not retained empty: %s %v %v", dir, entries, err)
		}
	}
}

func TestValidateReusesRuntimeProfileAndRejectsUnsupportedPackages(t *testing.T) {
	for _, tc := range []struct {
		name, body, file string
		content          []byte
	}{
		{"invalid_utf8", "Read the report.", "SKILL.md", []byte("---\nname: sample-skill\ndescription: \xff\n---\nRead.\n")},
		{"missing_reference", "Read [guide](references/missing.md).", "", nil},
		{"escaped_reference", "Read [guide](%2e%2e/secret.md).", "", nil},
		{"compiled_python", "Read the report.", "helper.pyc", []byte("compiled")},
		{"name_mismatch", "Read.", "SKILL.md", []byte("---\nname: another\ndescription: Read.\n---\nRead.\n")},
		{"permission_override", "Read.", "SKILL.md", []byte("---\nname: sample-skill\ndescription: Read.\nallowed-tools: bash\n---\nRead.\n")},
		{"oversize", "Read.", "large.txt", []byte(strings.Repeat("x", (128<<10)+1))},
		{"credential_literal", "Read reports.\napi_key = \"synthetic-test-only-credential\"", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := fixture(t, tc.body)
			if tc.file != "" {
				if err := os.WriteFile(filepath.Join(dir, tc.file), tc.content, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Validate(context.Background(), dir); err == nil {
				t.Fatal("invalid skill passed native validation")
			}
		})
	}
}

func TestPackageContainsValidatedImmutableUTF8FilesAndNeverOverwrites(t *testing.T) {
	ctx := context.Background()
	dir := fixture(t, "核验 [说明](guide.md)。")
	guide := []byte("UTF-8 支持文件。\n")
	if err := os.WriteFile(filepath.Join(dir, "guide.md"), guide, 0600); err != nil {
		t.Fatal(err)
	}
	validated, err := Validate(ctx, dir)
	if err != nil || validated.Name != "sample-skill" || validated.FileCount != 2 || validated.Version == "" || validated.Behavior != "not_run" {
		t.Fatalf("validation: %+v %v", validated, err)
	}
	output := filepath.Join(t.TempDir(), "sample-skill.skill")
	packed, err := Package(ctx, dir, output)
	if err != nil || packed.Version != validated.Version {
		t.Fatalf("package: %+v %v", packed, err)
	}
	reader, err := zip.OpenReader(output)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if len(reader.File) != 2 || reader.File[0].Name != "sample-skill/SKILL.md" || reader.File[1].Name != "sample-skill/guide.md" {
		t.Fatalf("archive paths: %+v", reader.File)
	}
	r, err := reader.File[1].Open()
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || string(data) != string(guide) {
		t.Fatalf("archive lost UTF8: %q %v", data, err)
	}
	if _, err := Package(ctx, dir, output); err == nil {
		t.Fatal("packager overwrote an existing artifact")
	}
	if _, err := Package(ctx, dir, filepath.Join(dir, "inside.skill")); err == nil {
		t.Fatal("archive output inside input accepted")
	}
}

func TestPackageRefusesLinksAndBrokenInputWithoutLeavingOutput(t *testing.T) {
	ctx := context.Background()
	dir := fixture(t, "Read [missing](absent.md).")
	output := filepath.Join(t.TempDir(), "bad.skill")
	if _, err := Package(ctx, dir, output); err == nil {
		t.Fatal("broken reference was packaged")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("invalid package left output: %v", err)
	}
	dir = fixture(t, "Read the report.")
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("do not package"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "linked.txt")); err != nil {
		t.Skip("symlink capability unavailable:", err)
	}
	if _, err := Package(ctx, dir, output); err == nil {
		t.Fatal("linked file escaped package root")
	}
}

func TestPackageFailureAndCancellationRemoveOnlyCreatedOutput(t *testing.T) {
	for _, cancelAfterWrite := range []bool{false, true} {
		t.Run(fmtTestName(cancelAfterWrite), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			dir := fixture(t, "Read the report.")
			output := filepath.Join(t.TempDir(), "sample-skill.skill")
			_, err := packageSkill(ctx, dir, output, func(_ context.Context, file *os.File, _ skills.AuthoringPackage) error {
				if _, err := file.Write([]byte("partial archive")); err != nil {
					return err
				}
				if cancelAfterWrite {
					cancel()
					return nil
				}
				return io.ErrShortWrite
			})
			want := io.ErrShortWrite
			if cancelAfterWrite {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatalf("expected write/cancellation failure: %v", err)
			}
			if _, err := os.Lstat(output); !os.IsNotExist(err) {
				t.Fatalf("partial archive was not removed: %v", err)
			}
		})
	}
}

func TestPackageFailurePreservesReplacedOutput(t *testing.T) {
	dir := fixture(t, "Read the report.")
	output := filepath.Join(t.TempDir(), "sample-skill.skill")
	moved := output + ".moved"
	replacement := []byte("independent replacement archive")
	_, err := packageSkill(context.Background(), dir, output, func(_ context.Context, file *os.File, _ skills.AuthoringPackage) error {
		if _, err := file.Write([]byte("partial archive")); err != nil {
			return err
		}
		// Allow the replacement to happen after closing, as it can before
		// failed-package deferred cleanup. Do not rely on open-handle locks.
		if err := file.Close(); err != nil {
			return err
		}
		if err := os.Rename(output, moved); err != nil {
			return err
		}
		if err := os.WriteFile(output, replacement, 0600); err != nil {
			return err
		}
		return io.ErrShortWrite
	})
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("original write failure lost: %v", err)
	}
	data, err := os.ReadFile(output)
	if err != nil || string(data) != string(replacement) {
		t.Fatalf("independent replacement was changed: %q %v", data, err)
	}
	if data, err := os.ReadFile(moved); err != nil || string(data) != "partial archive" {
		t.Fatalf("relocated output was changed: %q %v", data, err)
	}
}
