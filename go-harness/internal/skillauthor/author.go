// Package skillauthor implements local templates, validation and distributable
// archives. It never runs support scripts, calls a model or publishes skills.
package skillauthor

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills/evolution"
	"gopkg.in/yaml.v3"
)

type Report struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Version     string                 `json:"version"`
	FileCount   int                    `json:"file_count"`
	Bytes       int64                  `json:"bytes"`
	Behavior    string                 `json:"behavior"`
	Findings    []harness.SkillFinding `json:"findings"`
}

func Initialize(ctx context.Context, parent, name, description string) (string, error) {
	return initialize(ctx, parent, name, description, func(file *os.File, content []byte) error {
		written, err := file.Write(content)
		if err == nil && written != len(content) {
			err = io.ErrShortWrite
		}
		return err
	})
}

func removeCreatedDirectory(root *os.Root, name string, expected os.FileInfo) error {
	current, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if current.Mode()&os.ModeSymlink != 0 || !os.SameFile(current, expected) {
		return errors.New("new skill directory changed; replacement retained")
	}
	return root.Remove(name)
}

func initialize(ctx context.Context, parent, name, description string, write func(*os.File, []byte) error) (string, error) {
	if !harness.ValidSkillName(name) || !utf8.ValidString(description) || strings.TrimSpace(description) == "" || len(description) > 1024 || strings.ContainsRune(description, 0) {
		return "", errors.New("name and a nonempty UTF-8 description (at most 1024 bytes) are required")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	parent, err := filepath.Abs(parent)
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return "", err
	}
	defer root.Close()
	header, err := yaml.Marshal(map[string]string{"name": name, "description": strings.TrimSpace(description)})
	if err != nil {
		return "", err
	}
	content := append([]byte("---\n"), header...)
	content = append(content, []byte("---\n\n# "+strings.ReplaceAll(name, "-", " ")+"\n\n[TODO: Write the concrete workflow, required inputs, and verification criteria.]\n")...)
	if err = root.Mkdir(name, 0700); err != nil {
		return "", fmt.Errorf("create new skill directory: %w", err)
	}
	// Root.Lstat captures identity through a handle on Windows; os.Lstat may
	// defer loading the file ID until SameFile and then observe a replacement.
	directory, err := root.Lstat(name)
	if err != nil {
		return "", err
	}
	if !directory.IsDir() || directory.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("new skill directory changed before opening")
	}
	child, err := root.OpenRoot(name)
	if err != nil {
		return "", errors.Join(err, removeCreatedDirectory(root, name, directory))
	}
	defer child.Close()
	openedDirectory, err := child.Stat(".")
	if err != nil || !os.SameFile(directory, openedDirectory) {
		return "", errors.New("new skill directory changed while opening")
	}
	file, err := child.OpenFile("SKILL.md", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		_ = child.Close()
		return "", errors.Join(err, removeCreatedDirectory(root, name, directory))
	}
	createdFile, err := file.Stat()
	if err == nil {
		err = write(file, content)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		// Remove only our exclusive file through the pinned directory handle.
		// A replaced file or an additional user file is never deleted.
		current, lookupErr := child.Lstat("SKILL.md")
		if lookupErr == nil && createdFile != nil && os.SameFile(current, createdFile) {
			err = errors.Join(err, child.Remove("SKILL.md"))
		}
		_ = child.Close()
		return "", errors.Join(err, removeCreatedDirectory(root, name, directory))
	}
	return filepath.Join(parent, name), nil
}

func read(ctx context.Context, directory string) (skills.AuthoringPackage, Report, error) {
	var report Report
	p, err := skills.ReadAuthoringPackage(ctx, directory)
	if err != nil {
		return p, report, err
	}
	if err = evolution.ValidateAuthoringFiles(p.Files); err != nil {
		return p, report, err
	}
	for _, finding := range p.Findings {
		if finding.Code == "possible_credential" {
			return p, report, errors.New("candidate contains a credential-like literal")
		}
	}
	if bytes.Contains(p.Files["SKILL.md"], []byte("[TODO:")) {
		return p, report, errors.New("complete the generated skill scaffold before validation")
	}
	return p, Report{Name: p.Name, Description: p.Description, Version: p.Version, FileCount: len(p.Files), Bytes: p.Bytes, Behavior: "not_run", Findings: p.Findings}, nil
}

func Validate(ctx context.Context, directory string) (Report, error) {
	_, report, err := read(ctx, directory)
	return report, err
}

func Package(ctx context.Context, directory, output string) (Report, error) {
	return packageSkill(ctx, directory, output, writeArchive)
}

func removeCreatedOutput(root *os.Root, name string, expected os.FileInfo) error {
	current, err := root.Lstat(name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if expected == nil || !current.Mode().IsRegular() || !os.SameFile(current, expected) {
		return errors.New("new archive output changed; replacement retained")
	}
	return root.Remove(name)
}

func packageSkill(ctx context.Context, directory, output string, write func(context.Context, *os.File, skills.AuthoringPackage) error) (report Report, err error) {
	p, report, err := read(ctx, directory)
	if err != nil {
		return report, err
	}
	source, err := filepath.Abs(directory)
	if err != nil {
		return report, err
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return report, err
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return report, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(output))
	if err != nil {
		return report, err
	}
	output = filepath.Join(parent, filepath.Base(output))
	rel, err := filepath.Rel(source, output)
	if err != nil {
		return report, err
	}
	if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return report, errors.New("archive output must be outside the skill directory")
	}
	if strings.ToLower(filepath.Ext(output)) != ".skill" {
		return report, errors.New("archive output requires the .skill extension")
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return report, err
	}
	defer root.Close()
	f, err := root.OpenFile(filepath.Base(output), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return report, err
	}
	createdFile, err := f.Stat()
	complete := false
	defer func() {
		_ = f.Close()
		if !complete {
			// Refuse cleanup when the path no longer identifies our file.
			// The identity check and removal are separate filesystem operations.
			err = errors.Join(err, removeCreatedOutput(root, filepath.Base(output), createdFile))
		}
	}()
	if err != nil {
		return report, err
	}
	err = write(ctx, f, p)
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return report, err
	}
	complete = true
	return report, nil
}

func writeArchive(ctx context.Context, f *os.File, p skills.AuthoringPackage) error {
	archive := zip.NewWriter(f)
	var err error
	for _, entry := range p.Manifest {
		if err = ctx.Err(); err != nil {
			break
		}
		var writer io.Writer
		writer, err = archive.Create(p.Name + "/" + entry.Path)
		if err != nil {
			break
		}
		_, err = writer.Write(p.Files[entry.Path])
		if err != nil {
			break
		}
	}
	return errors.Join(err, archive.Close())
}
