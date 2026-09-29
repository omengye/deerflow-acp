package assets

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

const outputAlias = "/mnt/user-data/outputs/"

func artifactKey(runID, callID string) string { return runID + "\x00" + callID }

// StageArtifacts returns stable references but does not register/publish them.
// Runtime's successful tool_end commits them with its receipt and audit event.
func (s *Store) StageArtifacts(ctx context.Context, x harness.Session, runID, callID string, paths []string) (contents []harness.Content, returnErr error) {
	if runID == "" || callID == "" || len(paths) == 0 || len(paths) > 32 {
		return nil, invalid("present_files requires a tool call and 1 to 32 output paths")
	}
	var data []byte
	if err := s.db.QueryRowContext(ctx, `SELECT receipt FROM harness_tool_receipts WHERE run_id=? AND tool_call_id=? AND session_id=? AND state='started'`, runID, callID, x.ID).Scan(&data); err != nil {
		return nil, harness.ErrReceiptConflict
	}
	var receipt harness.ToolReceipt
	if json.Unmarshal(data, &receipt) != nil || receipt.ToolName != "present_files" {
		return nil, harness.ErrReceiptConflict
	}
	p, err := s.begin(x)
	if err != nil {
		return nil, err
	}
	defer func() {
		if returnErr != nil {
			returnErr = errors.Join(returnErr, p.Finish(false))
		}
	}()
	outputRoot := filepath.Join(x.CWD, ".deerflow", "outputs")
	seen := map[string]bool{}
	for _, path := range paths {
		if strings.HasPrefix(path, outputAlias) {
			path = filepath.Join(outputRoot, filepath.FromSlash(strings.TrimPrefix(path, outputAlias)))
		} else if !filepath.IsAbs(path) {
			local := filepath.FromSlash(path)
			prefix := filepath.Join(".deerflow", "outputs") + string(filepath.Separator)
			if strings.HasPrefix(local, prefix) {
				path = filepath.Join(x.CWD, local)
			} else {
				path = filepath.Join(outputRoot, local)
			}
		}
		rel, err := filepath.Rel(outputRoot, path)
		if err != nil || !filepath.IsLocal(rel) {
			return nil, invalid("only session output files can be presented")
		}
		if seen[rel] {
			continue
		}
		seen[rel] = true
		// Pin the outputs directory itself: a workspace symlink must not broaden
		// the presentation boundary to arbitrary files elsewhere in the cwd.
		f, err := openWorkspaceFile(outputRoot, path)
		if err != nil {
			return nil, err
		}
		before, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, err
		}
		if !before.Mode().IsRegular() || before.Size() > 200*1024*1024 {
			f.Close()
			return nil, invalid("artifact must be a regular output file at most 200 MiB")
		}
		mediaType := mime.TypeByExtension(filepath.Ext(path))
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		ref, err := p.snapshot(ctx, f, filepath.Base(path), mediaType, harness.AssetArtifact, 200*1024*1024)
		after, statErr := f.Stat()
		f.Close()
		if err != nil {
			return nil, err
		}
		if statErr != nil {
			return nil, statErr
		}
		if before.Size() != after.Size() || before.ModTime() != after.ModTime() || ref.Size != before.Size() {
			return nil, invalid("artifact changed while being snapshotted")
		}
		p.Input = append(p.Input, assetContent(ref, "DeerFlow local artifact"))
	}
	s.mu.Lock()
	if s.closing.Load() {
		s.mu.Unlock()
		return nil, errors.New("asset store is closing")
	}
	key := artifactKey(runID, callID)
	if s.staged[key] != nil {
		s.mu.Unlock()
		return nil, harness.ErrReceiptConflict
	}
	s.staged[key] = p
	s.mu.Unlock()
	return append([]harness.Content(nil), p.Input...), nil
}

// TakeArtifacts transfers cleanup ownership to the terminal event transaction.
func (s *Store) TakeArtifacts(sessionID, runID, callID string) *Prepared {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := artifactKey(runID, callID)
	p := s.staged[key]
	if p == nil || p.session.ID != sessionID {
		return nil
	}
	delete(s.staged, key)
	return p
}

func (p *Prepared) AttachArtifacts(ctx context.Context, tx *sql.Tx, runID, callID string) error {
	if err := p.attachRecords(ctx, tx); err != nil {
		return err
	}
	for _, ref := range p.refs {
		if ref.Kind != harness.AssetArtifact {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO harness_artifacts(session_id,run_id,tool_call_id,asset_id) VALUES(?,?,?,?)`, p.session.ID, runID, callID, ref.ID); err != nil {
			return err
		}
	}
	return nil
}

// ToolImages reports that the prepared snapshots came from an inline tool
// result. Its text and ordering are already represented in the event.
func (p *Prepared) ToolImages() bool { return p.toolImages }

// ValidateToolContent prevents a staged snapshot from being committed under a
// terminal event that omits or changes the reference returned to Eino.
func (p *Prepared) ValidateToolContent(content []harness.Content) error {
	if !p.toolImages {
		return nil
	}
	seen := make(map[string]bool, len(p.refs))
	for _, c := range content {
		if c.Asset == nil {
			continue
		}
		for _, ref := range p.refs {
			if c.Type == "image" && c.Data == "" && *c.Asset == ref && c.URI == harness.AssetURI(ref) && c.MimeType == ref.MimeType {
				if seen[ref.ID] {
					return invalid("duplicate staged tool image")
				}
				seen[ref.ID] = true
			}
		}
	}
	if len(seen) != len(p.refs) {
		return invalid("terminal tool result lost a staged image")
	}
	return nil
}

// StageToolImages imports bounded inline images produced by an enhanced tool.
// The terminal tool_end transaction owns publication and rollback.
func (s *Store) StageToolImages(ctx context.Context, x harness.Session, runID, callID string, images []harness.Content) (contents []harness.Content, returnErr error) {
	if runID == "" || callID == "" || len(images) == 0 || len(images) > harness.MaxInputImagesPerTurn {
		return nil, invalid("tool image batch must contain 1 to 8 images")
	}
	var receiptData []byte
	if err := s.db.QueryRowContext(ctx, `SELECT receipt FROM harness_tool_receipts WHERE run_id=? AND tool_call_id=? AND session_id=? AND state='started'`, runID, callID, x.ID).Scan(&receiptData); err != nil {
		return nil, harness.ErrReceiptConflict
	}
	var receipt harness.ToolReceipt
	if json.Unmarshal(receiptData, &receipt) != nil {
		return nil, harness.ErrReceiptConflict
	}
	p, err := s.begin(x)
	if err != nil {
		return nil, err
	}
	p.toolImages = true
	defer func() {
		if returnErr != nil {
			returnErr = errors.Join(returnErr, p.Finish(false))
		}
	}()
	var total int64
	for i, c := range images {
		if c.Type != "image" || c.Asset != nil || c.URI != "" || c.Text != "" {
			return nil, invalid("tool image must contain only inline data")
		}
		data, mediaType, err := decodeImage(c.Data, c.MimeType)
		if err != nil {
			return nil, err
		}
		total += int64(len(data))
		if total > harness.MaxInputImageTotalBytes {
			return nil, invalid("tool image batch exceeds 40 MiB")
		}
		name := safeName(c.Name, "tool-image-"+strconv.Itoa(i+1)+imageExtension(mediaType))
		ref, err := p.snapshot(ctx, bytes.NewReader(data), name, mediaType, harness.AssetImage, harness.MaxInputImageBytes)
		if err != nil {
			return nil, err
		}
		p.Input = append(p.Input, assetContent(ref, "Tool image"))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := artifactKey(runID, callID)
	if s.closing.Load() || s.staged[key] != nil {
		return nil, harness.ErrReceiptConflict
	}
	s.staged[key] = p
	return append([]harness.Content(nil), p.Input...), nil
}

// StageImage snapshots a workspace image for a view_image tool call. Its bytes
// become resolvable only when runtime commits the successful terminal receipt.
func (s *Store) StageImage(ctx context.Context, x harness.Session, runID, callID, path string) (content harness.Content, returnErr error) {
	var data []byte
	if err := s.db.QueryRowContext(ctx, `SELECT receipt FROM harness_tool_receipts WHERE run_id=? AND tool_call_id=? AND session_id=? AND state='started'`, runID, callID, x.ID).Scan(&data); err != nil {
		return content, harness.ErrReceiptConflict
	}
	var receipt harness.ToolReceipt
	if json.Unmarshal(data, &receipt) != nil || receipt.ToolName != "view_image" {
		return content, harness.ErrReceiptConflict
	}
	p, err := s.begin(x)
	if err != nil {
		return content, err
	}
	defer func() {
		if returnErr != nil {
			returnErr = errors.Join(returnErr, p.Finish(false))
		}
	}()
	if strings.HasPrefix(path, outputAlias) {
		path = filepath.Join(x.CWD, ".deerflow", "outputs", filepath.FromSlash(strings.TrimPrefix(path, outputAlias)))
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(x.CWD, path)
	}
	f, err := openWorkspaceFile(x.CWD, path)
	if err != nil {
		return content, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return content, err
	}
	if !before.Mode().IsRegular() || before.Size() > harness.MaxInputImageBytes {
		return content, invalid("image must be a regular workspace file at most 20 MiB")
	}
	data, err = io.ReadAll(&contextReader{ctx: ctx, reader: io.LimitReader(f, harness.MaxInputImageBytes+1)})
	if err != nil {
		return content, err
	}
	after, err := f.Stat()
	if err != nil {
		return content, err
	}
	if before.Size() != after.Size() || before.ModTime() != after.ModTime() || int64(len(data)) != before.Size() {
		return content, invalid("image changed while being snapshotted")
	}
	mediaType, err := validateImage(data, "")
	if err != nil {
		return content, err
	}
	ref, err := p.snapshot(ctx, bytes.NewReader(data), filepath.Base(path), mediaType, harness.AssetImage, harness.MaxInputImageBytes)
	if err != nil {
		return content, err
	}
	content = assetContent(ref, "Workspace image")
	p.Input = []harness.Content{content}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := artifactKey(runID, callID)
	if s.closing.Load() || s.staged[key] != nil {
		return harness.Content{}, harness.ErrReceiptConflict
	}
	s.staged[key] = p
	return content, nil
}

func (s *Store) AbortRun(runID string) error {
	s.mu.Lock()
	var pending []*Prepared
	for key, p := range s.staged {
		if strings.HasPrefix(key, runID+"\x00") {
			pending = append(pending, p)
			delete(s.staged, key)
		}
	}
	s.mu.Unlock()
	var err error
	for _, p := range pending {
		err = errors.Join(err, p.Finish(false))
	}
	return err
}

func (s *Store) ListArtifacts(ctx context.Context, sessionID string) ([]harness.Content, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.metadata FROM harness_artifacts r JOIN harness_assets a ON a.id=r.asset_id WHERE r.session_id=? ORDER BY r.rowid`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]harness.Content, 0)
	for rows.Next() {
		var data []byte
		var ref harness.AssetRef
		if err = rows.Scan(&data); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(data, &ref); err != nil {
			return nil, err
		}
		result = append(result, assetContent(ref, "DeerFlow local artifact"))
	}
	return result, rows.Err()
}
