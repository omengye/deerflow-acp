package configschema

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
	"go.yaml.in/yaml/v3"
)

const EvolutionPolicyVersion = "skill-evolution-v2"

// RemoveEvolutionConfiguration operates only on the retired feature settings.
// Source permissions, models and other features keep their existing contract.
func RemoveEvolutionConfiguration(document map[string]any) bool {
	changed := false
	for _, key := range []string{"skill_evolution", "skill_evolution_migration_ack", "skill_evolution_migration"} {
		if _, ok := document[key]; ok {
			delete(document, key)
			changed = true
		}
	}
	return changed
}

// MigrateEvolutionYAML is the only decoder for the retired configuration.
// It preserves YAML nodes (including comments and environment references) and
// rejects ambiguous documents before deleting any fields.
func MigrateEvolutionYAML(raw []byte) ([]byte, bool, error) {
	if len(raw) > 2<<20 {
		return nil, false, errors.New("config exceeds 2 MiB")
	}
	var doc yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&doc); err != nil {
		return nil, false, err
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, false, errors.New("expected one configuration document")
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, false, errors.New("configuration must be an object")
	}
	var checked map[string]any
	if err := doc.Decode(&checked); err != nil {
		return nil, false, err
	}
	changed := RemoveEvolutionConfiguration(checked)
	if !changed {
		return raw, false, nil
	}
	root := doc.Content[0]
	kept := make([]*yaml.Node, 0, len(root.Content))
	for i := 0; i < len(root.Content); i += 2 {
		switch root.Content[i].Value {
		case "skill_evolution", "skill_evolution_migration_ack", "skill_evolution_migration":
			continue
		}
		kept = append(kept, root.Content[i], root.Content[i+1])
	}
	root.Content = kept
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(&doc); err != nil {
		return nil, false, err
	}
	if err := encoder.Close(); err != nil {
		return nil, false, err
	}
	return out.Bytes(), true, nil
}

type evolutionConfigJournal struct {
	Epoch     string `json:"pipeline_epoch,omitempty"`
	Watermark int64  `json:"input_watermark,omitempty"`
	Version   string `json:"version"`
	ID        string `json:"migration_id"`
	Before    string `json:"before_sha256"`
	After     string `json:"after_sha256"`
	State     string `json:"state"`
}

func evolutionDigest(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }

// MigrateEvolutionFile shares the configuration writer's lock. A prepared
// journal plus atomic replacement makes interruption recoverable; no model work
// or database mutation happens here. The runtime creates its input epoch before
// starting any producer, independently of whether a legacy config was present.
func MigrateEvolutionFile(path string) error {
	lock := flock.New(filepath.Join(filepath.Dir(path), ".config-tool.lock"))
	if err := lock.Lock(); err != nil {
		return err
	}
	defer lock.Unlock()
	return MigrateEvolutionFileLocked(path)
}

// MigrateEvolutionFileLocked requires the caller to own .config-tool.lock.
func MigrateEvolutionFileLocked(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	updated, changed, err := MigrateEvolutionYAML(raw)
	if err != nil {
		return fmt.Errorf("migrate skill evolution configuration: %w", err)
	}
	journalPath := filepath.Join(filepath.Dir(path), ".skill-evolution-migration.json")
	if changed {
		body, readErr := os.ReadFile(journalPath)
		if readErr == nil {
			var prior evolutionConfigJournal
			if json.Unmarshal(body, &prior) != nil || prior.Version != EvolutionPolicyVersion {
				return errors.New("invalid skill evolution migration journal")
			}
			if prior.State == "config_written" || prior.State == "complete" {
				return errors.New("skill_evolution configuration is retired; remove reintroduced legacy fields")
			}
			if prior.Before != evolutionDigest(raw) || prior.After != evolutionDigest(updated) {
				return errors.New("configuration changed during skill evolution migration")
			}
		} else if !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		}
	}
	if !changed {
		body, readErr := os.ReadFile(journalPath)
		if errors.Is(readErr, os.ErrNotExist) {
			sha := evolutionDigest(raw)
			journal := evolutionConfigJournal{Version: EvolutionPolicyVersion, ID: evolutionDigest([]byte(path + "\n" + sha)), Before: sha, After: sha, State: "config_written"}
			body, _ := json.Marshal(journal)
			return writeEvolutionFile(journalPath, body, 0600)
		}
		if readErr != nil {
			return readErr
		}
		var journal evolutionConfigJournal
		if json.Unmarshal(body, &journal) != nil || journal.Version != EvolutionPolicyVersion {
			return errors.New("invalid skill evolution migration journal")
		}
		if journal.State == "config_written" || journal.State == "complete" {
			return nil
		}
		if journal.After != evolutionDigest(raw) {
			return errors.New("configuration changed during skill evolution migration")
		}
		journal.State = "config_written"
		body, _ = json.Marshal(journal)
		return writeEvolutionFile(journalPath, body, 0600)
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	journal := evolutionConfigJournal{Version: EvolutionPolicyVersion, Before: evolutionDigest(raw), After: evolutionDigest(updated), State: "prepared"}
	journal.ID = evolutionDigest([]byte(path + "\n" + journal.Before))
	body, _ := json.Marshal(journal)
	if err = writeEvolutionFile(journalPath, body, 0600); err != nil {
		return err
	}
	if err = writeEvolutionFile(path, updated, info.Mode().Perm()); err != nil {
		return err
	}
	journal.State = "config_written"
	body, _ = json.Marshal(journal)
	return writeEvolutionFile(journalPath, body, 0600)
}

func writeEvolutionFile(path string, raw []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".evolution-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(mode); err == nil {
		_, err = file.Write(raw)
	}
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func EvolutionMigrationID(path string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(path), ".skill-evolution-migration.json"))
	if err != nil {
		return "", err
	}
	var j evolutionConfigJournal
	if json.Unmarshal(raw, &j) != nil || j.Version != EvolutionPolicyVersion || (j.State != "config_written" && j.State != "complete") || j.ID == "" {
		return "", errors.New("incomplete automatic evolution configuration migration")
	}
	return j.ID, nil
}

// CompleteEvolutionFile is the final startup fence, after database migrations
// and reference recovery and before maintenance workers. A crash on either
// side resumes the same epoch and watermark on the next startup.
func CompleteEvolutionFile(ctx context.Context, path string, db *sql.DB) error {
	lock := flock.New(filepath.Join(filepath.Dir(path), ".config-tool.lock"))
	if err := lock.Lock(); err != nil {
		return err
	}
	defer lock.Unlock()
	journalPath := filepath.Join(filepath.Dir(path), ".skill-evolution-migration.json")
	raw, err := os.ReadFile(journalPath)
	if err != nil {
		return err
	}
	var j evolutionConfigJournal
	if json.Unmarshal(raw, &j) != nil || j.Version != EvolutionPolicyVersion || (j.State != "config_written" && j.State != "complete") {
		return errors.New("incomplete evolution configuration journal")
	}
	var id, epoch, policy string
	var watermark int64
	if err = db.QueryRowContext(ctx, `SELECT migration_id,pipeline_epoch,policy_version,input_watermark FROM df_evolution_epoch WHERE id=1`).Scan(&id, &epoch, &policy, &watermark); err != nil {
		return err
	}
	if id != j.ID || policy != j.Version || epoch == "" || watermark < 0 {
		return errors.New("evolution database and configuration migration differ")
	}
	if j.State == "complete" {
		if j.Epoch != epoch || j.Watermark != watermark {
			return errors.New("evolution migration watermark changed")
		}
		return nil
	}
	j.State, j.Epoch, j.Watermark = "complete", epoch, watermark
	raw, _ = json.Marshal(j)
	return writeEvolutionFile(journalPath, raw, 0600)
}
