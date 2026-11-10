package skillmaintenance

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
)

const AutomaticPolicyVersion = "skill-evolution-v2"

// BeginAutomaticEpoch is called under the runtime's exclusive directory lock,
// before any producer starts. Its input boundary is durable and never advances
// on restart. Old calls, publications and content remain available for audit.
func BeginAutomaticEpoch(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS df_evolution_epoch(
 id INTEGER PRIMARY KEY CHECK(id=1),schema_version INTEGER NOT NULL,policy_version TEXT NOT NULL,
 pipeline_epoch TEXT NOT NULL,migration_id TEXT NOT NULL,input_watermark INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS df_evolution_retired_inputs(root_id TEXT NOT NULL,session_id TEXT NOT NULL,input_id TEXT NOT NULL,PRIMARY KEY(root_id,session_id,input_id));
CREATE INDEX IF NOT EXISTS df_evolution_retired_input_identity ON df_evolution_retired_inputs(session_id,input_id);
CREATE TABLE IF NOT EXISTS df_evolution_retired_proposals(proposal_id TEXT PRIMARY KEY);`); err != nil {
		return err
	}
	var version, storedID, storedEpoch string
	err := tx.QueryRowContext(ctx, `SELECT policy_version,migration_id,pipeline_epoch FROM df_evolution_epoch WHERE id=1`).Scan(&version, &storedID, &storedEpoch)
	if err == nil {
		if version != AutomaticPolicyVersion {
			return errors.New("unsupported automatic evolution epoch")
		}
		if id, _ := ctx.Value(migrationIdentityKey{}).(string); id != "" && id != storedID {
			// A host started without a config has an unbound epoch identity.
			// Adding its first config binds that identity without retiring fresh
			// work or moving the original input boundary. A bound config cannot
			// silently replace another config's migration identity.
			if storedID != storedEpoch {
				return errors.New("configuration and database evolution migration identities differ")
			}
			_, err = tx.ExecContext(ctx, `UPDATE df_evolution_epoch SET migration_id=? WHERE id=1 AND migration_id=pipeline_epoch`, id)
			return err
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	epoch := rand.Text()
	migrationID, _ := ctx.Value(migrationIdentityKey{}).(string)
	if migrationID == "" {
		migrationID = epoch
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO df_evolution_epoch VALUES(1,2,?,?,?,(SELECT COALESCE(MAX(sequence),0) FROM harness_events))`, AutomaticPolicyVersion, epoch, migrationID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO df_evolution_retired_inputs SELECT id,session_id,input_id FROM harness_runs`); err != nil {
		return err
	}
	statements := []struct{ table, sql string }{
		{"harness_learning_inputs", `INSERT OR IGNORE INTO df_evolution_retired_inputs SELECT root_id,session_id,input_id FROM harness_learning_inputs`},
		{"budget_members", `INSERT OR IGNORE INTO df_evolution_retired_inputs SELECT m.root_id,r.session_id,r.input_id FROM budget_members m JOIN harness_runs r ON r.id=m.id`},
		{"df_skill_maintenance_jobs", `UPDATE df_skill_maintenance_jobs SET state='cancelled',state_version=state_version+1,token='',lease_ns=0,retry_ns=0,error_code='historical_epoch' WHERE state IN ('queued','retry','running','waiting')`},
		{"harness_learning_outbox", `UPDATE harness_learning_outbox SET state='skipped',reason='historical_epoch' WHERE state IN ('pending','queued')`},
		{"df_learning_consolidation_inputs", `UPDATE df_learning_consolidation_inputs SET state='skipped',reason='historical_epoch' WHERE state IN ('pending','queued')`},
		{"df_learning_match_inputs", `UPDATE df_learning_match_inputs SET state='skipped',reason='historical_epoch' WHERE state IN ('pending','queued')`},
		{"df_skill_proposals", `INSERT OR IGNORE INTO df_evolution_retired_proposals SELECT id FROM df_skill_proposals WHERE status NOT IN ('published','rejected','superseded')`},
	}
	for _, statement := range statements {
		var exists bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)`, statement.table).Scan(&exists); err != nil {
			return err
		}
		if exists {
			if _, err = tx.ExecContext(ctx, statement.sql); err != nil {
				return err
			}
		}
	}
	return nil
}

func HistoricalRootTx(ctx context.Context, tx *sql.Tx, root string) (bool, error) {
	var table, historical bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='df_evolution_retired_inputs')`).Scan(&table); err != nil {
		return false, err
	}
	if !table {
		return false, nil
	}
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_evolution_retired_inputs WHERE root_id=?)`, root).Scan(&historical)
	return historical, err
}
