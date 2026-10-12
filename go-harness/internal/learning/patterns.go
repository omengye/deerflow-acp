package learning

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

const ConsolidationPolicyVersion = "procedural-consolidation-v1"
const patternStorageLimit = 128 << 10
const maxPatternMembers = 32

type experienceDependency struct {
	ID          string `json:"id"`
	SHA         string `json:"sha256"`
	Extraction  string `json:"extraction"`
	SourceSHA   string `json:"source_sha256"`
	ResultSHA   string `json:"result_sha256"`
	Session     string `json:"session"`
	Episode     string `json:"episode"`
	Independent string `json:"independence_key"`
	Epoch       int64  `json:"epoch"`
}

type patternSnapshot struct {
	Pattern          harness.SkillExperiencePattern `json:"pattern"`
	Members          []experienceDependency         `json:"members"`
	Shared           bool                           `json:"workspace_shared,omitempty"`
	ResolutionPolicy string                         `json:"resolution_policy,omitempty"`
	Resolutions      []experienceResolution         `json:"resolutions,omitempty"`
}

func (s *Store) experienceTx(ctx context.Context, tx *sql.Tx, id string) (e harness.SkillExperience, dep experienceDependency, err error) {
	ctx, finish := withSourceActivity(ctx, tx)
	defer finish(&err)
	ctx = withEvidenceCache(ctx, tx)
	cache := transactionEvidenceCache(ctx, tx)
	if saved, found := cache.experiences[id]; found {
		if saved.active {
			if err := sourceActiveTx(ctx, tx); err != nil {
				return saved.experience, saved.dependency, err
			}
		}
		return saved.experience, saved.dependency, ctx.Err()
	}
	var data []byte
	var revision int64
	var jobID, owner string
	err = tx.QueryRowContext(ctx, `SELECT e.payload,e.payload_sha,e.revision,e.extraction_id,e.session_id,e.episode_id,
 x.payload_sha,x.result_sha,x.epoch,x.job_id,l.independence_key,p.workspace_key
 FROM df_learning_experiences e JOIN df_learning_extractions x ON x.id=e.extraction_id
 JOIN df_learning_episodes p ON p.id=e.episode_id JOIN df_learning_episode_lineage l ON l.episode_id=p.id WHERE e.id=?`, id).Scan(&data, &dep.SHA, &revision, &dep.Extraction, &dep.Session, &dep.Episode, &dep.SourceSHA, &dep.ResultSHA, &dep.Epoch, &jobID, &dep.Independent, &owner)
	if err != nil {
		return e, dep, err
	}
	dep.ID = id
	if len(data) > 64<<10 || digest(data) != dep.SHA || json.Unmarshal(data, &e) != nil || e.ID != id || e.SessionID != dep.Session || e.EpisodeID != dep.Episode || e.Revision != revision || revision != 1 || e.Origin != "model_extracted" || e.Quality != "unassessed" || dep.ResultSHA == "" || dep.Independent == "" {
		return e, dep, invalid("inconsistent pattern experience")
	}
	job, err := skillmaintenance.ReadJob(ctx, tx, jobID)
	if err != nil {
		return e, dep, err
	}
	if job.State != "completed" {
		return e, dep, harness.ErrPermissionDenied
	}
	snap, result, err := s.readExtractionTx(ctx, tx, job)
	if err != nil {
		return e, dep, err
	}
	if snap.ID != dep.Extraction || snap.Session != dep.Session || snap.Episode != dep.Episode || snap.Owner != owner || snap.Epoch != dep.Epoch || job.Request.InputDigest != dep.SourceSHA || result != dep.ResultSHA {
		return e, dep, invalid("inconsistent experience dependency")
	}
	cache.experiences[id] = verifiedExperience{experience: e, dependency: dep, active: sourceActivityRecorded(ctx, tx)}
	return e, dep, nil
}

func (s *Store) patternTx(ctx context.Context, tx *sql.Tx, id string, revision int64, current bool) (out patternSnapshot, err error) {
	ctx, finish := withSourceActivity(ctx, tx)
	defer finish(&err)
	ctx = withEvidenceCache(ctx, tx)
	snap, owner, err := s.patternRecordTx(ctx, tx, id, revision, current)
	if err != nil {
		return snap, err
	}
	if err = s.sharedPatternTx(ctx, tx, snap, owner); err != nil {
		return snap, err
	}
	members := make([]harness.SkillExperience, 0, len(snap.Members))
	for _, dep := range snap.Members {
		e, liveDep, err := s.experienceTx(ctx, tx, dep.ID)
		if err != nil {
			return snap, err
		}
		if dep != liveDep {
			return snap, harness.ErrPermissionDenied
		}
		var workspace string
		if err = tx.QueryRowContext(ctx, `SELECT workspace_key FROM df_learning_episodes WHERE id=?`, dep.Episode).Scan(&workspace); err != nil || workspace != owner {
			if err == nil {
				err = harness.ErrPermissionDenied
			}
			return snap, err
		}
		if liveWorkspace, err := sessionWorkspaceTx(ctx, tx, dep.Session); err != nil || liveWorkspace != owner {
			if err == nil {
				err = harness.ErrPermissionDenied
			}
			return snap, err
		}
		members = append(members, e)
	}
	expected, err := projectPattern(snap, members)
	if err != nil || encodedDigest(expected) != encodedDigest(snap.Pattern) {
		return snap, invalid("inconsistent pattern projection")
	}
	return snap, nil
}

// History reads verify the saved projection but do not rebuild every task
// tree. Model admission and later promotion always use patternTx instead.
func (s *Store) patternRecordTx(ctx context.Context, tx *sql.Tx, id string, revision int64, current bool) (patternSnapshot, string, error) {
	var snap patternSnapshot
	var data []byte
	var sha, session, owner string
	var live int64
	err := tx.QueryRowContext(ctx, `SELECT r.payload,r.payload_sha,p.session_id,p.owner_key,p.current_revision
 FROM df_learning_pattern_revisions r JOIN df_learning_patterns p ON p.id=r.pattern_id
 WHERE r.pattern_id=? AND r.revision=?`, id, revision).Scan(&data, &sha, &session, &owner, &live)
	if err != nil {
		return snap, owner, err
	}
	if current && live != revision {
		return snap, owner, harness.ErrSkillConflict
	}
	if len(data) > patternStorageLimit || digest(data) != sha || json.Unmarshal(data, &snap) != nil || snap.Pattern.ID != id || snap.Pattern.Revision != revision || snap.Pattern.SessionID != session || snap.Pattern.Origin != "model_extracted" || snap.Pattern.Quality != "unassessed" || snap.Pattern.SourceState != "checked_at_consolidation" || len(snap.Members) < 1 || len(snap.Members) > maxPatternMembers {
		return snap, owner, invalid("inconsistent pattern revision")
	}
	var sharedOwner string
	err = tx.QueryRowContext(ctx, `SELECT workspace_key FROM df_learning_pattern_sharing WHERE pattern_id=?`, id).Scan(&sharedOwner)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return snap, owner, err
	}
	if snap.Shared != (err == nil) || snap.Shared && sharedOwner != owner {
		return snap, owner, invalid("inconsistent pattern sharing scope")
	}
	rows, err := tx.QueryContext(ctx, `SELECT experience_id,experience_sha,episode_id,independence_key,epoch
 FROM df_learning_pattern_members WHERE pattern_id=? AND revision=? ORDER BY experience_id LIMIT ?`, id, revision, maxPatternMembers+1)
	if err != nil {
		return snap, owner, err
	}
	var saved []experienceDependency
	for rows.Next() {
		var dep experienceDependency
		if err = rows.Scan(&dep.ID, &dep.SHA, &dep.Episode, &dep.Independent, &dep.Epoch); err != nil {
			break
		}
		saved = append(saved, dep)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return snap, owner, err
	}
	if len(saved) != len(snap.Members) {
		return snap, owner, invalid("inconsistent pattern members")
	}
	members := make([]harness.SkillExperience, 0, len(snap.Members))
	ownerMember := false
	for i, dep := range snap.Members {
		ownerMember = ownerMember || dep.Session == session
		if dep.ID != saved[i].ID || dep.SHA != saved[i].SHA || dep.Episode != saved[i].Episode || dep.Independent != saved[i].Independent || dep.Epoch != saved[i].Epoch || !snap.Shared && dep.Session != session || i > 0 && dep.ID <= snap.Members[i-1].ID {
			return snap, owner, invalid("inconsistent pattern dependency")
		}
		var raw []byte
		var sha string
		var e harness.SkillExperience
		if err = tx.QueryRowContext(ctx, `SELECT payload,payload_sha FROM df_learning_experiences WHERE id=? AND session_id=? AND episode_id=?`, dep.ID, dep.Session, dep.Episode).Scan(&raw, &sha); err != nil {
			return snap, owner, err
		}
		if len(raw) > 64<<10 || digest(raw) != sha || sha != dep.SHA || json.Unmarshal(raw, &e) != nil || e.ID != dep.ID || e.SessionID != dep.Session || e.EpisodeID != dep.Episode {
			return snap, owner, invalid("inconsistent saved pattern experience")
		}
		members = append(members, e)
	}
	if !ownerMember {
		return snap, owner, invalid("missing pattern owner membership")
	}
	expected, err := projectPattern(snap, members)
	if err != nil || encodedDigest(expected) != encodedDigest(snap.Pattern) {
		return snap, owner, invalid("inconsistent saved pattern projection")
	}
	if err = s.verifyPatternResolutionsTx(ctx, tx, snap, members); err != nil {
		return snap, owner, err
	}
	return snap, owner, nil
}

func buildPattern(id, session string, revision int64, representative string, deps []experienceDependency, experiences []harness.SkillExperience, conflicts []string, ambiguous bool, now time.Time) (harness.SkillExperiencePattern, error) {
	return buildPatternResolved(id, session, revision, representative, deps, experiences, conflicts, ambiguous, now, nil)
}

func buildPatternResolved(id, session string, revision int64, representative string, deps []experienceDependency, experiences []harness.SkillExperience, conflicts []string, ambiguous bool, now time.Time, resolved map[string]bool) (harness.SkillExperiencePattern, error) {
	p := harness.SkillExperiencePattern{ID: id, SessionID: session, Revision: revision, Origin: "model_extracted", Quality: "unassessed", SourceState: "checked_at_consolidation", RepresentativeID: representative, ExperienceIDs: []string{}, EpisodeIDs: []string{}, ConflictsWith: slices.Clone(conflicts), UpdatedAt: now}
	if len(deps) != len(experiences) || len(deps) < 1 || len(deps) > maxPatternMembers || revision < 1 {
		return p, invalid("invalid pattern member bounds")
	}
	if p.ConflictsWith == nil {
		p.ConflictsWith = []string{}
	}
	sort.Strings(p.ConflictsWith)
	if len(p.ConflictsWith) > 16 || len(slices.Compact(slices.Clone(p.ConflictsWith))) != len(p.ConflictsWith) || slices.Contains(p.ConflictsWith, id) {
		return p, invalid("invalid pattern conflicts")
	}
	episodes, observed := map[string]bool{}, map[string]bool{}
	independent := map[string]bool{}
	found, unresolved := false, false
	for i, e := range experiences {
		dep := deps[i]
		p.ExperienceIDs = append(p.ExperienceIDs, e.ID)
		episodes[e.EpisodeID], independent[dep.Independent] = true, true
		if e.Candidate.Outcome == "observed_execution" {
			observed[dep.Independent] = true
		} else if !resolved[e.ID] {
			unresolved = true
		}
		if e.ID == representative {
			p.Candidate, found = e.Candidate, true
		}
	}
	if !found {
		return p, invalid("missing pattern representative")
	}
	for episode := range episodes {
		p.EpisodeIDs = append(p.EpisodeIDs, episode)
	}
	sort.Strings(p.EpisodeIDs)
	p.SupportEpisodes, p.ObservedEpisodes = len(independent), len(observed)
	switch {
	case len(p.ConflictsWith) != 0:
		p.EvidenceState = "conflicting"
	case ambiguous:
		p.EvidenceState = "ambiguous"
	case unresolved:
		p.EvidenceState = "unresolved"
	case p.ObservedEpisodes < 2:
		p.EvidenceState = "insufficient_evidence"
	default:
		p.EvidenceState = "observed_execution"
	}
	return p, nil
}

func (s *Store) Patterns(ctx context.Context, sessionID, after string, limit int) (harness.SkillExperiencePatternPage, error) {
	page := harness.SkillExperiencePatternPage{Patterns: []harness.SkillExperiencePattern{}}
	if len(after) > 128 || limit < 1 || limit > 100 {
		return page, harness.ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	return s.patternPageTx(ctx, tx, `SELECT id,current_revision FROM df_learning_patterns p WHERE session_id=? AND id>?
 AND NOT EXISTS(SELECT 1 FROM df_learning_pattern_sharing sh WHERE sh.pattern_id=p.id) ORDER BY id LIMIT ?`, sessionID, after, limit, false)
}

// WorkspacePatterns is host-only: owning one ACP session does not authorize
// reading another conversation, even when both use the same directory.
func (s *Store) WorkspacePatterns(ctx context.Context, sessionID, after string, limit int) (harness.SkillExperiencePatternPage, error) {
	page := harness.SkillExperiencePatternPage{Patterns: []harness.SkillExperiencePattern{}}
	if len(after) > 128 || limit < 1 || limit > 100 {
		return page, harness.ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	owner, err := sessionWorkspaceTx(ctx, tx, sessionID)
	if err != nil {
		return page, err
	}
	var epoch int64
	if err = tx.QueryRowContext(ctx, `SELECT epoch FROM harness_learning_controls WHERE session_id=?`, sessionID).Scan(&epoch); err != nil {
		return page, err
	}
	if err = s.sharedSessionTx(ctx, tx, sessionID, owner, epoch); err != nil {
		return page, err
	}
	return s.patternPageTx(ctx, tx, `SELECT p.id,p.current_revision FROM df_learning_patterns p JOIN df_learning_pattern_sharing sh ON sh.pattern_id=p.id
 WHERE sh.workspace_key=? AND p.id>? ORDER BY p.id LIMIT ?`, owner, after, limit, true)
}

func (s *Store) patternPageTx(ctx context.Context, tx *sql.Tx, query, key, after string, limit int, shared bool) (harness.SkillExperiencePatternPage, error) {
	page := harness.SkillExperiencePatternPage{Patterns: []harness.SkillExperiencePattern{}}
	rows, err := tx.QueryContext(ctx, query, key, after, limit+1)
	if err != nil {
		return page, err
	}
	type ref struct {
		id       string
		revision int64
	}
	var refs []ref
	for rows.Next() {
		var r ref
		if err = rows.Scan(&r.id, &r.revision); err != nil {
			break
		}
		refs = append(refs, r)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return page, err
	}
	if len(refs) > limit {
		refs, page.Next = refs[:limit], refs[limit-1].id
	}
	for _, r := range refs {
		snap, owner, err := s.patternRecordTx(ctx, tx, r.id, r.revision, true)
		if err != nil {
			return page, err
		}
		if shared {
			if err = s.sharedPatternTx(ctx, tx, snap, owner); err != nil {
				return page, err
			}
		}
		review, err := patternReviewTx(ctx, tx, r.id)
		if err != nil {
			return page, err
		}
		snap.Pattern.Review = &review
		snap.Pattern.SourceState = "not_revalidated"
		page.Patterns = append(page.Patterns, snap.Pattern)
	}
	return page, tx.Commit()
}
