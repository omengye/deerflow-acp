package assets

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// PrepareFork copies only immutable snapshots referenced by the retained
// history. The new session owns the copies independently of the source.
func (s *Store) PrepareFork(ctx context.Context, source string, target harness.Session, history [][]byte) (*Prepared, map[string]harness.AssetRef, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT metadata FROM harness_assets WHERE session_id=?`, source)
	if err != nil {
		return nil, nil, err
	}
	var refs []storedAsset
	for rows.Next() {
		var data []byte
		if err = rows.Scan(&data); err != nil {
			break
		}
		var ref storedAsset
		if err = json.Unmarshal(data, &ref); err != nil {
			break
		}
		for _, payload := range history {
			if bytes.Contains(payload, []byte(ref.ID)) {
				refs = append(refs, ref)
				break
			}
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	p, err := s.begin(target)
	if err != nil {
		return nil, nil, err
	}
	mapping := make(map[string]harness.AssetRef)
	p.forkReferences = make(map[string]string)
	for _, stored := range refs {
		ref := stored.AssetRef
		data, e := s.resolve(ctx, source, ref)
		if e != nil {
			_ = p.Finish(false)
			return nil, nil, e
		}
		copy, e := p.snapshot(ctx, bytes.NewReader(data), ref.Name, ref.MimeType, ref.Kind, ref.Size)
		if e != nil {
			_ = p.Finish(false)
			return nil, nil, e
		}
		if copy.SHA256 != ref.SHA256 {
			_ = p.Finish(false)
			return nil, nil, fmt.Errorf("fork asset digest mismatch")
		}
		if ref.Kind == harness.AssetArtifact {
			outputPath := stored.OutputPath
			if outputPath == "" {
				outputPath, e = s.legacyOutputPath(ctx, target.CWD, ref)
				if e != nil {
					_ = p.Finish(false)
					return nil, nil, e
				}
			}
			p.records[len(p.records)-1].OutputPath = outputPath
		}
		mapping[ref.ID] = copy
		// Both identities come from digest-verified source metadata and a new
		// host-generated snapshot, never from client-supplied paths.
		p.forkReferences[harness.AssetURI(ref)] = harness.AssetURI(copy)
		p.forkReferences[s.localURI(ref)] = s.localURI(copy)
		p.forkReferences[filepath.Join(s.path, ref.ID+".blob")] = filepath.Join(s.path, copy.ID+".blob")
	}
	return p, mapping, nil
}

// AttachFork writes ownership without creating executable input or artifact
// intents. Historical references remain readable through their new owner.
func (p *Prepared) AttachFork(ctx context.Context, tx *sql.Tx) error {
	if err := p.attachRecords(ctx, tx); err != nil {
		return err
	}
	for _, ref := range p.refs {
		if ref.Kind == harness.AssetArtifact {
			if _, err := tx.ExecContext(ctx, `INSERT INTO harness_history_artifacts(session_id,asset_id) VALUES(?,?)`, p.session.ID, ref.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// ForkReferences lets the client remap retained local transcript links to the
// copied snapshots. It grants no execution or task artifact ownership.
func (p *Prepared) ForkReferences() map[string]string { return maps.Clone(p.forkReferences) }
