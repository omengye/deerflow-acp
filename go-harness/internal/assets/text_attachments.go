package assets

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
)

func validTextAttachment(name, text string) bool {
	if name == "" || len(name) > 255 || strings.ContainsAny(name, "/\\\x00\r\n") || len(text) > harness.MaxTextAttachmentBytes || !utf8.ValidString(text) {
		return false
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".txt", ".md", ".markdown", ".csv", ".tsv", ".json", ".yaml", ".yml", ".log":
	default:
		return false
	}
	for _, r := range name + text {
		if r < 32 && r != '\n' && r != '\r' && r != '\t' {
			return false
		}
	}
	return true
}

func (s *Store) ListAttachments(ctx context.Context, x harness.Session, cursor string) (harness.AttachmentPage, error) {
	page := harness.AttachmentPage{Attachments: []harness.AssetRef{}}
	if cursor != "" && !generatedID(cursor) {
		return page, invalid("invalid attachment cursor")
	}
	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	if s.closed {
		return page, errors.New("asset store is closed")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT a.metadata,a.workspace,h.cwd FROM harness_assets a JOIN harness_sessions h ON h.id=a.session_id WHERE a.session_id=? AND a.id>? AND json_extract(a.metadata,'$.kind')=? AND EXISTS (SELECT 1 FROM harness_input_assets i WHERE i.asset_id=a.id) ORDER BY a.id LIMIT 33`, x.ID, cursor, harness.AssetFile)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var body []byte
		var workspace, cwd string
		if err = rows.Scan(&body, &workspace, &cwd); err != nil {
			return page, err
		}
		if !session.SameWorkspace(x.CWD, workspace) || !session.SameWorkspace(cwd, workspace) {
			return page, harness.ErrPermissionDenied
		}
		var ref harness.AssetRef
		if err = json.Unmarshal(body, &ref); err != nil {
			return page, err
		}
		if len(page.Attachments) == 32 {
			page.NextCursor = page.Attachments[31].ID
			break
		}
		page.Attachments = append(page.Attachments, ref)
	}
	return page, rows.Err()
}

func (s *Store) ReadAttachment(ctx context.Context, x harness.Session, id string, offset int64, maxBytes int) (harness.ToolOutputChunk, error) {
	var chunk harness.ToolOutputChunk
	if !generatedID(id) || offset < 0 || maxBytes < 0 {
		return chunk, invalid("invalid attachment read")
	}
	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	if s.closed {
		return chunk, errors.New("asset store is closed")
	}
	var metadata []byte
	var workspace, cwd string
	err := s.db.QueryRowContext(ctx, `SELECT a.metadata,a.workspace,h.cwd FROM harness_assets a JOIN harness_sessions h ON h.id=a.session_id WHERE a.id=? AND a.session_id=? AND EXISTS (SELECT 1 FROM harness_input_assets i WHERE i.asset_id=a.id)`, id, x.ID).Scan(&metadata, &workspace, &cwd)
	if errors.Is(err, sql.ErrNoRows) {
		return chunk, harness.ErrNotFound
	}
	if err != nil {
		return chunk, err
	}
	if !session.SameWorkspace(x.CWD, workspace) || !session.SameWorkspace(cwd, workspace) {
		return chunk, harness.ErrPermissionDenied
	}
	var ref harness.AssetRef
	if err = json.Unmarshal(metadata, &ref); err != nil {
		return chunk, err
	}
	if ref.Kind != harness.AssetFile || ref.Size > harness.MaxTextAttachmentBytes {
		return chunk, invalid("only UTF-8 text attachments up to 1 MiB can be read")
	}
	data, err := s.resolve(ctx, x.ID, ref)
	if err != nil {
		return chunk, err
	}
	if !validTextAttachment(ref.Name, string(data)) {
		return chunk, invalid("unsupported attachment; attach UTF-8 TXT, Markdown, CSV, TSV, JSON, YAML or LOG")
	}
	if offset > int64(len(data)) || offset < int64(len(data)) && !utf8.RuneStart(data[offset]) {
		return chunk, invalid("invalid UTF-8 attachment offset")
	}
	if maxBytes == 0 || maxBytes > 4096 {
		maxBytes = 4096
	}
	if maxBytes < utf8.UTFMax {
		maxBytes = utf8.UTFMax
	}
	end := min(int64(len(data)), offset+int64(maxBytes))
	for end > offset && end < int64(len(data)) && !utf8.RuneStart(data[end]) {
		end--
	}
	return harness.ToolOutputChunk{Text: string(data[offset:end]), Offset: offset, NextOffset: end, TotalBytes: int64(len(data)), EOF: end == int64(len(data))}, nil
}
