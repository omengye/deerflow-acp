package assets

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
)

const maxToolOutputSnapshot = 4 << 20
const maxToolOutputRead = 4096

// StoreToolOutput commits a private snapshot before its reference can enter
// Eino's native session history. The complete output remains available after a
// process restart and is removed with its owning session.
func (s *Store) StoreToolOutput(ctx context.Context, x harness.Session, output string) (ref harness.AssetRef, returnErr error) {
	if !utf8.ValidString(output) || len(output) > maxToolOutputSnapshot {
		return ref, invalid("tool output must be UTF-8 and at most 4 MiB")
	}
	p, err := s.begin(x)
	if err != nil {
		return ref, err
	}
	committed := false
	defer func() { returnErr = errors.Join(returnErr, p.Finish(committed)) }()
	ref, err = p.snapshot(ctx, strings.NewReader(output), "tool-output.txt", "text/plain; charset=utf-8", harness.AssetToolOutput, maxToolOutputSnapshot)
	if err != nil {
		return ref, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ref, err
	}
	defer tx.Rollback()
	if err = p.attachRecords(ctx, tx); err != nil {
		return ref, err
	}
	if err = tx.Commit(); err != nil {
		return ref, err
	}
	committed = true
	return ref, nil
}

// ReadToolOutput resolves an opaque ID only within its saved session and
// workspace, verifies the snapshot digest, and emits one bounded UTF-8 chunk.
func (s *Store) ReadToolOutput(ctx context.Context, x harness.Session, id string, offset int64, requested int) (harness.ToolOutputChunk, error) {
	var chunk harness.ToolOutputChunk
	if id == "" || len(id) > 128 || offset < 0 || requested < 0 {
		return chunk, invalid("invalid tool output read")
	}
	if requested == 0 || requested > maxToolOutputRead {
		requested = maxToolOutputRead
	}
	if requested < utf8.UTFMax {
		requested = utf8.UTFMax
	}
	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	if s.closed {
		return chunk, errors.New("asset store is closed")
	}
	var metadata []byte
	var workspace, currentCWD string
	err := s.db.QueryRowContext(ctx, `SELECT a.metadata,a.workspace,h.cwd FROM harness_assets a JOIN harness_sessions h ON h.id=a.session_id WHERE a.id=? AND a.session_id=?`, id, x.ID).Scan(&metadata, &workspace, &currentCWD)
	if errors.Is(err, sql.ErrNoRows) {
		return chunk, harness.ErrNotFound
	}
	if err != nil {
		return chunk, err
	}
	if !session.SameWorkspace(x.CWD, workspace) || !session.SameWorkspace(currentCWD, workspace) {
		return chunk, harness.ErrPermissionDenied
	}
	var ref harness.AssetRef
	if err = json.Unmarshal(metadata, &ref); err != nil {
		return chunk, err
	}
	if ref.Kind != harness.AssetToolOutput || ref.ID != id || ref.SessionID != x.ID || ref.Size > maxToolOutputSnapshot {
		return chunk, harness.ErrPermissionDenied
	}
	data, err := s.resolve(ctx, x.ID, ref)
	if err != nil {
		return chunk, err
	}
	if !utf8.Valid(data) || offset > int64(len(data)) || (offset < int64(len(data)) && !utf8.RuneStart(data[offset])) {
		return chunk, invalid("invalid tool output offset or encoding")
	}
	end := offset + int64(requested)
	if end > int64(len(data)) {
		end = int64(len(data))
	}
	for end > offset && end < int64(len(data)) && !utf8.RuneStart(data[end]) {
		end--
	}
	chunk = harness.ToolOutputChunk{Text: string(data[offset:end]), Offset: offset, NextOffset: end, TotalBytes: int64(len(data)), EOF: end == int64(len(data))}
	return chunk, nil
}
