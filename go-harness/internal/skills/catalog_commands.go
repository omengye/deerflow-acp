package skills

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func validManagementText(value string, max int) bool {
	return value != "" && len(value) <= max && utf8.ValidString(value) && strings.TrimSpace(value) == value && strings.IndexFunc(value, unicode.IsControl) < 0
}

func validManagementHead(h harness.SkillHead) bool {
	return h.StateVersion > 0 && h.Exists && validHash(h.Version)
}

// SetEnabledChecked makes operator enablement durable against startup imports.
// It does not create an overlay or grant permission to publish new content.
func (r *Registry) SetEnabledChecked(ctx context.Context, request harness.SkillEnablement) (harness.SkillManagementResult, error) {
	if !validManagementHead(request.Expected) {
		return harness.SkillManagementResult{}, invalid("enablement requires an existing expected head")
	}
	return r.catalogCommand(ctx, request.Target, request.RequestKey, request.Actor, "set_enabled", request, func(p *PublicationTx) (SkillHead, error) {
		h, err := p.Current()
		if err != nil {
			return h, err
		}
		if h != request.Expected {
			return h, harness.ErrSkillConflict
		}
		if !h.Exists {
			return h, harness.ErrNotFound
		}
		if h.Managed && h.Enabled == request.Enabled {
			return h, nil
		}
		return p.moveHead(h, h.Version, request.Enabled, true, "", "")
	})
}

// RetryExportChecked queues one exact observed export state. A retry of the
// same request key never resets attempts again, even after exporter progress.
func (r *Registry) RetryExportChecked(ctx context.Context, request harness.SkillExportRetry) (harness.SkillManagementResult, error) {
	if request.Expected.StateVersion < 1 || !request.Expected.Managed || !validHash(request.ExpectedToken) {
		return harness.SkillManagementResult{}, invalid("retry requires a managed head and export state token")
	}
	return r.catalogCommand(ctx, request.Target, request.RequestKey, request.Actor, "retry_export", request, func(p *PublicationTx) (SkillHead, error) {
		h, err := p.Current()
		if err != nil {
			return h, err
		}
		if h != request.Expected {
			return h, harness.ErrSkillConflict
		}
		if p.export == nil {
			return h, invalid("no export root is configured")
		}
		item, err := readExport(p.tx.QueryRowContext(ctx, `SELECT `+exportColumns+` FROM df_skill_exports WHERE source_key=? AND name=? AND export_key=?`, p.source.identity, p.name, p.export.key), p.source, p.export.key)
		if errors.Is(err, sql.ErrNoRows) {
			return h, harness.ErrNotFound
		}
		if err != nil {
			return h, err
		}
		if item.state.Head != h || item.state.StateToken != request.ExpectedToken {
			return h, harness.ErrSkillConflict
		}
		now := time.Now().UTC()
		if !now.After(item.state.UpdatedAt) {
			now = item.state.UpdatedAt.Add(time.Nanosecond)
		}
		_, err = p.tx.ExecContext(ctx, `UPDATE df_skill_exports SET status='pending',attempts=0,error_code='',retry_after_ns=0,updated_at=? WHERE source_key=? AND name=? AND export_key=?`, now.Format(time.RFC3339Nano), p.source.identity, p.name, p.export.key)
		return h, err
	})
}

// The audit row and mutation commit atomically. Replays return the original
// applied head and a fresh projection, even if newer commands have since won.
func (r *Registry) catalogCommand(ctx context.Context, target harness.SkillTarget, key, actor, operation string, request any, apply func(*PublicationTx) (SkillHead, error)) (harness.SkillManagementResult, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result harness.SkillManagementResult
	if r.closed {
		return result, ErrClosed
	}
	if !validManagementText(key, 256) || !validManagementText(actor, 128) {
		return result, invalid("management request key and actor are required")
	}
	s, err := r.catalogTarget(target)
	if err != nil {
		return result, err
	}
	data, err := json.Marshal(request)
	if err != nil {
		return result, err
	}
	want := digest(data)
	var expectedApplied SkillHead
	switch command := request.(type) {
	case harness.SkillEnablement:
		expectedApplied = command.Expected
		if !expectedApplied.Managed || expectedApplied.Enabled != command.Enabled {
			expectedApplied.StateVersion++
			expectedApplied.Managed, expectedApplied.Enabled = true, command.Enabled
		}
	case harness.SkillExportRetry:
		expectedApplied = command.Expected
	default:
		return result, invalid("unsupported skill management command")
	}
	err = r.withPublication(ctx, s, target.Name, func(p *PublicationTx) error {
		var savedHash, savedOperation, savedIntent, applied string
		err := p.tx.QueryRowContext(ctx, `SELECT operation,intent_hash,intent,applied_head FROM df_skill_management_requests WHERE source_key=? AND name=? AND request_key=?`, s.identity, target.Name, key).Scan(&savedOperation, &savedHash, &savedIntent, &applied)
		switch {
		case err == nil:
			if !validHash(savedHash) || digest([]byte(savedIntent)) != savedHash || json.Unmarshal([]byte(applied), &result.AppliedHead) != nil || result.AppliedHead.StateVersion < 1 || !result.AppliedHead.Managed {
				return errors.New("skill management audit record is inconsistent")
			}
			if savedHash != want || savedOperation != operation {
				return harness.ErrSkillConflict
			}
			if result.AppliedHead != expectedApplied {
				return errors.New("skill management applied head is inconsistent")
			}
			result.Replayed = true
		case errors.Is(err, sql.ErrNoRows):
			if result.AppliedHead, err = apply(p); err != nil {
				return err
			}
			headData, _ := json.Marshal(result.AppliedHead)
			if _, err = p.tx.ExecContext(ctx, `INSERT INTO df_skill_management_requests(source_key,name,request_key,operation,intent_hash,intent,applied_head,created_at) VALUES(?,?,?,?,?,?,?,?)`, s.identity, target.Name, key, operation, want, string(data), string(headData), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				return err
			}
		default:
			return err
		}
		if result.Skill, err = p.catalogEntry(); err != nil {
			return err
		}
		result.CatalogVersion, err = p.CatalogVersion()
		return err
	})
	return result, err
}

// RefreshCatalogImport indexes the bound directory under the observed head.
// It never republishes a historical database version into that directory.
func (r *Registry) RefreshCatalogImport(ctx context.Context, target harness.SkillTarget, expected harness.SkillHead) (harness.SkillCatalogDetail, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result harness.SkillCatalogDetail
	if r.closed {
		return result, ErrClosed
	}
	s, err := r.catalogTarget(target)
	if err != nil {
		return result, err
	}
	origin := importOrigin{name: target.Name}
	err = r.withPublication(ctx, s, target.Name, func(p *PublicationTx) error {
		h, err := p.Current()
		if err != nil {
			return err
		}
		if h != expected {
			return harness.ErrSkillConflict
		}
		if err = p.tx.QueryRowContext(ctx, `SELECT last_import_version,import_directory FROM df_skill_control WHERE source_key=? AND name=?`, s.identity, target.Name).Scan(&origin.version, &origin.directory); err != nil {
			return err
		}
		return r.refreshSource(p, origin.directory, nil)
	})
	if err != nil {
		return result, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	p := r.catalogTx(ctx, tx, s, target.Name)
	if result.Skill, err = p.catalogEntry(); err != nil {
		return result, err
	}
	if result.CatalogVersion, err = p.CatalogVersion(); err != nil {
		return result, err
	}
	return result, tx.Commit()
}
