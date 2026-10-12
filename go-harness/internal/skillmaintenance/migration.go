package skillmaintenance

import (
	"context"
	"database/sql"
	"errors"
)

func migrateJobOwners(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(df_skill_maintenance_jobs)`)
	if err != nil {
		return err
	}
	columns := map[string]bool{}
	for rows.Next() {
		var id, required, primary int
		var name, kind string
		var value any
		if err = rows.Scan(&id, &name, &kind, &required, &value, &primary); err != nil {
			break
		}
		columns[name] = true
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	for _, column := range []struct{ name, definition string }{
		{"job_kind", "TEXT NOT NULL DEFAULT 'legacy'"},
		{"owner_kind", "TEXT NOT NULL DEFAULT 'skill'"},
		{"owner_key", "TEXT NOT NULL DEFAULT ''"},
		{"owner_request_key", "TEXT NOT NULL DEFAULT ''"},
	} {
		if !columns[column.name] {
			if _, err = tx.ExecContext(ctx, `ALTER TABLE df_skill_maintenance_jobs ADD COLUMN `+column.name+` `+column.definition); err != nil {
				return err
			}
		}
	}
	_, err = tx.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS df_skill_maintenance_owner_request
 ON df_skill_maintenance_jobs(job_kind,owner_kind,owner_key,owner_request_key) WHERE owner_kind IN ('workspace','user');
CREATE INDEX IF NOT EXISTS df_skill_maintenance_kind_queue ON df_skill_maintenance_jobs(job_kind,state,retry_ns);`)
	return err
}

// BindLegacyKindTx follows verification of an actual immutable producer input.
// Unresolved legacy jobs stay readable but cannot be claimed or spend quota.
func BindLegacyKindTx(ctx context.Context, tx *sql.Tx, expected Job, kind JobKind) error {
	if expected.Request.Kind != LegacyJob || kind != SkillGeneration && kind != SkillBehavior {
		return ErrInvalid
	}
	fresh, err := ReadJob(ctx, tx, expected.ID)
	if err != nil {
		return err
	}
	if fresh != expected {
		return ErrConflict
	}
	result, err := tx.ExecContext(ctx, `UPDATE df_skill_maintenance_jobs SET job_kind=?,owner_key=? WHERE id=? AND job_kind='legacy'`, kind, expected.Request.OwnerKey, expected.ID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return ErrConflict
	}
	return err
}
