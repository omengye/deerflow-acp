package evolution

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/learning"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

type generationHost struct {
	store                    *skillmaintenance.Store
	generator                skillmaintenance.Generator
	identity, reviewIdentity string
	allowed                  map[string]harness.SkillTarget
	workers                  int
	policy                   harness.SkillMaintenanceBudget
	lifecycle                collectorLifecycle
	discovery                *discoveryHost
	behavior                 *behaviorHost
	experienceSource         *learning.Store
}

var errGenerationConfiguration = errors.New("skill generation configuration changed")

func (s *Service) GenerationEnabled() bool { return s.generation != nil }

type generationSnapshot struct {
	Request                    harness.SkillGenerationRequest `json:"request"`
	Target                     harness.SkillTarget            `json:"target"`
	OverlayBase                *harness.SkillRef              `json:"overlay_base,omitempty"`
	GeneratorIdentity          string                         `json:"generator_identity"`
	ValidatorIdentity          string                         `json:"validator_identity"`
	DiscoveryID                string                         `json:"discovery_id,omitempty"`
	DiscoveryDigest            string                         `json:"discovery_digest,omitempty"`
	BehaviorIdentity           string                         `json:"behavior_identity,omitempty"`
	CreationRetryAuthorization string                         `json:"creation_retry_authorization,omitempty"`
}

type generationRecord struct {
	job                             skillmaintenance.Job
	input                           generationSnapshot
	id, candidate, reason, proposal string
	created                         time.Time
}

func migrateGeneration(ctx context.Context, tx *sql.Tx) error {
	if err := skillmaintenance.Migrate(ctx, tx); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS df_skill_generation_inputs (
 id TEXT PRIMARY KEY,job_id TEXT NOT NULL UNIQUE REFERENCES df_skill_maintenance_jobs(id),
 source_key TEXT NOT NULL,name TEXT NOT NULL,request_key TEXT NOT NULL,request_sha TEXT NOT NULL,payload BLOB NOT NULL,
 candidate_version TEXT NOT NULL DEFAULT '',candidate_reason TEXT NOT NULL DEFAULT '',proposal_id TEXT NOT NULL DEFAULT '',progress_sha TEXT NOT NULL,
 created_at TEXT NOT NULL,UNIQUE(source_key,name,request_key));
CREATE TABLE IF NOT EXISTS df_skill_generation_catalog(id INTEGER PRIMARY KEY CHECK(id=1),revision INTEGER NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS df_skill_generation_proposal ON df_skill_generation_inputs(proposal_id) WHERE proposal_id<>'';
INSERT OR IGNORE INTO df_skill_generation_catalog VALUES(1,0);
CREATE TRIGGER IF NOT EXISTS df_skill_generation_insert AFTER INSERT ON df_skill_generation_inputs BEGIN UPDATE df_skill_generation_catalog SET revision=revision+1 WHERE id=1; END;
CREATE TRIGGER IF NOT EXISTS df_skill_generation_update AFTER UPDATE ON df_skill_generation_inputs BEGIN UPDATE df_skill_generation_catalog SET revision=revision+1 WHERE id=1; END;
DROP TRIGGER IF EXISTS df_skill_generation_job_update;
CREATE TRIGGER df_skill_generation_job_update AFTER UPDATE ON df_skill_maintenance_jobs WHEN NEW.owner_kind='skill' BEGIN UPDATE df_skill_generation_catalog SET revision=revision+1 WHERE id=1; END;`)
	return err
}

// ConfigureGeneration is startup-only. All dependencies and source grants are
// fixed for this service lifetime; disabled hosts still expose stored history.
func (s *Service) ConfigureGeneration(ctx context.Context, generator skillmaintenance.Generator, policy skillmaintenance.Policy, sources []string, allowBuiltin bool) error {
	if s.generation != nil || generator == nil || !bounded(generator.Identity(), 128) || generator.Identity() == "" || len(sources) == 0 || len(sources) > 32 {
		return invalid("generation requires explicit sources and a versioned generator")
	}
	validator, ok := s.validator.(harness.SkillVersionedValidator)
	if !ok || validator.Identity() == "" || !bounded(validator.Identity(), 128) {
		return invalid("generation requires a versioned validator")
	}
	configured, err := s.registry.PublicationSources()
	if err != nil {
		return err
	}
	builtin, err := s.registry.BuiltinSources()
	if err != nil {
		return err
	}
	g := &generationHost{generator: generator, identity: generator.Identity(), reviewIdentity: validator.Identity(), allowed: map[string]harness.SkillTarget{}, workers: policy.Workers, policy: policy}
	for _, id := range sources {
		if _, duplicate := g.allowed[id]; duplicate {
			return invalid("duplicate generation source")
		}
		found := false
		for _, target := range configured {
			if target.SourceID == id {
				g.allowed[id] = target
				found = true
				break
			}
		}
		if !found || !allowBuiltin && slices.Contains(builtin, id) {
			return invalid("generation source is unavailable or not authorized")
		}
	}
	g.store, err = skillmaintenance.NewWithHooks(ctx, s.db, policy, skillmaintenance.Hooks{Admission: s.admitMaintenance, Ready: s.readyMaintenance, Terminal: s.finalizeMaintenanceTx, CallAdmission: s.admitMaintenanceCall})
	if err != nil {
		return err
	}
	s.generation = g
	return nil
}

func generationRequestHash(r harness.SkillGenerationRequest, t harness.SkillTarget) string {
	return encodedHash([]any{1, r, t})
}
func generationProgress(r generationRecord) string {
	return encodedHash([]string{r.candidate, r.reason, r.proposal})
}

func readGeneration(ctx context.Context, db rowReader, jobID string) (generationRecord, error) {
	var r generationRecord
	var source, name, key, requestSHA, progress, created string
	var payload []byte
	job, err := skillmaintenance.ReadJob(ctx, db, jobID)
	if err != nil {
		return r, err
	}
	r.job = job
	if job.Request.OwnerKind != skillmaintenance.SkillOwner || job.Request.Kind != skillmaintenance.SkillGeneration && job.Request.Kind != skillmaintenance.LegacyJob {
		return r, invalid("generation producer is inconsistent")
	}
	err = db.QueryRowContext(ctx, `SELECT id,source_key,name,request_key,request_sha,payload,candidate_version,candidate_reason,proposal_id,progress_sha,created_at FROM df_skill_generation_inputs WHERE job_id=?`, jobID).
		Scan(&r.id, &source, &name, &key, &requestSHA, &payload, &r.candidate, &r.reason, &r.proposal, &progress, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return r, harness.ErrNotFound
	}
	if err != nil {
		return r, err
	}
	if len(payload) > 32<<10 || json.Unmarshal(payload, &r.input) != nil || hash(payload) != job.Request.InputDigest || r.id != job.Request.Reference || source != job.Request.SourceIdentity || name != job.Request.Name || key != job.Request.Key ||
		r.input.Target.SourceIdentity != source || r.input.Target.Name != name || r.input.Request.Name != name || r.input.Request.SourceID != r.input.Target.SourceID || r.input.Request.RequestKey != key || generationRequestHash(r.input.Request, r.input.Target) != requestSHA || generationProgress(r) != progress ||
		!validOverlayBase(r.input.OverlayBase, r.input.Target) || r.candidate != "" && !hexDigest(r.candidate) || !bounded(r.reason, 4000) || r.candidate == "" && (r.reason != "" || r.proposal != "") {
		return r, invalid("generation record is inconsistent")
	}
	if (r.input.DiscoveryID == "") != (r.input.DiscoveryDigest == "") || r.input.DiscoveryID != "" && (!strings.HasPrefix(r.input.DiscoveryID, "sd_") || len(r.input.DiscoveryID) > 128 || !hexDigest(r.input.DiscoveryDigest)) {
		return r, invalid("generation discovery identity is inconsistent")
	}
	if r.input.CreationRetryAuthorization != "" && (r.input.DiscoveryID == "" || !hexDigest(r.input.CreationRetryAuthorization)) {
		return r, invalid("creation retry authorization is inconsistent")
	}
	r.created, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return r, invalid("generation timestamp is inconsistent")
	}
	return r, nil
}

func (g *generationHost) checkConfiguration(r generationRecord) error {
	if err := g.checkSource(r); err != nil {
		return err
	}
	if r.input.GeneratorIdentity != g.identity || r.input.ValidatorIdentity != g.reviewIdentity {
		return errGenerationConfiguration
	}
	if r.input.BehaviorIdentity != g.behavior.bindingIdentity(r.input.Target) {
		return errGenerationConfiguration
	}
	return nil
}

func (g *generationHost) checkSource(r generationRecord) error {
	target, ok := g.allowed[r.input.Target.SourceID]
	target.Name = r.input.Target.Name
	if !ok || target != r.input.Target {
		return harness.ErrPermissionDenied
	}
	return nil
}

func (g *generationHost) admit(ctx context.Context, tx *sql.Tx, job skillmaintenance.Job) error {
	r, err := readGeneration(ctx, tx, job.ID)
	if err != nil {
		return err
	}
	if err = g.checkConfiguration(r); err != nil {
		return err
	}
	head, err := skills.ReadPublicationHead(ctx, tx, r.input.Target.SourceIdentity, r.input.Target.Name)
	if err != nil {
		return err
	}
	if head != r.input.Request.Base {
		return harness.ErrSkillConflict
	}
	if err = checkMaintenanceOverlay(ctx, tx, r.input.Target, r.input.OverlayBase); err != nil {
		return err
	}
	// A cancelled/rejected proposal must not admit the next review stage.
	if r.proposal != "" {
		p, err := readProposal(ctx, tx, r.proposal)
		if err != nil {
			return err
		}
		if p.Status != "validating" {
			return harness.ErrSkillConflict
		}
	}
	_, err = g.discoveryEvidence(ctx, tx, r)
	return err
}

func (s *Service) EnqueueGeneration(ctx context.Context, request harness.SkillGenerationRequest) (harness.SkillGenerationJob, error) {
	if s.generation == nil {
		return harness.SkillGenerationJob{}, invalid("generation is not configured")
	}
	if request.Author == "" {
		request.Author = "operator"
	}
	if !harness.ValidSkillName(request.SourceID) || !harness.ValidSkillName(request.Name) || strings.TrimSpace(request.Reason) == "" || !bounded(request.Reason, 4000) || request.RequestKey == "" || !bounded(request.RequestKey, 256) || !bounded(request.Author, 128) {
		return harness.SkillGenerationJob{}, invalid("generation request is invalid")
	}
	var result harness.SkillGenerationJob
	err := s.registry.WithPublication(ctx, request.SourceID, request.Name, func(tx *skills.PublicationTx) error {
		snapshot := generationSnapshot{Request: request, Target: tx.Target(), GeneratorIdentity: s.generation.identity, ValidatorIdentity: s.generation.reviewIdentity, BehaviorIdentity: s.behavior.bindingIdentity(tx.Target())}
		if err := s.generation.checkConfiguration(generationRecord{input: snapshot}); err != nil {
			return err
		}
		sha := generationRequestHash(request, tx.Target())
		var prior, priorSHA string
		err := tx.SQL().QueryRowContext(ctx, `SELECT job_id,request_sha FROM df_skill_generation_inputs WHERE source_key=? AND name=? AND request_key=?`, tx.Target().SourceIdentity, request.Name, request.RequestKey).Scan(&prior, &priorSHA)
		if err == nil {
			if sha != priorSHA {
				return harness.ErrSkillConflict
			}
			r, err := readGeneration(ctx, tx.SQL(), prior)
			if err != nil {
				return err
			}
			result = generationView(r)
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		head, err := tx.Current()
		if err != nil {
			return err
		}
		if head != request.Base {
			return harness.ErrSkillConflict
		}
		snapshot.OverlayBase, err = tx.OverlayBase()
		if err != nil {
			return err
		}
		payload, err := json.Marshal(snapshot)
		if err != nil {
			return err
		}
		r := generationRecord{id: "sgi_" + rand.Text(), input: snapshot, created: time.Now().UTC()}
		r.job, err = s.generation.store.EnqueueTx(ctx, tx.SQL(), skillmaintenance.Request{SourceIdentity: tx.Target().SourceIdentity, Name: request.Name, Key: request.RequestKey, Reference: r.id, InputDigest: hash(payload)})
		if err != nil {
			return err
		}
		_, err = tx.SQL().ExecContext(ctx, `INSERT INTO df_skill_generation_inputs(id,job_id,source_key,name,request_key,request_sha,payload,progress_sha,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, r.id, r.job.ID, tx.Target().SourceIdentity, request.Name, request.RequestKey, sha, payload, generationProgress(r), r.created.Format(time.RFC3339Nano))
		if err == nil {
			result = generationView(r)
		}
		return err
	})
	return result, err
}

func generationView(r generationRecord) harness.SkillGenerationJob {
	phase := "generating"
	if r.candidate != "" {
		phase = "validating"
	}
	if r.job.State == "completed" {
		phase = "review_complete"
	}
	return harness.SkillGenerationJob{ID: r.job.ID, DiscoveryID: r.input.DiscoveryID, Target: r.input.Target, Base: r.input.Request.Base, State: r.job.State, Phase: phase, StateVersion: r.job.StateVersion, Attempts: r.job.Attempts, Deadline: r.job.Deadline, RetryAt: r.job.RetryAt, ErrorCode: r.job.ErrorCode, ProposalID: r.proposal, CandidateVersion: r.candidate, CreatedAt: r.created}
}

func (s *Service) withGeneration(ctx context.Context, id string, fn func(*skills.PublicationTx, generationRecord) error) error {
	if !strings.HasPrefix(id, "smj_") || !bounded(id, 128) {
		return invalid("generation id is invalid")
	}
	r, err := readGeneration(ctx, s.db, id)
	if err != nil {
		return err
	}
	return s.registry.WithPublication(ctx, r.input.Target.SourceID, r.input.Target.Name, func(tx *skills.PublicationTx) error {
		fresh, err := readGeneration(ctx, tx.SQL(), id)
		if err != nil {
			return err
		}
		if tx.Target() != fresh.input.Target || fresh.input.Target != r.input.Target {
			return harness.ErrPermissionDenied
		}
		return fn(tx, fresh)
	})
}

func (s *Service) GetGeneration(ctx context.Context, id string) (harness.SkillGenerationJob, error) {
	var result harness.SkillGenerationJob
	err := s.withGeneration(ctx, id, func(_ *skills.PublicationTx, r generationRecord) error { result = generationView(r); return nil })
	return result, err
}
