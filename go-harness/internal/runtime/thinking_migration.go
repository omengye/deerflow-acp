package runtime

import (
	"context"
	"database/sql"
)

func migrateSessionThinking(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(harness_session_configs)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var ordinal, required, primary int
		var name, kind string
		var defaultValue sql.NullString
		if err = rows.Scan(&ordinal, &name, &kind, &required, &defaultValue, &primary); err != nil {
			break
		}
		found = found || name == "thinking_mode"
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil || found {
		return err
	}
	_, err = db.ExecContext(ctx, `ALTER TABLE harness_session_configs ADD COLUMN thinking_mode TEXT NOT NULL DEFAULT ''`)
	return err
}
