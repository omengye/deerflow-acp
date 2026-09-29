package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/cloudwego/eino/adk"
)

var _ adk.CheckPointStore = (*Store)(nil)
var _ adk.CheckPointDeleter = (*Store)(nil)

func (s *Store) Get(ctx context.Context, id string) ([]byte, bool, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx, "SELECT payload FROM eino_checkpoints WHERE id=?", id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

func (s *Store) Set(ctx context.Context, id string, data []byte) error {
	if id == "" {
		return errors.New("sqlite: checkpoint id is empty")
	}
	if data == nil {
		data = []byte{}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO eino_checkpoints(id,payload,updated_at) VALUES(?,?,?)
ON CONFLICT(id) DO UPDATE SET payload=excluded.payload,updated_at=excluded.updated_at`,
		id, data, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) Delete(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM eino_checkpoints WHERE id=?", id)
	return err
}
