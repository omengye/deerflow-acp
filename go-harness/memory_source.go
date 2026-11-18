package deerflow

import (
	"context"
	"database/sql"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/memory"
)

func memoryInputCapture(store *memory.Store, cfg Config) func(context.Context, *sql.Tx, harness.RunRequest, int64, []byte) error {
	return func(ctx context.Context, tx *sql.Tx, req harness.RunRequest, sequence int64, raw []byte) error {
		enabled := cfg.MemoryEnabled == nil || *cfg.MemoryEnabled
		writable := enabled && req.Session.Mode != "plan" && req.Session.ApprovalMode != harness.ApprovalReadOnly
		var kinds []harness.MemoryScope
		if cfg.MemorySingleUser {
			kinds = []harness.MemoryScope{harness.MemoryGlobal, harness.MemoryWorkspace}
		} else {
			switch cfg.MemoryScope {
			case harness.MemorySession:
				kinds = []harness.MemoryScope{harness.MemorySession}
			case harness.MemoryWorkspace, harness.MemoryGlobal:
				kinds = []harness.MemoryScope{harness.MemoryWorkspace}
			default:
				kinds = []harness.MemoryScope{harness.MemoryWorkspace}
				if cfg.MemoryUserID != "" {
					kinds = append(kinds, harness.MemoryUser)
				}
			}
		}
		var scopes []memory.Scope
		for _, kind := range kinds {
			scope, err := memoryRunScope(cfg, req, kind)
			if err != nil {
				return err
			}
			scopes = append(scopes, scope)
		}
		return store.CaptureInputTx(ctx, tx, req, sequence, raw, cfg.memoryGlobalID, scopes, writable, cfg.MemoryExtraction)
	}
}
