package assets

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/session"
)

const artifactDigestBlockSize = 1024 * 1024
const artifactDigestCacheEntries = 64

type artifactReadKey struct {
	id, digest string
	size       int64
}

type artifactReadDigest struct {
	key    artifactReadKey
	blocks [][sha256.Size]byte
}

// The bounded LRU retains only digests (32 bytes per MiB per entry), never
// output bytes. Every hit rehashes the entire requested blocks against a prior
// full SHA256 verification. Neither mtime nor file identity grants trust.
type artifactReadCache struct {
	mu      sync.Mutex
	entries []artifactReadDigest
}

func (c *artifactReadCache) get(key artifactReadKey) ([][sha256.Size]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, entry := range c.entries {
		if entry.key == key {
			copy(c.entries[1:i+1], c.entries[:i])
			c.entries[0] = entry
			return entry.blocks, true
		}
	}
	return nil, false
}

func (c *artifactReadCache) put(key artifactReadKey, blocks [][sha256.Size]byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, entry := range c.entries {
		if entry.key == key {
			copy(c.entries[1:i+1], c.entries[:i])
			c.entries[0] = artifactReadDigest{key, blocks}
			return
		}
	}
	if len(c.entries) < artifactDigestCacheEntries {
		c.entries = append(c.entries, artifactReadDigest{})
	}
	copy(c.entries[1:], c.entries[:len(c.entries)-1])
	c.entries[0] = artifactReadDigest{key, blocks}
}

// ReadArtifactRange binds every returned byte to the saved immutable digest.
// A cache miss verifies the complete file and records 1 MiB block digests;
// subsequent reads verify all complete blocks covering the requested range.
// Tampering outside a cached range is detected when that block is requested.
func (s *Store) ReadArtifactRange(ctx context.Context, sessionID string, ref harness.AssetRef, offset int64, limit int) ([]byte, error) {
	if offset < 0 || limit < 1 || limit > harness.MaxTaskArtifactReadLimit || ref.Size < 0 || ref.Size > s.maxArtifactBytes || offset > ref.Size {
		return nil, invalid("invalid artifact range")
	}
	if sessionID == "" || ref.SessionID != sessionID || ref.Kind != harness.AssetArtifact {
		return nil, harness.ErrPermissionDenied
	}
	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	if s.closed || s.closing.Load() {
		return nil, errors.New("asset store is closed")
	}
	var metadata []byte
	var path, workspace, cwd string
	err := s.db.QueryRowContext(ctx, `SELECT a.metadata,a.path,a.workspace,s.cwd FROM harness_assets a JOIN harness_sessions s ON s.id=a.session_id WHERE a.id=? AND a.session_id=?`, ref.ID, sessionID).Scan(&metadata, &path, &workspace, &cwd)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, harness.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var saved harness.AssetRef
	if json.Unmarshal(metadata, &saved) != nil || saved != ref || !session.SameWorkspace(workspace, cwd) || path != ref.ID+".blob" || filepath.Base(path) != path {
		return nil, harness.ErrPermissionDenied
	}
	f, err := openRead(s.root, path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err = checkArtifactSize(f, ref.Size); err != nil {
		return nil, err
	}
	key := artifactReadKey{ref.ID, ref.SHA256, ref.Size}
	blocks, ok := s.artifactReads.get(key)
	if !ok {
		blocks, err = verifyArtifactBlocks(ctx, f, ref)
		if err != nil {
			return nil, err
		}
		s.artifactReads.put(key, blocks)
	}
	end := min(offset+int64(limit), ref.Size)
	data := make([]byte, 0, end-offset)
	buffer := make([]byte, artifactDigestBlockSize)
	for i := offset / artifactDigestBlockSize; i*artifactDigestBlockSize < end; i++ {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		start := i * artifactDigestBlockSize
		block := buffer[:min(int64(artifactDigestBlockSize), ref.Size-start)]
		if _, err = f.ReadAt(block, start); err != nil {
			return nil, err
		}
		if i >= int64(len(blocks)) || sha256.Sum256(block) != blocks[i] {
			return nil, invalid("asset snapshot digest changed")
		}
		data = append(data, block[max(offset-start, 0):min(end-start, int64(len(block)))]...)
	}
	if err = checkArtifactSize(f, ref.Size); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}

func checkArtifactSize(f *os.File, size int64) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != size {
		return invalid("asset snapshot size changed")
	}
	return nil
}

func verifyArtifactBlocks(ctx context.Context, f *os.File, ref harness.AssetRef) ([][sha256.Size]byte, error) {
	buffer := make([]byte, artifactDigestBlockSize)
	blocks := make([][sha256.Size]byte, 0, (ref.Size+artifactDigestBlockSize-1)/artifactDigestBlockSize)
	hash := sha256.New()
	for remaining := ref.Size; remaining > 0; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		block := buffer[:min(remaining, int64(len(buffer)))]
		if _, err := io.ReadFull(f, block); err != nil {
			return nil, err
		}
		_, _ = hash.Write(block)
		blocks = append(blocks, sha256.Sum256(block))
		remaining -= int64(len(block))
	}
	var tail [1]byte
	if n, err := f.Read(tail[:]); n != 0 || !errors.Is(err, io.EOF) {
		return nil, invalid("asset snapshot size changed")
	}
	if hex.EncodeToString(hash.Sum(nil)) != ref.SHA256 {
		return nil, invalid("asset snapshot digest changed")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return blocks, nil
}
