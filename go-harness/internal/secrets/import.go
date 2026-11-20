package secrets

import (
	"context"
	"database/sql"
	"errors"
)

var ErrImportConflict = errors.New("secret changed during import; retry with a fresh plan")

// ImportWriter keeps names stable between planning and publication. Metadata
// readers continue to work, including search readiness callbacks during save.
type ImportWriter struct{ v *Vault }

func (v *Vault) BeginImport() (*ImportWriter, func()) {
	v.writes.Lock()
	return &ImportWriter{v}, v.writes.Unlock
}

// Store commits the entire create-only batch and checks all reused versions.
// A failure leaves no partially created account pair or imported batch.
func (w *ImportWriter) Store(ctx context.Context, create []SetRequest, reuse map[string]Metadata) error {
	v := w.v
	if err := v.usable(); err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	tx, err := v.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for name, expected := range reuse {
		var version int64
		var fingerprint, keyID, kind string
		err = tx.QueryRowContext(ctx, `SELECT s.current_version,x.fingerprint,x.key_id,x.kind FROM harness_secrets s JOIN harness_secret_versions x ON x.name=s.name AND x.version=s.current_version WHERE s.name=? AND s.deleted_at=''`, name).Scan(&version, &fingerprint, &keyID, &kind)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrImportConflict
		}
		if err != nil {
			return err
		}
		if version != expected.Version || fingerprint != expected.Fingerprint || keyID != v.keyID || kind != string(KindString) {
			return ErrImportConflict
		}
	}
	for _, req := range create {
		if IsCredentialName(req.Name) {
			return ErrInvalid
		}
		if err = validateSetRequest(req); err != nil {
			return err
		}
		if err = v.writeVersion(ctx, tx, req, true); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if len(create) > 0 {
		return v.rebuild(context.WithoutCancel(ctx))
	}
	return nil
}
