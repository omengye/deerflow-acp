package evolution

import (
	"context"
	"database/sql"
	"errors"

	"github.com/omengye/deerflow-acp/go-harness/harness"
)

func discoveryEpisodeTx(ctx context.Context, tx *sql.Tx, root string) (string, error) {
	var pinned string
	err := tx.QueryRowContext(ctx, `SELECT episode_key FROM df_skill_discovery_episode_roots WHERE root_id=?`, root).Scan(&pinned)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if pinned != "" && !hexDigest(pinned) {
		return "", errDiscoveryEvidence
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='harness_learning_inputs')`).Scan(&exists); err != nil {
		return "", err
	}
	if exists {
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT l.independence_key FROM harness_learning_inputs i
 JOIN df_learning_episode_lineage l ON l.episode_id=i.episode_id WHERE i.root_id=? LIMIT 2`, root)
		if err != nil {
			return "", err
		}
		var keys []string
		for rows.Next() {
			var key string
			if err = rows.Scan(&key); err != nil {
				break
			}
			keys = append(keys, key)
		}
		if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
			return "", err
		}
		if len(keys) > 1 || len(keys) == 1 && !hexDigest(keys[0]) {
			return "", errDiscoveryEvidence
		}
		if len(keys) == 1 {
			if pinned != "" && pinned != keys[0] {
				return "", errDiscoveryEvidence
			}
			if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO df_skill_discovery_episode_roots(root_id,episode_key) VALUES(?,?)`, root, keys[0]); err != nil {
				return "", err
			}
			return keys[0], nil
		}
	}
	if pinned != "" {
		return pinned, nil
	}
	// Hosts without accepted learning input retain conservative root identity;
	// models and feedback summaries cannot invent a new independent episode.
	key := encodedHash([]string{"observed-root-episode-v1", root})
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO df_skill_discovery_episode_roots(root_id,episode_key) VALUES(?,?)`, root, key)
	return key, err
}

func discoveryEpisodeDigest(key string, target harness.SkillTarget, version string) string {
	return encodedHash([]string{"discovery-episode-target-version-v1", key, target.SourceIdentity, target.Name, version})
}

func discoveryEpisodeConsumedTx(ctx context.Context, tx *sql.Tx, root, key string, target harness.SkillTarget, version string) (bool, error) {
	var consumed bool
	// Retain legacy trigger-specific reservations during upgrade. New work
	// always writes the unified key as well, independent of trigger kind.
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_discovery_episode_consumed WHERE digest=?)
 OR EXISTS(SELECT 1 FROM df_skill_discovery_consumed WHERE root_digest IN (?,?,?))`,
		discoveryEpisodeDigest(key, target, version), discoveryRootKey(root, target, version), derivedRootKey("missing_skill", root, target), derivedRootKey("repeated_workflow", root, target)).Scan(&consumed)
	if err != nil || consumed {
		return consumed, err
	}
	var learning bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='harness_learning_inputs')`).Scan(&learning); err != nil || !learning {
		return false, err
	}
	// A legacy choice may have spent another root in this same episode. Upgrade
	// must not turn that alias into a fresh vote just because no unified row exists.
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_discoveries d
 JOIN df_skill_discovery_consumed c ON c.discovery_id=d.id
 JOIN json_each(CAST(d.payload AS TEXT),'$.evidence') ref
 LEFT JOIN df_skill_discovery_episode_roots pin ON pin.root_id=json_extract(ref.value,'$.root_id')
 LEFT JOIN harness_learning_inputs input ON input.root_id=json_extract(ref.value,'$.root_id')
 LEFT JOIN df_learning_episode_lineage lineage ON lineage.episode_id=input.episode_id
 WHERE d.source_key=? AND d.name=? AND json_extract(CAST(d.payload AS TEXT),'$.base.version')=?
 AND COALESCE(json_extract(CAST(d.payload AS TEXT),'$.episode_dedup'),0)=0
 AND (pin.episode_key=? OR lineage.independence_key=?))`, target.SourceIdentity, target.Name, version, key, key).Scan(&consumed)
	return consumed, err
}

func reserveDiscoveryEpisodesTx(ctx context.Context, tx *sql.Tx, d discoverySnapshot) error {
	for _, ref := range d.Evidence {
		if !hexDigest(ref.EpisodeKey) {
			return errDiscoveryEvidence
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO df_skill_discovery_episode_consumed(digest,discovery_id) VALUES(?,?)`, discoveryEpisodeDigest(ref.EpisodeKey, d.Target, d.Base.Version), d.ID); err != nil {
			return err
		}
	}
	return nil
}

func verifyDiscoveryEpisodesTx(ctx context.Context, tx *sql.Tx, d discoverySnapshot) error {
	reservation := d.ID
	if d.CreationRetry != nil {
		reservation = d.CreationRetry.EvidenceReservationID
	}
	seen := map[string]bool{}
	for _, ref := range d.Evidence {
		if !hexDigest(ref.EpisodeKey) || seen[ref.EpisodeKey] {
			return errDiscoveryEvidence
		}
		seen[ref.EpisodeKey] = true
		var id string
		if err := tx.QueryRowContext(ctx, `SELECT discovery_id FROM df_skill_discovery_episode_consumed WHERE digest=?`, discoveryEpisodeDigest(ref.EpisodeKey, d.Target, d.Base.Version)).Scan(&id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errDiscoveryEvidence
			}
			return err
		}
		if id != reservation {
			return errDiscoveryEvidence
		}
		if d.kind() == "experience_pattern" {
			key, err := discoveryEpisodeTx(ctx, tx, ref.RootID)
			if err != nil {
				return err
			}
			if key != ref.EpisodeKey {
				return errDiscoveryEvidence
			}
		}
	}
	return nil
}
