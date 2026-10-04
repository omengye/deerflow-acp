package deerflow

import (
	"context"
	"errors"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// restoreHostResources runs before workers start. Host config is the only
// source of connection intent; persisted rows contain identity and grants only.
// Renewing transport authority does not modify native task state, receipts,
// saved execution contracts, continuation eligibility or reconciliation fences.
func (h *backgroundHost) restoreHostResources(ctx context.Context) error {
	if h.mcp == nil {
		return nil
	}
	cursor := ""
	for {
		rows, err := h.store.DB().QueryContext(ctx, `SELECT task_id FROM harness_background_resources WHERE task_id>? AND state='authorized' AND COALESCE(json_extract(ref,'$.hostRecipeId'),'')<>'' ORDER BY task_id LIMIT 128`, cursor)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				break
			}
			ids = append(ids, id)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		for _, id := range ids {
			if err = ctx.Err(); err != nil {
				return err
			}
			r, err := readBackgroundResource(ctx, h.store.DB(), id)
			if err != nil {
				return err
			}
			if h.mcp.HostRecipeStatus(r.ref) != "restorable" {
				continue
			}
			session, err := h.runtime.Store.Session(ctx, r.ref.SessionID)
			if errors.Is(err, harness.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			generation, err := h.mcp.RestoreHost(ctx, r.ref, session.CWD)
			if err != nil {
				// An unavailable or changed external service cannot prevent the
				// operator opening the desktop to inspect waiting tasks.
				if ctx.Err() != nil {
					return ctx.Err()
				}
				continue
			}
			if generation == r.generation {
				continue
			}
			if _, err = h.store.DB().ExecContext(ctx, `UPDATE harness_background_resources SET version=version+1,generation=? WHERE task_id=? AND version=? AND ref_sha=? AND state='authorized'`, generation, id, r.version, resourceHash(r.ref)); err != nil {
				return err
			}
		}
		cursor = ids[len(ids)-1]
	}
}
