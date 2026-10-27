package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

type MigrationItem struct {
	FactID      string `json:"fact_id"`
	Revision    int64  `json:"revision"`
	ContentSHA  string `json:"content_sha256"`
	Workspace   string `json:"workspace"`
	Destination string `json:"destination"`
	Key         string `json:"key"`
}
type MigrationReport struct {
	Personal int    `json:"personal"`
	Channel  int    `json:"channel"`
	Name     string `json:"name"`
}

// MigrateSingleUser consumes an operator-reviewed manifest. No automatic global
// promotion is performed on startup. The transaction is repeatable and preserves
// original timestamps, source input/run IDs and all revision history.
func (s *Store) MigrateSingleUser(ctx context.Context, name string, items []MigrationItem) (MigrationReport, error) {
	profile, err := s.ProfileID(ctx)
	if err != nil {
		return MigrationReport{}, err
	}
	global, err := NewScope(GlobalScope, "", "", profile, "")
	if err != nil {
		return MigrationReport{}, err
	}
	report := MigrationReport{Name: name}
	if name == "" || len(name) > 128 || len(items) > 1000 {
		return report, fmt.Errorf("invalid migration manifest")
	}
	err = transact(ctx, s.db, func(tx *sql.Tx) error {
		var prior []byte
		err := tx.QueryRowContext(ctx, `SELECT report FROM memory_migrations WHERE name=?`, name).Scan(&prior)
		if err == nil {
			return json.Unmarshal(prior, &report)
		}
		if err != sql.ErrNoRows {
			return err
		}
		if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS memory_channel_preferences(channel TEXT NOT NULL,slot_key TEXT NOT NULL,content TEXT NOT NULL,source_fact_id TEXT NOT NULL,updated_at TEXT NOT NULL,PRIMARY KEY(channel,slot_key))`); err != nil {
			return err
		}
		for _, item := range items {
			origin, err := NewScope(WorkspaceScope, item.Workspace, "", "", "")
			if err != nil {
				return err
			}
			f, err := readFact(ctx, tx, origin, item.FactID)
			if err != nil {
				return err
			}
			hash := sha256.Sum256([]byte(f.Content))
			if f.Revision != item.Revision || hex.EncodeToString(hash[:]) != item.ContentSHA {
				return ErrVersionConflict
			}
			key := CanonicalKey(item.Key)
			if !slotPattern.MatchString(key) {
				return fmt.Errorf("invalid migration key")
			}
			switch item.Destination {
			case "personal":
				if f.Category != "profile" && f.Category != "preference" {
					return fmt.Errorf("not a personal fact")
				}
				existing, _, err := slotHead(ctx, tx, global, key)
				if err != nil {
					return err
				}
				if existing.ID != "" {
					return fmt.Errorf("personal attribute already exists; review migration")
				}
				version, err := checkScope(ctx, tx, global, true)
				if err != nil {
					return err
				}
				version, err = bumpScope(ctx, tx, global, version)
				if err != nil {
					return err
				}
				copy := f
				identity := sha256.Sum256([]byte(global.key + "\x00" + key))
				copy.ID = "mem-" + hex.EncodeToString(identity[:16])
				copy.ScopeKey = global.key
				copy.Revision = 1
				copy.Source.ID = OperationID("migration", name, f.ID)
				copy.Source.PolicyVersion = "single-user-migration-v1"
				if _, err = tx.ExecContext(ctx, `INSERT INTO memory_facts(id,scope_key,head_revision,created_at) VALUES(?,?,1,?)`, copy.ID, copy.ScopeKey, copy.CreatedAt.Format(time.RFC3339Nano)); err != nil {
					return err
				}
				if err = insertRevision(ctx, tx, copy, version, false); err != nil {
					return err
				}
				if _, err = tx.ExecContext(ctx, `INSERT INTO memory_slots(scope_key,slot_key,fact_id) VALUES(?,?,?)`, global.key, key, copy.ID); err != nil {
					return err
				}
				report.Personal++
			case "folotoy":
				if f.Category != "preference" {
					return fmt.Errorf("not a channel preference")
				}
				if _, err = tx.ExecContext(ctx, `INSERT INTO memory_channel_bindings(workspace,channel) VALUES(?,'folotoy') ON CONFLICT(workspace) DO NOTHING`, origin.workspace); err != nil {
					return err
				}
				_, err = tx.ExecContext(ctx, `INSERT INTO memory_channel_preferences(channel,slot_key,content,source_fact_id,updated_at) VALUES('folotoy',?,?,?,?) ON CONFLICT(channel,slot_key) DO UPDATE SET content=excluded.content,source_fact_id=excluded.source_fact_id,updated_at=excluded.updated_at WHERE excluded.updated_at>memory_channel_preferences.updated_at`, key, f.Content, f.ID, f.UpdatedAt.Format(time.RFC3339Nano))
				if err != nil {
					return err
				}
				report.Channel++
			default:
				return fmt.Errorf("unknown migration destination")
			}
			version, err := checkScope(ctx, tx, origin, false)
			if err != nil {
				return err
			}
			version, err = bumpScope(ctx, tx, origin, version)
			if err != nil {
				return err
			}
			f.Revision++
			f.Source = Source{ID: OperationID("migration-delete", name, f.ID), Kind: "operator", ActorID: "single-user-migration", PolicyVersion: "single-user-migration-v1"}
			f.UpdatedAt = time.Now().UTC()
			if _, err = tx.ExecContext(ctx, `UPDATE memory_facts SET head_revision=?,deleted=1 WHERE id=?`, f.Revision, f.ID); err != nil {
				return err
			}
			if err = insertRevision(ctx, tx, f, version, true); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM memory_vectors WHERE fact_id=?`, f.ID); err != nil {
				return err
			}
		}
		raw, _ := json.Marshal(report)
		_, err = tx.ExecContext(ctx, `INSERT INTO memory_migrations(name,report,created_at) VALUES(?,?,?)`, name, raw, time.Now().UTC().Format(time.RFC3339Nano))
		return err
	})
	return report, err
}
