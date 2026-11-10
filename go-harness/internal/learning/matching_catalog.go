package learning

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

var errMatchCatalogProgress = errors.New("catalog metadata scan made progress")

// Metadata pages are committed before any paid matching request. Ranking lives
// in SQLite, so both memory and each scheduler slice remain bounded.
func (s *Store) matchCatalogTx(ctx context.Context, tx *sql.Tx, p patternSnapshot, cwd string) (skills.MatchInventory, string, error) {
	inv := skills.MatchInventory{}
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM df_skill_catalog WHERE id=1`).Scan(&inv.Catalog); err != nil {
		return inv, "", err
	}
	key := encodedDigest([]any{MatchingPolicyVersion, s.matchAccess.Identity(), cwd, inv.Catalog, p})
	var cursor string
	var complete bool
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO df_learning_match_catalog_scan(scan_key,cursor) VALUES(?,'')`, key); err != nil {
		return inv, key, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT cursor,complete FROM df_learning_match_catalog_scan WHERE scan_key=?`, key).Scan(&cursor, &complete); err != nil {
		return inv, key, err
	}
	if complete {
		return inv, key, nil
	}
	page, next, done, err := s.matchAccess.InventoryPageTx(ctx, tx, cwd, cursor)
	if err != nil {
		return inv, key, err
	}
	reads := map[harness.SkillRef]bool{}
	err = walkPatternMembers(ctx, tx, p, func(dep experienceDependency) error {
		var jobID string
		if err = tx.QueryRowContext(ctx, `SELECT job_id FROM df_learning_extractions WHERE id=?`, dep.Extraction).Scan(&jobID); err != nil {
			return err
		}
		job, e := skillmaintenance.ReadJob(ctx, tx, jobID)
		if e != nil {
			return e
		}
		extraction, _, e := s.readExtractionTx(ctx, tx, job)
		if e != nil {
			return e
		}
		for _, ref := range extraction.View.References {
			if ref.Kind == "skill_read" && ref.Skill != nil {
				reads[*ref.Skill] = true
			}
		}
		return nil
	})
	if err != nil {
		return inv, key, err
	}
	query := proceduralTokens(p.Pattern.Candidate)
	for _, entry := range page.Skills {
		choice := matchChoice{Skill: entry, ActualReads: []harness.SkillRef{}}
		for ref := range reads {
			if ref.SourceIdentity == entry.Target.SourceIdentity && ref.Name == entry.Target.Name {
				choice.ActualReads = append(choice.ActualReads, ref)
			}
		}
		sort.Slice(choice.ActualReads, func(i, j int) bool { return choice.ActualReads[i].Version < choice.ActualReads[j].Version })
		score := 0
		for token := range proceduralTokens(harness.SkillExperienceCandidate{Goal: entry.Target.Name, Applicability: entry.Description}) {
			if query[token] {
				score++
			}
		}
		raw, _ := json.Marshal(choice)
		if _, err = tx.ExecContext(ctx, `INSERT INTO df_learning_match_catalog_entries VALUES(?,?,?,?,?,?)`, key, entry.Target.Name, score, len(choice.ActualReads) > 0, raw, digest(raw)); err != nil {
			return inv, key, err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE df_learning_match_catalog_scan SET cursor=?,complete=? WHERE scan_key=?`, next, done, key); err != nil {
		return inv, key, err
	}
	if !done {
		return inv, key, errMatchCatalogProgress
	}
	return inv, key, nil
}
