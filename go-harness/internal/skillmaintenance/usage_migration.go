package skillmaintenance

import (
	"context"
	"database/sql"
	"errors"
)

// Old conservative reservations were never actual usage. Rename the audit
// fields once and recompute totals exclusively from known provider receipts.
func migrateUsageLedger(ctx context.Context, tx *sql.Tx) error {
	for _, table := range []string{"df_skill_maintenance_calls", "df_skill_maintenance_days"} {
		rows, err := tx.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
		if err != nil {
			return err
		}
		columns := map[string]bool{}
		for rows.Next() {
			var ordinal, required, key int
			var name, kind string
			var value any
			if err = rows.Scan(&ordinal, &name, &kind, &required, &value, &key); err != nil {
				break
			}
			columns[name] = true
		}
		if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, rename := range [][2]string{{"reserved_tokens", "estimated_tokens"}, {"charged_tokens", "actual_tokens"}} {
			if columns[rename[0]] {
				if _, err = tx.ExecContext(ctx, `ALTER TABLE `+table+` RENAME COLUMN `+rename[0]+` TO `+rename[1]); err != nil {
					return err
				}
			}
		}
		if table == "df_skill_maintenance_calls" && columns["charged_tokens"] {
			if _, err = tx.ExecContext(ctx, `UPDATE df_skill_maintenance_calls SET actual_tokens=0 WHERE estimated=1`); err != nil {
				return err
			}
		}
		if table == "df_skill_maintenance_days" && columns["charged_tokens"] {
			if _, err = tx.ExecContext(ctx, `UPDATE df_skill_maintenance_days SET actual_tokens=COALESCE((SELECT SUM(c.actual_tokens) FROM df_skill_maintenance_calls c WHERE c.day=df_skill_maintenance_days.day),0)`); err != nil {
				return err
			}
		}
	}
	return nil
}
