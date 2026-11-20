package skills

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
	"strings"
)

// RouteVersionTx is shared by snapshots and catalog discovery. A missing root
// is a normal catalog lookup, so it never opts into an observing candidate.
func RouteVersionTx(ctx context.Context, tx *sql.Tx, source, name, current, workspace, root string) (string, error) {
	var state, scope, stable, public, candidate string
	err := tx.QueryRowContext(ctx, `SELECT state,workspace,stable_version,public_version,candidate_version FROM df_skill_routes WHERE source_key=? AND name=?`, source, name).Scan(&state, &scope, &stable, &public, &candidate)
	if errors.Is(err, sql.ErrNoRows) {
		return current, nil
	}
	if err != nil {
		return "", err
	}
	if current != candidate {
		return current, nil
	} // A newer explicit edit wins CAS.
	if scope != "" && !session.SameWorkspace(scope, workspace) {
		return public, nil
	}
	if state == "stable" {
		return candidate, nil
	}
	if state != "observing" {
		return stable, nil
	}
	if root == "" {
		return stable, nil
	}
	var pinned string
	err = tx.QueryRowContext(ctx, `SELECT version FROM df_skill_route_tasks WHERE root_id=? AND source_key=? AND name=?`, root, source, name).Scan(&pinned)
	if err == nil {
		return pinned, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	// Deterministic sampling, bounded to eight concurrent roots per target.
	chosen := stable
	key := digest([]byte(root + "/" + source + "/" + name))
	if strings.ContainsRune("0123", rune(key[0])) {
		var active int
		err = tx.QueryRowContext(ctx, `SELECT count(*) FROM df_skill_route_tasks t WHERE source_key=? AND name=? AND version=? AND EXISTS(SELECT 1 FROM budget_members m JOIN harness_runs r ON r.id=m.id WHERE m.root_id=t.root_id AND r.status NOT IN ('completed','failed','cancelled'))`, source, name, candidate).Scan(&active)
		if err != nil {
			return "", err
		}
		if active < 8 {
			chosen = candidate
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO df_skill_route_tasks VALUES(?,?,?,?)`, root, source, name, chosen)
	return chosen, err
}

// AdmitObservationTx records immutable baseline and workspace authority in the
// same publication transaction. It never authorizes another workspace.
func AdmitObservationTx(ctx context.Context, tx *sql.Tx, target harness.SkillTarget, workspace, base, candidate string) error {
	return admitRouteTx(ctx, tx, target, workspace, base, candidate, "observing")
}

func admitRouteTx(ctx context.Context, tx *sql.Tx, target harness.SkillTarget, workspace, base, candidate, state string) error {
	if workspace == "" {
		workspace = target.Workspace
	}
	if workspace == "" {
		return fmt.Errorf("%w: automatic adoption requires a workspace", harness.ErrPermissionDenied)
	}
	stable := base
	if state == "stable" {
		stable = candidate
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO df_skill_routes(source_key,name,workspace,state,stable_version,public_version,candidate_version) VALUES(?,?,?,?,?,?,?)
 ON CONFLICT(source_key,name) DO UPDATE SET workspace=excluded.workspace,state=excluded.state,public_version=CASE WHEN df_skill_routes.candidate_version=excluded.public_version THEN df_skill_routes.public_version ELSE excluded.public_version END,stable_version=excluded.stable_version,candidate_version=excluded.candidate_version`, target.SourceIdentity, target.Name, workspace, state, stable, base, candidate)
	return err
}
