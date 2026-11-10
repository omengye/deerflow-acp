package skills

import (
	"context"
	"database/sql"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// A different entry point must not turn an automatic candidate into an
// unrestricted user installation. Automatic publication uses its own scoped
// CAS; ordinary edits remain allowed when they produce independent content.
func guardAutomaticContent(ctx context.Context, tx *sql.Tx, version string) error {
	if version == "" {
		return nil
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_automatic_content WHERE version=?)`, version).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return harness.ErrPermissionDenied
	}
	return nil
}
