package learning

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

const PromotionPolicyVersion = "procedural-proposal-evidence-v2"
const ReviewDraftPolicyVersion = "procedural-review-draft-evidence-v1"

type Promotion struct {
	Target      harness.SkillTarget
	Head        harness.SkillHead
	OverlayBase *harness.SkillRef
	Relation    string
	SessionID   string
	Name        string
	Evidence    []harness.SkillDiscoveryEvidence
	Summary     string
	Manual      bool
}

func (s *Store) PromotionEnabled() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed && s.enabled && s.extractor != nil && s.consolidator != nil && s.matcher != nil && s.matchAccess != nil
}

func (s *Store) PromotionDatabase(db *sql.DB) bool { return s != nil && s.db == db }

// A scan hint is not evidence authority. It only avoids rebuilding task trees
// while quotas are full or the already-pinned target head has changed.
func (s *Store) PromotionHint(ctx context.Context, id string) (harness.SkillExperienceMatch, error) {
	var out harness.SkillExperienceMatch
	var data []byte
	var sha string
	if err := s.db.QueryRowContext(ctx, `SELECT result,result_sha FROM df_learning_matches WHERE id=?
 UNION ALL SELECT result,result_sha FROM df_learning_adjudications WHERE id=?`, id, id).Scan(&data, &sha); err != nil {
		return out, err
	}
	var result matchResult
	if len(data) > 32<<10 || sha == "" || digest(data) != sha || json.Unmarshal(data, &result) != nil || result.Match.ID != id || result.Match.Relation != "new_capability" && (result.Match.Target == nil || result.Match.Head == nil) {
		return out, invalid("inconsistent promotion scan hint")
	}
	return result.Match, nil
}

// Automatic publication evidence never accepts fewer than two episodes. Both
// promotion paths run inside the host's existing publication transaction.
func (s *Store) PromotionTx(ctx context.Context, tx *sql.Tx, matchID string, required int, from, until time.Time) (Promotion, error) {
	if !s.PromotionEnabled() || tx == nil || required < 2 || required > 16 || !from.Before(until) {
		return Promotion{}, harness.ErrPermissionDenied
	}
	return s.promotionTx(ctx, tx, matchID, required, from, until, false)
}

// ReviewDraftPromotionTx is only for separately enabled automatic drafting. A
// complete local observation is still required; this is not a quality vote.
func (s *Store) ReviewDraftPromotionTx(ctx context.Context, tx *sql.Tx, matchID string, from, until time.Time) (Promotion, error) {
	if !s.PromotionEnabled() || tx == nil || !from.Before(until) {
		return Promotion{}, harness.ErrPermissionDenied
	}
	return s.promotionTx(ctx, tx, matchID, 1, from, until, true)
}

// ResumePromotionTx preserves the generation's evidence revision while validating
// the complete current ledger for withdrawals and new conflicts. Only a bound
// promotion may do this; new discoveries still require the current revision.
type promotionResumeKey struct{}

func (s *Store) ResumePromotionTx(ctx context.Context, tx *sql.Tx, job, match, pattern string, required int, from, until time.Time) (Promotion, error) {
	if err := s.GuardPromotionTx(ctx, tx, job, match, pattern); err != nil {
		return Promotion{}, err
	}
	return s.PromotionTx(context.WithValue(ctx, promotionResumeKey{}, true), tx, match, required, from, until)
}

func (s *Store) promotionTx(ctx context.Context, tx *sql.Tx, matchID string, required int, from, until time.Time, reviewDrafts bool) (out Promotion, err error) {
	ctx, finish := withSourceActivity(ctx, tx)
	defer finish(&err)
	ctx = withEvidenceCache(ctx, tx)
	var result *matchResult
	var inputSHA string
	manual := strings.HasPrefix(matchID, "ladj_")
	if manual {
		_, result, inputSHA, err = s.readAdjudicationTx(ctx, tx, matchID)
	} else {
		var jobID string
		if err = tx.QueryRowContext(ctx, `SELECT job_id FROM df_learning_matches WHERE id=?`, matchID).Scan(&jobID); err != nil {
			return out, err
		}
		job, readErr := skillmaintenance.ReadJob(ctx, tx, jobID)
		if readErr != nil {
			return out, readErr
		}
		if job.State != "completed" {
			return out, harness.ErrPermissionDenied
		}
		_, result, err = s.readMatchTx(ctx, tx, job)
		inputSHA = job.Request.InputDigest
	}
	if err != nil || result == nil {
		if err == nil {
			err = harness.ErrPermissionDenied
		}
		return out, err
	}
	m := result.Match
	switch m.Relation {
	case "body_gap", "trigger_mismatch":
		if m.Target == nil || m.Head == nil || !m.Head.Exists || !m.Head.Enabled {
			return out, harness.ErrPermissionDenied
		}
	case "new_capability":
		if m.Target != nil || m.Head != nil || m.OverlayBase != nil || !harness.ValidSkillCapabilityName(m.SuggestedName) {
			return out, harness.ErrPermissionDenied
		}
	default:
		return out, harness.ErrPermissionDenied
	}
	resume, _ := ctx.Value(promotionResumeKey{}).(bool)
	p, err := s.patternTx(ctx, tx, m.PatternID, m.PatternRevision, !resume)
	if err != nil {
		return out, err
	}
	if resume {
		var revision int64
		if err = tx.QueryRowContext(ctx, `SELECT current_revision FROM df_learning_patterns WHERE id=?`, m.PatternID).Scan(&revision); err != nil {
			return out, err
		}
		live, e := s.patternTx(ctx, tx, m.PatternID, revision, true)
		if e != nil {
			return out, e
		}
		if len(live.Pattern.ConflictsWith) != 0 || live.Pattern.EvidenceState != "observed_execution" {
			return out, harness.ErrPermissionDenied
		}
		if err = s.guardPatternConsolidationTx(ctx, tx, live); err != nil {
			return out, err
		}
	}
	if err = s.guardPatternConsolidationTx(ctx, tx, p); err != nil {
		return out, err
	}
	observed := p.Pattern.EvidenceState == "observed_execution" || reviewDrafts && p.Pattern.EvidenceState == "insufficient_evidence"
	if !observed || p.Pattern.ObservedEpisodes < required || len(p.Pattern.ConflictsWith) != 0 {
		return out, harness.ErrPermissionDenied
	}
	// The immutable matching input already binds the historical extraction,
	// consolidation and matching models. A new primary model requires a new
	// evaluation, not loss of otherwise valid, authorized task evidence.
	policy := encodedDigest([]string{PromotionPolicyVersion, PolicyVersion, ConsolidationPolicyVersion, MatchingPolicyVersion, inputSHA, s.matchAccess.Identity()})
	if manual {
		policy = encodedDigest([]string{policy, AdjudicationPolicyVersion})
	}
	if reviewDrafts {
		policy = encodedDigest([]string{policy, ReviewDraftPolicyVersion})
	}
	common := harness.SkillExperienceDiscoveryReference{MatchID: matchID, PatternID: m.PatternID, PatternRevision: m.PatternRevision, MatchInputSHA: inputSHA, MatchResultSHA: encodedDigest(*result), PatternSHA: encodedDigest(p), PolicyIdentity: policy}
	if _, err = tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS df_learning_promotion_projection(independence TEXT PRIMARY KEY,root TEXT NOT NULL,payload BLOB NOT NULL,spent INTEGER NOT NULL DEFAULT 0); DELETE FROM df_learning_promotion_projection`); err != nil {
		return out, err
	}
	resolved := map[string]bool{}
	for _, resolution := range p.Resolutions {
		resolved[resolution.Prior] = true
	}
	err = walkPatternMembers(ctx, tx, p, func(dep experienceDependency) error {
		e, _, err := s.experienceTx(ctx, tx, dep.ID)
		if err != nil {
			return err
		}
		if e.Candidate.Outcome != "observed_execution" {
			// Retain and revalidate history, but only the later completed source
			// can contribute an observation for this original task.
			if resolved[dep.ID] || p.ResolutionDigest != "" && p.Pattern.EvidenceState == "observed_execution" {
				return nil
			}
			return harness.ErrPermissionDenied
		}
		var extractionJob string
		if err = tx.QueryRowContext(ctx, `SELECT job_id FROM df_learning_extractions WHERE id=?`, dep.Extraction).Scan(&extractionJob); err != nil {
			return err
		}
		j, err := skillmaintenance.ReadJob(ctx, tx, extractionJob)
		if err != nil {
			return err
		}
		x, _, err := s.readExtractionTx(ctx, tx, j)
		if err != nil {
			return err
		}
		for _, ref := range x.View.References {
			if ref.Kind != "tool_observation" || ref.State != string(harness.ReceiptCompleted) || ref.Truncated || !localObservation(ref.ToolName) || !slices.Contains(e.Candidate.References, ref.ID) {
				continue
			}
			for _, src := range x.Sources {
				ended, observed := observationEnded(x, src, ref.RunID)
				if !observed || ended.Before(from) || ended.After(until) {
					continue
				}
				copy := common
				copy.EpisodeID, copy.IndependenceKey = dep.Episode, dep.Independent
				fixed := harness.SkillDiscoveryEvidence{RootID: src.Root, Experience: &copy}
				raw, _ := json.Marshal(fixed)
				if _, err = tx.ExecContext(ctx, `INSERT INTO df_learning_promotion_projection(independence,root,payload) VALUES(?,?,?) ON CONFLICT(independence) DO UPDATE SET root=excluded.root,payload=excluded.payload WHERE excluded.root<df_learning_promotion_projection.root`, dep.Independent, fixed.RootID, raw); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM df_learning_promotion_projection`).Scan(&count); err != nil {
		return out, err
	}
	if count < required {
		return out, harness.ErrPermissionDenied
	}
	var discovery bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='df_skill_discoveries')`).Scan(&discovery); err != nil {
		return out, err
	}
	if discovery && !resume {
		// Prioritize new roots without copying the lifetime ledger into Go.
		// Discovery still performs the authoritative target/version spend check.
		var source, name, version string
		if m.Target != nil {
			source, name, version = m.Target.SourceIdentity, m.Target.Name, m.Head.Version
		}
		_, err = tx.ExecContext(ctx, `UPDATE df_learning_promotion_projection AS x SET spent=1 WHERE EXISTS(SELECT 1 FROM df_skill_discoveries d JOIN json_each(CAST(d.payload AS TEXT),'$.evidence') e WHERE json_extract(e.value,'$.root_id')=x.root AND ((?<>'' AND d.source_key=? AND d.name=? AND json_extract(CAST(d.payload AS TEXT),'$.base.version')=?) OR (?='' AND json_extract(e.value,'$.experience.pattern_id')=?)))`, source, source, name, version, source, m.PatternID)
		if err != nil {
			return out, err
		}
	}
	out = Promotion{Relation: m.Relation, SessionID: m.SessionID, Name: m.SuggestedName, Manual: manual}
	if m.Target != nil {
		out.Target, out.Head, out.OverlayBase = *m.Target, *m.Head, m.OverlayBase
	}
	rows, err := tx.QueryContext(ctx, `SELECT payload FROM df_learning_promotion_projection ORDER BY spent,independence LIMIT 32`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var raw []byte
		var evidence harness.SkillDiscoveryEvidence
		if err = rows.Scan(&raw); err != nil {
			break
		}
		if err = json.Unmarshal(raw, &evidence); err != nil {
			break
		}
		out.Evidence = append(out.Evidence, evidence)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return out, err
	}
	candidate := p.Pattern.Candidate
	candidate.References = nil
	origin := ""
	if manual {
		origin = m.Origin
	}
	data, err := json.Marshal(struct {
		Origin    string                           `json:"target_origin,omitempty"`
		Quality   string                           `json:"quality"`
		Relation  string                           `json:"relation"`
		Reason    string                           `json:"matching_reason"`
		Procedure harness.SkillExperienceCandidate `json:"observed_procedure"`
	}{origin, "unassessed; independent local completion receipts do not prove efficacy or skill causality", m.Relation, m.Reason, candidate})
	if err != nil || len(data) > 32<<10 {
		return Promotion{}, invalid("promotion summary exceeds bounds")
	}
	out.Summary = string(data)
	return out, nil
}

func (s *Store) Promotion(ctx context.Context, id string, required int, from, until time.Time) (Promotion, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Promotion{}, err
	}
	defer tx.Rollback()
	return s.PromotionTx(ctx, tx, id, required, from, until)
}

// IDs are only scan hints. Publication admission revalidates the entire source
// tree, current revision, inventory and exact matching result transactionally.
func (s *Store) PromotionIDs(ctx context.Context, after string, includeNew bool) ([]string, error) {
	return s.PromotionIDsWithReviewDrafts(ctx, after, includeNew, false)
}

func (s *Store) PromotionIDsWithReviewDrafts(ctx context.Context, after string, includeNew, includeDrafts bool) ([]string, error) {
	if !s.PromotionEnabled() {
		return nil, harness.ErrPermissionDenied
	}
	rows, err := s.db.QueryContext(ctx, `SELECT m.id FROM (
 SELECT id,pattern_id,revision,result,result_sha,job_id,0 AS manual FROM df_learning_matches
 UNION ALL SELECT id,pattern_id,revision,result,result_sha,'' AS job_id,1 AS manual FROM df_learning_adjudications) m
 LEFT JOIN df_skill_maintenance_jobs j ON j.id=m.job_id JOIN df_learning_patterns p ON p.id=m.pattern_id
 LEFT JOIN df_learning_pattern_reviews review ON review.pattern_id=p.id
 JOIN df_learning_pattern_revisions r ON r.pattern_id=p.id AND r.revision=p.current_revision
	WHERE m.id>? AND m.result_sha<>'' AND (j.state='completed' OR m.manual=1) AND m.revision=p.current_revision
 AND (m.manual=1 OR EXISTS(SELECT 1 FROM df_learning_match_inputs i WHERE i.pattern_id=m.pattern_id AND i.revision=m.revision AND i.job_id=m.job_id AND i.state='processed'))
 AND COALESCE(review.ignored,0)=0 AND (m.manual=1 AND review.adjudication_id=m.id
 OR m.manual=0 AND COALESCE(review.adjudication_revision,0)<>m.revision)
	AND (json_extract(CAST(m.result AS TEXT),'$.match.relation') IN ('body_gap','trigger_mismatch')
	 OR (? AND json_extract(CAST(m.result AS TEXT),'$.match.relation')='new_capability'))
 AND (json_extract(CAST(r.payload AS TEXT),'$.pattern.evidence_state')='observed_execution'
 OR (? AND json_extract(CAST(r.payload AS TEXT),'$.pattern.evidence_state')='insufficient_evidence'
 AND json_extract(CAST(r.payload AS TEXT),'$.pattern.observed_episodes')>=1))
 AND NOT EXISTS(SELECT 1 FROM df_learning_promotion_dependencies dep WHERE dep.match_id=m.id)
	ORDER BY m.id LIMIT 8`, after, includeNew, includeDrafts)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	return ids, errors.Join(err, rows.Err(), rows.Close())
}

// Source choice is supplied by trusted discovery configuration, never by a
// matcher. Recheck the canonical session workspace and frozen runtime policy.
func (s *Store) NewPromotionTargetTx(ctx context.Context, tx *sql.Tx, sessionID, sourceID, name string) (harness.SkillTarget, error) {
	if !s.PromotionEnabled() {
		return harness.SkillTarget{}, harness.ErrPermissionDenied
	}
	var cwd string
	if err := tx.QueryRowContext(ctx, `SELECT cwd FROM harness_sessions WHERE id=?`, sessionID).Scan(&cwd); err != nil {
		return harness.SkillTarget{}, err
	}
	return s.matchAccess.NewCapabilityTargetTx(ctx, tx, cwd, sourceID, name)
}

func (s *Store) NewPromotionTarget(ctx context.Context, sessionID, sourceID, name string) (harness.SkillTarget, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return harness.SkillTarget{}, err
	}
	defer tx.Rollback()
	return s.NewPromotionTargetTx(ctx, tx, sessionID, sourceID, name)
}
