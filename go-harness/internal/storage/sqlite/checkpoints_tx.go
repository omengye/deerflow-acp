package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

func (s *Store) SetCheckpointTx(ctx context.Context, tx *sql.Tx, id string, data []byte) error {
	if id == "" {
		return errors.New("sqlite: checkpoint id is empty")
	}
	if data == nil {
		data = []byte{}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO eino_checkpoints(id,payload,updated_at) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET payload=excluded.payload,updated_at=excluded.updated_at`, id, data, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) DeleteCheckpointTx(ctx context.Context, tx *sql.Tx, id string) error {
	_, err := tx.ExecContext(ctx, "DELETE FROM eino_checkpoints WHERE id=?", id)
	return err
}
