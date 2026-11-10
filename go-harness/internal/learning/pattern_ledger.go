package learning

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/omengye/deerflow-acp/go-harness/harness"
	"hash"
	"slices"
	"sort"
)

// Revisions contain bounded representative samples. Membership itself is an
// append-only ledger: an experience is stored once with its first revision.
// Every source, conflict and independent vote is checked against that ledger,
// including evidence which is absent from the representative sample.
func compactPattern(snap patternSnapshot) patternSnapshot {
	if snap.LedgerDigest != "" {
		return snap
	}
	snap.LedgerDigest = encodedDigest(snap.Members)
	snap.Members = snap.Members[:min(len(snap.Members), maxPatternMembers)]
	snap.Pattern.ExperienceIDs = snap.Pattern.ExperienceIDs[:min(len(snap.Pattern.ExperienceIDs), maxPatternMembers)]
	snap.Pattern.EpisodeIDs = snap.Pattern.EpisodeIDs[:min(len(snap.Pattern.EpisodeIDs), maxPatternMembers)]
	return snap
}

func expandPatternLedger(ctx context.Context, tx *sql.Tx, snap *patternSnapshot) error {
	if snap.LedgerDigest == "" {
		return nil
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM df_learning_pattern_members WHERE pattern_id=? AND revision<=?`, snap.Pattern.ID, snap.Pattern.Revision).Scan(&count); err != nil {
		return err
	}
	if count > maxPatternMembers {
		return nil
	} // Paged verification retains the bounded snapshot.
	rows, err := tx.QueryContext(ctx, `SELECT m.experience_id,m.experience_sha,e.extraction_id,x.payload_sha,x.result_sha,e.session_id,m.episode_id,m.independence_key,m.epoch
 FROM df_learning_pattern_members m JOIN df_learning_experiences e ON e.id=m.experience_id JOIN df_learning_extractions x ON x.id=e.extraction_id
 WHERE m.pattern_id=? AND m.revision<=? ORDER BY m.experience_id`, snap.Pattern.ID, snap.Pattern.Revision)
	if err != nil {
		return err
	}
	var deps []experienceDependency
	for rows.Next() {
		var dep experienceDependency
		if err = rows.Scan(&dep.ID, &dep.SHA, &dep.Extraction, &dep.SourceSHA, &dep.ResultSHA, &dep.Session, &dep.Episode, &dep.Independent, &dep.Epoch); err != nil {
			break
		}
		deps = append(deps, dep)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return err
	}
	if encodedDigest(deps) != snap.LedgerDigest {
		return invalid("pattern evidence ledger changed")
	}
	sample := compactPattern(patternSnapshot{Members: deps})
	if encodedDigest(sample.Members) != encodedDigest(snap.Members) {
		return invalid("pattern representative sample changed")
	}
	snap.Members = deps
	snap.LedgerDigest = ""
	snap.Pattern.ExperienceIDs = []string{}
	snap.Pattern.EpisodeIDs = []string{}
	episodes := map[string]bool{}
	for _, dep := range deps {
		snap.Pattern.ExperienceIDs = append(snap.Pattern.ExperienceIDs, dep.ID)
		episodes[dep.Episode] = true
	}
	for episode := range episodes {
		snap.Pattern.EpisodeIDs = append(snap.Pattern.EpisodeIDs, episode)
	}
	sort.Strings(snap.Pattern.EpisodeIDs)
	return nil
}

func migratePatternLedger(ctx context.Context, tx *sql.Tx) error {
	var done bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_learning_migrations WHERE name='pattern-ledger-v2')`).Scan(&done); err != nil {
		return err
	}
	if done {
		return nil
	}
	// Old revisions copied all prior members. Retain the first membership only;
	// its revision and immutable dependency still reconstruct every old revision.
	_, err := tx.ExecContext(ctx, `DELETE FROM df_learning_pattern_members AS m WHERE EXISTS(SELECT 1 FROM df_learning_pattern_members p WHERE p.pattern_id=m.pattern_id AND p.experience_id=m.experience_id AND p.revision<m.revision);
 CREATE UNIQUE INDEX IF NOT EXISTS df_learning_pattern_ledger_member ON df_learning_pattern_members(pattern_id,experience_id);
 INSERT INTO df_learning_migrations VALUES('pattern-ledger-v2');`)
	return err
}

// Walk the immutable ledger in bounded pages, merging at most one uncommitted
// member while planning a revision. The SQL cursor remains closed during source
// verification, so nested provenance reads never retain a lifetime-sized set.
func walkPatternMembers(ctx context.Context, tx *sql.Tx, snap patternSnapshot, visit func(experienceDependency) error) error {
	if snap.LedgerDigest == "" {
		for _, dep := range snap.Members {
			if err := visit(dep); err != nil {
				return err
			}
		}
		return nil
	}
	pending := slices.Clone(snap.pending)
	slices.SortFunc(pending, func(a, b experienceDependency) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	cursor := ""
	for {
		rows, err := tx.QueryContext(ctx, `SELECT m.experience_id,m.experience_sha,e.extraction_id,x.payload_sha,x.result_sha,e.session_id,m.episode_id,m.independence_key,m.epoch
 FROM df_learning_pattern_members m JOIN df_learning_experiences e ON e.id=m.experience_id JOIN df_learning_extractions x ON x.id=e.extraction_id
 WHERE m.pattern_id=? AND m.revision<=? AND m.experience_id>? ORDER BY m.experience_id LIMIT 32`, snap.Pattern.ID, snap.Pattern.Revision, cursor)
		if err != nil {
			return err
		}
		page := []experienceDependency{}
		for rows.Next() {
			var d experienceDependency
			if err = rows.Scan(&d.ID, &d.SHA, &d.Extraction, &d.SourceSHA, &d.ResultSHA, &d.Session, &d.Episode, &d.Independent, &d.Epoch); err != nil {
				break
			}
			page = append(page, d)
		}
		if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, dep := range page {
			for len(pending) > 0 && pending[0].ID < dep.ID {
				if err = visit(pending[0]); err != nil {
					return err
				}
				pending = pending[1:]
			}
			if len(pending) > 0 && pending[0].ID == dep.ID {
				if pending[0] != dep {
					return harness.ErrPermissionDenied
				}
				pending = pending[1:]
			}
			if err = visit(dep); err != nil {
				return err
			}
			cursor = dep.ID
		}
		if len(page) < 32 {
			break
		}
	}
	for _, dep := range pending {
		if err := visit(dep); err != nil {
			return err
		}
	}
	return nil
}

type ledgerHash struct {
	sum   hash.Hash
	count int
}

func newLedgerHash() *ledgerHash {
	h := &ledgerHash{sum: sha256.New()}
	h.sum.Write([]byte("["))
	return h
}
func (h *ledgerHash) add(value any) {
	if h.count > 0 {
		h.sum.Write([]byte(","))
	}
	raw, _ := json.Marshal(value)
	h.sum.Write(raw)
	h.count++
}
func (h *ledgerHash) digest(nilEmpty bool) string {
	if h.count == 0 && nilEmpty {
		return encodedDigest(nil)
	}
	h.sum.Write([]byte("]"))
	return hex.EncodeToString(h.sum.Sum(nil))
}

func (s *Store) savedExperienceTx(ctx context.Context, tx *sql.Tx, dep experienceDependency) (harness.SkillExperience, error) {
	var e harness.SkillExperience
	var raw []byte
	var sha string
	if err := tx.QueryRowContext(ctx, `SELECT payload,payload_sha FROM df_learning_experiences WHERE id=? AND session_id=? AND episode_id=?`, dep.ID, dep.Session, dep.Episode).Scan(&raw, &sha); err != nil {
		return e, err
	}
	if len(raw) > 64<<10 || digest(raw) != sha || sha != dep.SHA || json.Unmarshal(raw, &e) != nil || e.ID != dep.ID || e.SessionID != dep.Session || e.EpisodeID != dep.Episode {
		return e, invalid("inconsistent ledger experience")
	}
	return e, nil
}

// Resolve only sources of the same host task. The scan holds one source pair,
// regardless of how many independent tasks the pattern has accumulated.
func (s *Store) pagedResolutionTx(ctx context.Context, tx *sql.Tx, snap patternSnapshot, prior experienceDependency, e harness.SkillExperience) (string, error) {
	if snap.ResolutionPolicy == "" {
		return "", nil
	}
	if snap.ResolutionPolicy != ResolutionPolicyVersion {
		return "", invalid("invalid resolution policy")
	}
	var before extractionSnapshot
	loaded := false
	chosen := ""
	var accepted int64
	err := walkPatternMembers(ctx, tx, snap, func(dep experienceDependency) error {
		if !sameResolutionTask(prior, dep) {
			return nil
		}
		v, err := s.savedExperienceTx(ctx, tx, dep)
		if err != nil {
			return err
		}
		if v.Candidate.Outcome != "observed_execution" {
			return nil
		}
		if !loaded {
			before, err = s.resolutionCaptureTx(ctx, tx, prior)
			if err != nil {
				return err
			}
			loaded = true
		}
		after, err := s.resolutionCaptureTx(ctx, tx, dep)
		if err != nil {
			return err
		}
		sequence := resolutionAccepted(before, after, e.Candidate, v.Candidate)
		if sequence > 0 && (chosen == "" || sequence < accepted || sequence == accepted && dep.ID < chosen) {
			chosen, accepted = dep.ID, sequence
		}
		return nil
	})
	return chosen, err
}

// The projection is aggregated in SQLite; only 32 dependency/episode examples
// and resolution links enter the immutable revision or a model's context.
func (s *Store) projectPagedPatternTx(ctx context.Context, tx *sql.Tx, snap patternSnapshot, owner string, live bool) (patternSnapshot, error) {
	if len(snap.pending) > 1 {
		return snap, invalid("too many pending ledger members")
	}
	out := snap
	out.Members = nil
	out.Pattern.ExperienceIDs = []string{}
	out.Pattern.EpisodeIDs = []string{}
	out.Resolutions = nil
	members, resolutions := newLedgerHash(), newLedgerHash()
	found, ownerFound, unresolved := false, false, false
	// The temporary projection is transaction-scoped and bounded in Go memory.
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS df_learning_projection(id TEXT PRIMARY KEY,episode TEXT NOT NULL,independence TEXT NOT NULL,observed INTEGER NOT NULL);DELETE FROM df_learning_projection;`); err != nil {
		return out, err
	}
	err := walkPatternMembers(ctx, tx, snap, func(dep experienceDependency) error {
		e, err := s.savedExperienceTx(ctx, tx, dep)
		if err != nil {
			return err
		}
		if !snap.Shared && dep.Session != snap.Pattern.SessionID {
			return harness.ErrPermissionDenied
		}
		if live {
			var checked experienceDependency
			e, checked, err = s.experienceTx(ctx, tx, dep.ID)
			if err != nil {
				return err
			}
			if checked != dep {
				return harness.ErrPermissionDenied
			}
			workspace, err := sessionWorkspaceTx(ctx, tx, dep.Session)
			if err != nil {
				return err
			}
			if workspace != owner {
				return harness.ErrPermissionDenied
			}
			var original string
			if err = tx.QueryRowContext(ctx, `SELECT workspace_key FROM df_learning_episodes WHERE id=?`, dep.Episode).Scan(&original); err != nil {
				return err
			}
			if original != owner {
				return harness.ErrPermissionDenied
			}
		}
		ownerFound = ownerFound || dep.Session == snap.Pattern.SessionID
		members.add(dep)
		if len(out.Members) < maxPatternMembers {
			out.Members = append(out.Members, dep)
			out.Pattern.ExperienceIDs = append(out.Pattern.ExperienceIDs, dep.ID)
		}
		if e.ID == snap.Pattern.RepresentativeID {
			found = true
			out.Pattern.Candidate = e.Candidate
		}
		observed := e.Candidate.Outcome == "observed_execution"
		if !observed {
			chosen, err := s.pagedResolutionTx(ctx, tx, snap, dep, e)
			if err != nil {
				return err
			}
			if chosen == "" {
				unresolved = true
			} else {
				r := experienceResolution{Prior: dep.ID, Verified: chosen}
				resolutions.add(r)
				if len(out.Resolutions) < maxPatternMembers {
					out.Resolutions = append(out.Resolutions, r)
				}
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO df_learning_projection VALUES(?,?,?,?)`, dep.ID, dep.Episode, dep.Independent, observed)
		return err
	})
	if err != nil {
		return out, err
	}
	if !found || !ownerFound || members.count < 1 {
		return out, invalid("missing ledger owner or representative")
	}
	out.LedgerDigest = members.digest(false)
	out.ResolutionDigest = resolutions.digest(true)
	if err = tx.QueryRowContext(ctx, `SELECT count(DISTINCT independence),count(DISTINCT CASE WHEN observed=1 THEN independence END) FROM df_learning_projection`).Scan(&out.Pattern.SupportEpisodes, &out.Pattern.ObservedEpisodes); err != nil {
		return out, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT episode FROM df_learning_projection ORDER BY episode LIMIT 32`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var episode string
		if err = rows.Scan(&episode); err != nil {
			break
		}
		out.Pattern.EpisodeIDs = append(out.Pattern.EpisodeIDs, episode)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return out, err
	}
	switch {
	case len(out.Pattern.ConflictsWith) > 0:
		out.Pattern.EvidenceState = "conflicting"
	case snap.Pattern.EvidenceState == "ambiguous":
		out.Pattern.EvidenceState = "ambiguous"
	case unresolved:
		out.Pattern.EvidenceState = "unresolved"
	case out.Pattern.ObservedEpisodes < 2:
		out.Pattern.EvidenceState = "insufficient_evidence"
	default:
		out.Pattern.EvidenceState = "observed_execution"
	}
	return out, nil
}

func (s *Store) verifyPagedPatternTx(ctx context.Context, tx *sql.Tx, snap patternSnapshot, owner string, live bool) error {
	expected, err := s.projectPagedPatternTx(ctx, tx, snap, owner, live)
	if err != nil {
		return err
	}
	// The first implementation stored all small resolution links with no digest.
	if snap.ResolutionDigest == "" && len(expected.Resolutions) < maxPatternMembers {
		expected.ResolutionDigest = ""
	}
	if encodedDigest(expected) != encodedDigest(snap) {
		return invalid("inconsistent paged pattern projection")
	}
	return nil
}
