package evolution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/learning"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

type discoveryHost struct {
	options   harness.SkillDiscoveryOptions
	lifecycle collectorLifecycle
	now       func() time.Time
}

type discoverySnapshot struct {
	Kind            string                                       `json:"kind,omitempty"`
	ID              string                                       `json:"id"`
	Target          harness.SkillTarget                          `json:"target"`
	Base            harness.SkillHead                            `json:"base"`
	OverlayBase     *harness.SkillRef                            `json:"overlay_base,omitempty"`
	Options         harness.SkillDiscoveryOptions                `json:"options"`
	CreatedAt       time.Time                                    `json:"created_at"`
	Evidence        []harness.SkillDiscoveryEvidence             `json:"evidence"`
	EpisodeDedup    bool                                         `json:"episode_dedup,omitempty"`
	HostAdjudicated bool                                         `json:"host_adjudicated,omitempty"`
	ReviewDraft     bool                                         `json:"review_draft,omitempty"`
	CreationRetry   *harness.SkillLearningNewSkillRetryReference `json:"creation_retry,omitempty"`
}

var errDiscoveryEvidence = errors.New("automatic discovery evidence changed")

func (s *Service) DiscoveryEnabled() bool { return s.discovery != nil }

func validDiscoveryOptions(o harness.SkillDiscoveryOptions) bool {
	return (!(o.ExperienceNewSkillsEnabled || o.ExperienceReviewDraftsEnabled) || o.ExperiencePatternsEnabled) && (!(o.MissingReportsEnabled || o.SuccessfulWorkflowsEnabled || o.ExperienceNewSkillsEnabled) || harness.ValidSkillName(o.NewSkillSourceID)) && o.RepeatThreshold >= 2 && o.RepeatThreshold <= 16 && o.RepeatWindowDays >= 1 && o.RepeatWindowDays <= 365 && o.CooldownHours >= 1 && o.CooldownHours <= 8760 && o.MaxDailyProposals >= 1 && o.MaxDailyProposals <= 100 && o.MaxPendingProposals >= 1 && o.MaxPendingProposals <= 1000
}

func migrateDiscovery(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS df_skill_discovery_scan(id INTEGER PRIMARY KEY CHECK(id=1),cursor TEXT NOT NULL,last_ns INTEGER NOT NULL);
INSERT OR IGNORE INTO df_skill_discovery_scan VALUES(1,'',0);
CREATE TABLE IF NOT EXISTS df_skill_discovery_candidates(
 signal_id TEXT PRIMARY KEY REFERENCES df_skill_signals(id) ON DELETE CASCADE,
 source_key TEXT NOT NULL,name TEXT NOT NULL,version TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS df_skill_discovery_candidate_group ON df_skill_discovery_candidates(source_key,name,version,signal_id);
CREATE TABLE IF NOT EXISTS df_skill_discovery_errors(
 signal_id TEXT PRIMARY KEY REFERENCES df_skill_signals(id) ON DELETE CASCADE,
 signal_version INTEGER NOT NULL,feedback_revision INTEGER NOT NULL,attempts INTEGER NOT NULL,retry_ns INTEGER NOT NULL,error_code TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS df_skill_discovery_health(id INTEGER PRIMARY KEY CHECK(id=1),last_error_code TEXT NOT NULL);
INSERT OR IGNORE INTO df_skill_discovery_health VALUES(1,'');
CREATE TABLE IF NOT EXISTS df_skill_discoveries(
 id TEXT PRIMARY KEY,source_key TEXT NOT NULL,name TEXT NOT NULL,job_id TEXT NOT NULL UNIQUE REFERENCES df_skill_maintenance_jobs(id),
 payload BLOB NOT NULL,payload_sha TEXT NOT NULL,created_ns INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS df_skill_discoveries_source ON df_skill_discoveries(source_key,name,id);
CREATE TABLE IF NOT EXISTS df_skill_discovery_consumed(root_digest TEXT PRIMARY KEY,discovery_id TEXT NOT NULL REFERENCES df_skill_discoveries(id));
CREATE TABLE IF NOT EXISTS df_skill_discovery_episode_consumed(digest TEXT PRIMARY KEY,discovery_id TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS df_skill_discovery_episode_roots(root_id TEXT PRIMARY KEY,episode_key TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS df_skill_discovery_experience_scan(id INTEGER PRIMARY KEY CHECK(id=1),cursor TEXT NOT NULL);
INSERT OR IGNORE INTO df_skill_discovery_experience_scan VALUES(1,'');
CREATE TABLE IF NOT EXISTS df_skill_discovery_days(day TEXT PRIMARY KEY,reservations INTEGER NOT NULL CHECK(reservations>=0));
CREATE TABLE IF NOT EXISTS df_skill_discovery_cooldowns(source_key TEXT NOT NULL,name TEXT NOT NULL,until_ns INTEGER NOT NULL,PRIMARY KEY(source_key,name));`)
	if err != nil {
		return err
	}
	return migrateDerivedDiscovery(ctx, tx)
}

// ConfigureDiscovery is a startup-only opt-in. Existing operator generation is
// kept independent; only this extra grant enables automatic paid maintenance.
func (s *Service) ConfigureDiscovery(options harness.SkillDiscoveryOptions) error {
	if s.discovery != nil || s.generation == nil || !validDiscoveryOptions(options) {
		return invalid("discovery requires configured generation and bounded options")
	}
	if options.ExperiencePatternsEnabled && (s.experienceSource == nil || !s.experienceSource.PromotionEnabled()) {
		return invalid("experience discovery requires explicitly enabled extraction, consolidation and matching")
	}
	if options.MissingReportsEnabled || options.SuccessfulWorkflowsEnabled || options.ExperienceNewSkillsEnabled {
		if _, ok := s.generation.allowed[options.NewSkillSourceID]; !ok {
			return invalid("new skill discovery source is not authorized for generation")
		}
		builtin, err := s.registry.BuiltinSources()
		if err != nil {
			return err
		}
		for _, id := range builtin {
			if id == options.NewSkillSourceID {
				return invalid("new skill discovery requires a non-builtin source")
			}
		}
	}
	s.discovery = &discoveryHost{options: options, now: time.Now}
	s.generation.discovery = s.discovery
	return nil
}

func discoveryRootKey(root string, target harness.SkillTarget, version string) string {
	return encodedHash([]string{"operator-corrections-v1", root, target.SourceIdentity, target.Name, version})
}

func readDiscovery(ctx context.Context, db rowReader, id string) (discoverySnapshot, string, string, error) {
	var d discoverySnapshot
	var source, name, job, digest string
	var payload []byte
	var created int64
	err := db.QueryRowContext(ctx, `SELECT source_key,name,job_id,payload,payload_sha,created_ns FROM df_skill_discoveries WHERE id=?`, id).Scan(&source, &name, &job, &payload, &digest, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return d, "", "", harness.ErrNotFound
	}
	if err != nil {
		return d, "", "", err
	}
	if len(payload) > 32<<10 || hash(payload) != digest || json.Unmarshal(payload, &d) != nil || d.ID != id || !strings.HasPrefix(id, "sd_") || len(id) > 128 || d.Target.SourceIdentity != source || d.Target.Name != name || !validDiscoveryOptions(d.Options) || d.CreatedAt.UnixNano() != created || !validOverlayBase(d.OverlayBase, d.Target) || !validDiscoverySnapshot(d) {
		return discoverySnapshot{}, "", "", invalid("discovery record is inconsistent")
	}
	roots := map[string]bool{}
	episodes := map[string]bool{}
	for _, ref := range d.Evidence {
		if ref.RootID == "" || roots[ref.RootID] || !validDiscoveryReference(d.kind(), ref) || d.EpisodeDedup && (!hexDigest(ref.EpisodeKey) || episodes[ref.EpisodeKey]) {
			return discoverySnapshot{}, "", "", invalid("discovery evidence record is inconsistent")
		}
		if d.kind() == "experience_pattern" && d.HostAdjudicated != strings.HasPrefix(ref.Experience.MatchID, "ladj_") {
			return discoverySnapshot{}, "", "", invalid("discovery target origin is inconsistent")
		}
		roots[ref.RootID] = true
		episodes[ref.EpisodeKey] = true
	}
	return d, job, digest, nil
}

// Read the current operator report and its actual reads. Neither a failed run
// nor a correction report is treated as a causal skill-quality determination.
func discoveryFeedback(ctx context.Context, tx *sql.Tx, id string, target harness.SkillTarget, version string) (harness.SkillSignal, harness.SkillFeedback, error) {
	signal, err := readSignal(ctx, tx, id, map[string]harness.SkillTarget{target.SourceIdentity: {SourceID: target.SourceID, SourceIdentity: target.SourceIdentity, Scope: target.Scope, Workspace: target.Workspace}})
	if err != nil {
		return signal, harness.SkillFeedback{}, err
	}
	if signal.Target != target || signal.Version != version || signal.FeedbackRevision == 0 || signal.CollectionPending || signal.CancelledRuns > 0 || signal.UncertainRuns > 0 {
		return signal, harness.SkillFeedback{}, errDiscoveryEvidence
	}
	var unsafe bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM budget_members m JOIN harness_runs r ON r.id=m.id LEFT JOIN harness_skill_outcomes o ON o.run_id=r.id
 WHERE m.root_id=? AND (r.status NOT IN ('completed','failed') OR o.sequence IS NULL OR o.processed<>1 OR o.effects_uncertain<>0))`, signal.RootID).Scan(&unsafe)
	if err != nil {
		return signal, harness.SkillFeedback{}, err
	}
	if unsafe {
		return signal, harness.SkillFeedback{}, errDiscoveryEvidence
	}
	f, err := readFeedback(ctx, tx, signal, signal.FeedbackRevision)
	if err != nil {
		return signal, f, err
	}
	confirmedFailure := f.Kind == "quality_failure" && signal.Quality.Eligible && signal.Quality.Confidence == "high" && signal.Quality.Kind == "skill_quality_failure"
	if (f.Kind != "correction" && !confirmedFailure) || f.SignalStateVersion != signal.StateVersion {
		return signal, f, errDiscoveryEvidence
	}
	for _, id := range f.ReadIDs {
		var found bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_signal_reads l JOIN harness_skill_reads r ON r.id=l.read_id JOIN harness_skill_outcomes o ON o.sequence=l.outcome_seq
 WHERE l.signal_id=? AND r.id=? AND r.root_id=? AND r.source_key=? AND r.source_id=? AND r.scope=? AND r.name=? AND r.version=? AND o.run_id=r.run_id AND o.session_id=r.session_id AND o.processed=1 AND o.effects_uncertain=0 AND o.status IN ('completed','failed'))`, signal.ID, id, signal.RootID, target.SourceIdentity, target.SourceID, target.Scope, target.Name, version).Scan(&found)
		if err != nil {
			return signal, f, err
		}
		if !found {
			return signal, f, errDiscoveryEvidence
		}
		var runID string
		if err = tx.QueryRowContext(ctx, `SELECT run_id FROM harness_skill_reads WHERE id=?`, id).Scan(&runID); err != nil {
			return signal, f, err
		}
		root, err := missingRoot(ctx, tx, signal.RootSessionID, runID)
		if err != nil {
			return signal, f, err
		}
		if root != signal.RootID {
			return signal, f, errDiscoveryEvidence
		}
	}
	return signal, f, nil
}

func (g *generationHost) discoveryEvidence(ctx context.Context, tx *sql.Tx, r generationRecord) ([]harness.SkillGenerationEvidence, error) {
	if r.input.DiscoveryID == "" {
		return nil, nil
	}
	if g.discovery == nil {
		return nil, errGenerationConfiguration
	}
	d, _, _, err := readDiscovery(ctx, tx, r.input.DiscoveryID)
	if err != nil {
		return nil, err
	}
	if d.Options != g.discovery.options {
		return nil, errGenerationConfiguration
	}
	return liveDiscoveryEvidence(ctx, tx, r, g.experienceSource)
}

func liveDiscoveryEvidence(ctx context.Context, tx *sql.Tx, r generationRecord, source *learning.Store) ([]harness.SkillGenerationEvidence, error) {
	d, job, digest, err := readDiscovery(ctx, tx, r.input.DiscoveryID)
	if err != nil {
		return nil, err
	}
	if job != r.job.ID || digest != r.input.DiscoveryDigest || d.Target != r.input.Target || d.Base != r.input.Request.Base || !skills.SameOverlayBase(d.OverlayBase, r.input.OverlayBase) {
		return nil, errDiscoveryEvidence
	}
	if (d.CreationRetry != nil) != (r.input.CreationRetryAuthorization != "") {
		return nil, errDiscoveryEvidence
	}
	if d.EpisodeDedup {
		if err = verifyDiscoveryEpisodesTx(ctx, tx, d); err != nil {
			return nil, err
		}
	}
	if d.kind() == "experience_pattern" {
		return liveExperienceDiscovery(ctx, tx, source, d, job)
	}
	if d.kind() != "operator_corrections" {
		return derivedDiscoveryEvidence(ctx, tx, d)
	}
	result := make([]harness.SkillGenerationEvidence, 0, len(d.Evidence))
	for _, ref := range d.Evidence {
		signal, f, err := discoveryFeedback(ctx, tx, ref.SignalID, d.Target, d.Base.Version)
		if discoverySkippable(err) {
			return nil, errDiscoveryEvidence
		}
		if err != nil {
			return nil, err
		}
		if signal.RootID != ref.RootID || signal.StateVersion != ref.SignalStateVersion || f.Revision != ref.FeedbackRevision || !slices.Equal(f.ReadIDs, ref.ReadIDs) {
			return nil, errDiscoveryEvidence
		}
		var consumed string
		if err = tx.QueryRowContext(ctx, `SELECT discovery_id FROM df_skill_discovery_consumed WHERE root_digest=?`, discoveryRootKey(ref.RootID, d.Target, d.Base.Version)).Scan(&consumed); err != nil {
			return nil, err
		}
		if consumed != d.ID {
			return nil, errDiscoveryEvidence
		}
		summary := "Operator correction; causality unassessed: " + f.Summary
		if f.Kind == "quality_failure" {
			summary = "Operator-confirmed skill quality failure; host environment exclusions checked: " + f.Summary
		}
		result = append(result, harness.SkillGenerationEvidence{Reference: ref.SignalID + "/feedback/" + strconv.FormatInt(ref.FeedbackRevision, 10), Summary: summary})
	}
	return result, nil
}

// Approval remains explicit and checks the live references again. Withdrawal,
// changed evidence or source revocation cannot publish a candidate merely
// because its model review completed before the change. Turning discovery off
// stops new paid work while preserving explicit approval of valid prior work.
func (s *Service) guardDiscoveryApproval(ctx context.Context, tx *skills.PublicationTx, p Proposal) error {
	var jobID string
	err := tx.SQL().QueryRowContext(ctx, `SELECT job_id FROM df_skill_generation_inputs WHERE proposal_id=?`, p.ID).Scan(&jobID)
	if errors.Is(err, sql.ErrNoRows) {
		if isDiscoveryTrigger(p.Trigger.Type) {
			return errDiscoveryEvidence
		}
		return nil
	}
	if err != nil {
		return err
	}
	r, err := readGeneration(ctx, tx.SQL(), jobID)
	if err != nil {
		return err
	}
	if r.input.DiscoveryID == "" {
		if isDiscoveryTrigger(p.Trigger.Type) {
			return errDiscoveryEvidence
		}
		return nil
	}
	if r.input.Target != p.Target || r.input.Request.Base != p.Base || r.candidate != p.CandidateVersion {
		return errDiscoveryEvidence
	}
	if s.experienceSource != nil {
		ready, err := s.experienceSource.PromotionReadyTx(ctx, tx.SQL(), jobID)
		if err != nil {
			return err
		}
		if !ready {
			return harness.ErrPermissionDenied
		}
	}
	if _, err = liveDiscoveryEvidence(ctx, tx.SQL(), r, s.experienceSource); err != nil {
		if discoverySkippable(err) {
			return errDiscoveryEvidence
		}
		return err
	}
	return s.guardExperienceCandidate(ctx, tx, r)
}
