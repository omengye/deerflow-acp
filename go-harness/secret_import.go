package deerflow

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofrs/flock"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/configschema"
	"github.com/omengye/deerflow-acp/go-harness/internal/secrets"
	"github.com/omengye/deerflow-acp/go-harness/internal/websearch"
)

// secretSources are the host settings files that secret_manage import_config
// moves plaintext credentials out of.
type secretSources struct {
	config string             // config.yaml the runtime started with
	search *websearch.Manager // saved search settings
}

// Fields of the saved search settings carry this prefix, so they never
// collide with config.yaml fields.
const searchSettingsField = "search-providers.json:"

type importCandidate struct {
	field, secret, value string
	provider             string // search settings provider; empty for config.yaml
}

type importPlan struct {
	importCandidate
	create bool
	sameAs string
	reuse  secrets.Metadata
}

type importedSetting struct {
	Field  string `json:"field"`
	Secret string `json:"secret"`
	Action string `json:"action"`
	SameAs string `json:"same_value_as,omitempty"`
}

type unchangedSetting struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

func onlyImportArguments(in secretManageInput) error {
	if in.Name != "" || in.Kind != "" || in.Value != "" || in.ValueSealed != "" || in.FromPath != "" || in.Generate != nil || in.Description != nil || in.FileName != "" || in.DeleteSource || in.Version != 0 {
		return fmt.Errorf("%w: import_config accepts only dry_run and fields", harness.ErrInvalidInput)
	}
	return nil
}

func (t *secretTool) importConfig(ctx context.Context, in secretManageInput) (any, error) {
	if err := onlyImportArguments(in); err != nil {
		return nil, err
	}
	if t.sources.config == "" {
		return nil, fmt.Errorf("%w: this runtime was started without a config.yaml, so there is nothing to import", harness.ErrInvalidInput)
	}
	// Nothing is written until the secrets are stored.
	notExecuted := harness.MarkToolNotExecuted
	// Resolve symlinks before taking the lock so the lock always lives beside
	// the real configuration file, matching desktopconfig.
	path, err := filepath.EvalSymlinks(t.sources.config)
	if err != nil {
		return nil, notExecuted(err)
	}
	unlock, err := lockConfig(ctx, path)
	if err != nil {
		return nil, err
	}
	defer unlock()
	raw, mode, err := readConfigFile(path)
	if err != nil {
		return nil, notExecuted(err)
	}
	found, err := configschema.FindPlaintextCredentials(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", harness.ErrInvalidInput, err)
	}
	var candidates []importCandidate
	unchanged := []unchangedSetting{}
	for _, c := range found {
		if c.Reason != "" {
			unchanged = append(unchanged, unchangedSetting{c.Field, c.Reason})
			continue
		}
		candidates = append(candidates, importCandidate{field: c.Field, secret: c.Secret, value: c.Value})
	}
	if t.sources.search != nil {
		keys, err := t.sources.search.StoredPlaintextKeys()
		if err != nil {
			return nil, notExecuted(err)
		}
		for _, name := range websearch.Names {
			if value, ok := keys[name]; ok {
				candidates = append(candidates, importCandidate{field: searchSettingsField + "providers." + name + ".api_key", secret: configschema.SecretName("SEARCH", name, "API_KEY"), value: value, provider: name})
			}
		}
		if pinned, err := t.sources.search.PinnedPlaintextVersions(); err == nil && pinned > 0 {
			unchanged = append(unchanged, unchangedSetting{"search-providers.json.versions", fmt.Sprintf("%d saved search configurations that earlier runs are pinned to keep their api keys, so those runs can resume unchanged; they are not rewritten", pinned)})
		}
	}
	selected, err := selectImports(candidates, in.Fields)
	if err != nil {
		return nil, err
	}
	for i := 0; i < len(selected); {
		if c := selected[i]; len(c.value) > secrets.MaxStringBytes || !utf8.ValidString(c.value) {
			unchanged = append(unchanged, unchangedSetting{c.field, fmt.Sprintf("the value cannot be a string secret: it is longer than %d bytes or not UTF-8 text", secrets.MaxStringBytes)})
			selected = slices.Delete(selected, i, i+1)
			continue
		}
		i++
	}
	writer, release := t.vault.BeginImport()
	defer release()
	plan, err := t.planImports(ctx, selected)
	if err != nil {
		return nil, notExecuted(err)
	}
	out := map[string]any{"config": filepath.Base(path), "unchanged": unchanged}
	if len(plan) == 0 {
		out["status"], out["imported"] = "nothing_to_import", []importedSetting{}
		out["note"] = "No setting this runtime reads holds a plaintext credential."
		return out, nil
	}
	names := map[string]string{}
	search := map[string]websearch.KeyReference{}
	for _, p := range plan {
		if p.provider == "" {
			names[p.field] = p.secret
		} else {
			search[p.provider] = websearch.KeyReference{Value: p.value, Secret: p.secret}
		}
	}
	// Rewrite in memory first: a config.yaml that cannot be rewritten stores
	// nothing, and a dry run reports it.
	var updated []byte
	if len(names) > 0 {
		if updated, err = configschema.ReplaceCredentials(raw, names); err != nil {
			return nil, fmt.Errorf("%w: %v", harness.ErrInvalidInput, err)
		}
	}
	if in.DryRun {
		out["status"], out["imported"] = "dry_run", importReport(plan, "create", "reuse")
		out["note"] = "Nothing was changed. Run import_config without dry_run to store these values and replace them with ${secret:NAME} references; fields limits the import to some of these settings."
		return out, nil
	}
	var creates []secrets.SetRequest
	reused := map[string]secrets.Metadata{}
	imported := map[string]string{}
	defer func() {
		for _, req := range creates {
			clear(req.Value)
		}
	}()
	for _, p := range plan {
		if p.provider == "" {
			imported[p.value] = p.secret
		}
		if !p.create {
			if p.reuse.Name != "" {
				reused[p.secret] = p.reuse
			}
			continue
		}
		source := "config.yaml " + p.field
		if p.provider != "" {
			source = strings.Replace(p.field, ":", " ", 1)
		}
		description := truncateUTF8("Imported from "+source, secrets.MaxDescriptionBytes)
		creates = append(creates, secrets.SetRequest{Name: p.secret, Kind: secrets.KindString, Value: []byte(p.value), Description: &description, Source: "import", Actor: t.actor()})
	}
	if err = writer.Store(ctx, creates, reused); err != nil {
		return nil, err
	}
	publish := func() error {
		if len(names) > 0 {
			if err := replaceConfigFile(path, raw, updated, mode); err != nil {
				return fmt.Errorf("the secrets are stored, but config.yaml was not changed: %v; run import_config again, which reuses them", err)
			}
		}
		return nil
	}
	report := importReport(plan, "created", "reused")
	if t.sources.search != nil {
		changed, err := t.sources.search.PublishMigration(ctx, search, updated, imported, publish)
		if err != nil {
			return nil, fmt.Errorf("the secrets are stored; retry publishing the configuration: %w", err)
		}
		for i := range report {
			if p := plan[i]; p.provider != "" && !slices.Contains(changed, p.provider) {
				report[i].Action = "stored; the setting changed meanwhile and was left alone"
			}
		}
	} else if err = publish(); err != nil {
		return nil, err
	}
	out["status"], out["imported"] = "imported", report
	out["note"] = "These settings now refer to the vault. The running DeerFlow keeps the values it read at start; it reads them from the vault when it next starts, and search reads its keys at each request. While a setting refers to a secret, do not delete or rename it; a new value takes effect at the next start."
	return out, nil
}

// selectImports keeps the candidates named in fields, or all of them.
func selectImports(candidates []importCandidate, fields []string) ([]importCandidate, error) {
	if fields == nil {
		return candidates, nil
	}
	if len(fields) == 0 || len(fields) > 256 {
		return nil, fmt.Errorf("%w: fields must name 1 to 256 settings", harness.ErrInvalidInput)
	}
	var selected []importCandidate
	for _, c := range candidates {
		if slices.Contains(fields, c.field) {
			selected = append(selected, c)
		}
	}
	if len(selected) != len(slices.Compact(slices.Sorted(slices.Values(fields)))) {
		// Never echo the fields: a misplaced value can arrive as one.
		return nil, fmt.Errorf("%w: fields must name settings that a dry run lists under imported", harness.ErrInvalidInput)
	}
	return selected, nil
}

// planImports names a secret for each value: one that already holds it, or
// a new one, so equal values share a secret and nothing is overwritten.
func (t *secretTool) planImports(ctx context.Context, selected []importCandidate) ([]importPlan, error) {
	if len(selected) == 0 {
		return nil, nil
	}
	existing, err := t.vault.List(ctx, true)
	if err != nil {
		return nil, err
	}
	byName := map[string]secrets.Metadata{}
	live := 0
	for _, m := range existing {
		if m.DeletedAt == nil {
			live++
		}
		byName[m.Name] = m
	}
	holds := func(m secrets.Metadata, fingerprint string) bool {
		return !secrets.IsCredentialName(m.Name) && m.DeletedAt == nil && !m.Unavailable && m.Kind == secrets.KindString && m.Fingerprint == fingerprint
	}
	chosen, first, taken := map[string]string{}, map[string]string{}, map[string]bool{}
	plan := make([]importPlan, 0, len(selected))
	created := 0
	for _, c := range selected {
		p := importPlan{importCandidate: c}
		if name, ok := chosen[c.value]; ok {
			p.secret, p.sameAs = name, first[c.value]
			plan = append(plan, p)
			continue
		}
		fingerprint := t.vault.Fingerprint([]byte(c.value))
		match := ""
		if m, ok := byName[c.secret]; ok && holds(m, fingerprint) {
			match = c.secret
		}
		for _, m := range existing {
			if match == "" && holds(m, fingerprint) {
				match = m.Name
			}
		}
		if match != "" {
			p.secret = match
			p.reuse = byName[match]
		} else {
			p.secret, p.create = freeSecretName(c.secret, byName, taken), true
			created++
			if p.secret == "" {
				return nil, fmt.Errorf("%w: no free secret name for %s", secrets.ErrInvalid, c.field)
			}
		}
		chosen[c.value], first[c.value], taken[p.secret] = p.secret, c.field, true
		plan = append(plan, p)
	}
	if live+created > secrets.MaxSecrets {
		return nil, fmt.Errorf("%w: importing needs %d new secrets, and the vault holds at most %d; delete unused ones first", secrets.ErrInvalid, created, secrets.MaxSecrets)
	}
	return plan, nil
}

// freeSecretName returns base, or base with the first free _2, _3... suffix:
// a name used by another value, even a deleted one, is never reused.
func freeSecretName(base string, byName map[string]secrets.Metadata, taken map[string]bool) string {
	for i := 1; i <= 1000; i++ {
		name := base
		if i > 1 {
			suffix := "_" + strconv.Itoa(i)
			name = strings.TrimRight(base[:min(len(base), secrets.MaxNameBytes-len(suffix))], "_") + suffix
		}
		if _, used := byName[name]; !used && !taken[name] && secrets.ValidName(name) {
			return name
		}
	}
	return ""
}

func importReport(plan []importPlan, create, reuse string) []importedSetting {
	report := make([]importedSetting, 0, len(plan))
	for _, p := range plan {
		action := reuse
		if p.create {
			action = create
		}
		report = append(report, importedSetting{Field: p.field, Secret: p.secret, Action: action, SameAs: p.sameAs})
	}
	return report
}

// configLockWait bounds how long an import waits for another config writer.
var configLockWait = 10 * time.Second

// lockConfig takes the lock that every DeerFlow config.yaml writer shares.
func lockConfig(ctx context.Context, config string) (func(), error) {
	lock := flock.New(filepath.Join(filepath.Dir(config), ".config-tool.lock"))
	wait, cancel := context.WithTimeout(ctx, configLockWait)
	defer cancel()
	locked, err := lock.TryLockContext(wait, 50*time.Millisecond)
	if err != nil || !locked {
		if ctx.Err() != nil {
			return nil, harness.MarkToolNotExecuted(ctx.Err())
		}
		return nil, harness.MarkToolNotExecuted(errors.New("another tool is changing config.yaml; retry shortly"))
	}
	return func() { _ = lock.Unlock() }, nil
}

func readConfigFile(path string) ([]byte, fs.FileMode, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%w: config.yaml is not a regular file", harness.ErrInvalidInput)
	}
	raw, err := io.ReadAll(io.LimitReader(file, configschema.MaxConfigBytes+1))
	if err != nil {
		return nil, 0, err
	}
	if len(raw) > configschema.MaxConfigBytes {
		return nil, 0, fmt.Errorf("%w: config.yaml exceeds 2 MiB", harness.ErrInvalidInput)
	}
	return raw, info.Mode().Perm(), nil
}

// replaceConfigFile atomically replaces path, which held before, with after.
// An editor that does not take the config lock may have changed the file;
// then nothing is replaced.
func replaceConfigFile(path string, before, after []byte, mode fs.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".deerflow-config-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(mode); err == nil {
		_, err = file.Write(after)
	}
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	current, _, err := readConfigFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, before) {
		return errors.New("config.yaml changed while the import ran")
	}
	return os.Rename(file.Name(), path)
}

// configReferences lists the settings that refer to secret name. It is best
// effort: a settings file that cannot be read lists nothing.
func (t *secretTool) configReferences(name string) []string {
	var fields []string
	if t.sources.config != "" {
		if raw, _, err := readConfigFile(t.sources.config); err == nil {
			if refs, err := configschema.SecretReferenceFields(raw); err == nil {
				fields = append(fields, refs[name]...)
			}
		}
	}
	if t.sources.search != nil {
		if refs, err := t.sources.search.SecretReferences(); err == nil {
			for _, provider := range refs[name] {
				fields = append(fields, searchSettingsField+"providers."+provider+".api_key")
			}
		}
	}
	return fields
}

func truncateUTF8(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	text = text[:limit]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text
}
