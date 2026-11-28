package assets

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

// legacyOutputPath recovers the exact published path from the original call.
// StageArtifacts deduplicates normalized paths and emits artifacts in that order.
func (s *Store) legacyOutputPath(ctx context.Context, cwd string, ref harness.AssetRef) (string, error) {
	var pathsJSON string
	var position int
	err := s.db.QueryRowContext(ctx, `SELECT json_extract(start.event,'$.arguments.paths'),item.key
FROM harness_artifacts a
JOIN harness_events start ON start.run_id=a.run_id AND start.session_id=a.session_id
JOIN harness_events published ON published.run_id=a.run_id AND published.session_id=a.session_id
JOIN json_each(published.event,'$.content') item
WHERE a.asset_id=? AND a.session_id=?
AND json_extract(start.event,'$.kind')='tool_start' AND json_extract(start.event,'$.toolCallId')=a.tool_call_id
AND json_type(start.event,'$.arguments.paths')='array'
AND json_extract(published.event,'$.kind')='artifact_presented' AND json_extract(published.event,'$.toolCallId')=a.tool_call_id
AND json_extract(item.value,'$.asset.id')=a.asset_id`, ref.ID, ref.SessionID).Scan(&pathsJSON, &position)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var paths []string
	if json.Unmarshal([]byte(pathsJSON), &paths) != nil {
		return "", nil
	}
	seen := make(map[string]bool)
	for _, path := range paths {
		rel, err := relativeArtifactPath(cwd, path)
		if err != nil {
			return "", nil
		}
		if seen[rel] {
			continue
		}
		if len(seen) == position {
			if filepath.Base(rel) == ref.Name {
				return filepath.ToSlash(rel), nil
			}
			return "", nil
		}
		seen[rel] = true
	}
	return "", nil
}

// resourceOutputURI is called after verifying the immutable snapshot and its
// owner. The output URI names the current workspace file; a missing or replaced
// output tree leaves the independently usable snapshot URI intact.
func (s *Store) resourceOutputURI(ctx context.Context, ref harness.AssetRef) (string, error) {
	if ref.Kind != harness.AssetArtifact {
		return "", nil
	}
	var cwd, outputPath string
	if err := s.db.QueryRowContext(ctx, `SELECT workspace,COALESCE(json_extract(metadata,'$.outputPath'),'')
FROM harness_assets WHERE id=? AND session_id=?`, ref.ID, ref.SessionID).Scan(&cwd, &outputPath); err != nil {
		return "", err
	}
	if outputPath == "" {
		var err error
		outputPath, err = s.legacyOutputPath(ctx, cwd, ref)
		if err != nil {
			return "", err
		}
	}
	rel := filepath.FromSlash(outputPath)
	if !filepath.IsLocal(rel) || filepath.Base(rel) != ref.Name {
		return "", nil
	}
	root := filepath.Join(cwd, ".deerflow", "outputs")
	path := filepath.Join(root, rel)
	f, err := openWorkspaceFile(root, path)
	if err != nil {
		return "", nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", nil
	}
	return filePathURI(path), nil
}
