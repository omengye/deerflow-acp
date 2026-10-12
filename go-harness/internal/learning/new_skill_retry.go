package learning

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
)

const NewSkillRetryPolicyVersion = "procedural-host-creation-retry-v1"
const MaxNewSkillRetries = 3

// The binding is supplied only by the publication transaction that creates the
// fresh generation/discovery snapshots. No old snapshot or consumption changes.
type NewSkillRetryBinding struct {
	GenerationID, GenerationDigest string
	DiscoveryID, DiscoveryDigest   string
	AutomaticGenerationID          string
	Reference                      harness.SkillLearningNewSkillRetryReference
	CreatedAt                      time.Time
	AuthorizationDigest            string
	// Generation snapshots contain only fixed host instructions/configuration
	// and references, never the learned procedure. Retain this original proof
	// when ignore removes unpublished producer rows, without reactivating them.
	GenerationSnapshot json.RawMessage
}

type newSkillRetrySnapshot struct {
	Actor   string                                    `json:"actor"`
	Request harness.SkillLearningNewSkillRetryRequest `json:"request"`
	Review  harness.SkillLearningPatternReview        `json:"review"`
	Binding NewSkillRetryBinding                      `json:"binding"`
}

func NewSkillRetryAuthorization(actor string, request harness.SkillLearningNewSkillRetryRequest, review harness.SkillLearningPatternReview, binding NewSkillRetryBinding) string {
	// Generation contains this digest; omit its ID/input digest to avoid a
	// circular hash while still pinning every host authorization/result field.
	binding.GenerationID, binding.GenerationDigest, binding.AuthorizationDigest = "", "", ""
	binding.GenerationSnapshot = nil
	return encodedDigest([]any{NewSkillRetryPolicyVersion, actor, request, review, binding})
}

func validNewSkillRetryRequest(r harness.SkillLearningNewSkillRetryRequest) bool {
	return validReviewRequest(r.PatternID, r.Scope, r.RequestKey, r.ExpectedPatternRevision, r.ExpectedStateVersion) &&
		strings.HasPrefix(r.PreviousGenerationID, "smj_") && validText(r.PreviousGenerationID, 128, true) &&
		r.ExpectedGenerationStateVersion > 0 && validText(r.PreviousProposalID, 128, false) && r.ExpectedProposalStateVersion >= 0 &&
		(r.PreviousProposalID == "") == (r.ExpectedProposalStateVersion == 0) && harness.ValidSkillCapabilityName(r.Target.Name) &&
		r.ExpectedHead == (harness.SkillHead{}) && r.OverlayBase == nil && r.ExpectedRetryNumber >= 1 && r.ExpectedRetryNumber <= MaxNewSkillRetries &&
		strings.HasPrefix(r.EvidenceReservationID, "sd_") && validText(r.EvidenceReservationID, 128, true)
}

func (s *Store) NewSkillRetryScopeTx(ctx context.Context, tx *sql.Tx, actor string, r harness.SkillLearningNewSkillRetryPreparationRequest) (harness.SkillLearningPatternReview, error) {
	if !validReviewRequest(r.PatternID, r.Scope, "prepare", r.ExpectedPatternRevision, r.ExpectedStateVersion) ||
		!strings.HasPrefix(r.PreviousGenerationID, "smj_") || !validText(r.PreviousGenerationID, 128, true) {
		return harness.SkillLearningPatternReview{}, harness.ErrInvalidInput
	}
	if _, err := s.reviewScopeTx(ctx, tx, actor, r.PatternID, r.Scope, r.ExpectedPatternRevision); err != nil {
		return harness.SkillLearningPatternReview{}, err
	}
	review, err := patternReviewTx(ctx, tx, r.PatternID)
	if err == nil && (!s.PromotionEnabled() || review.Ignored) {
		err = harness.ErrPermissionDenied
	}
	if err == nil && (review.PatternRevision != r.ExpectedPatternRevision || review.StateVersion != r.ExpectedStateVersion) {
		err = harness.ErrSkillConflict
	}
	return review, err
}

func retryPreparationRequest(r harness.SkillLearningNewSkillRetryRequest) harness.SkillLearningNewSkillRetryPreparationRequest {
	return harness.SkillLearningNewSkillRetryPreparationRequest{PatternID: r.PatternID, ExpectedPatternRevision: r.ExpectedPatternRevision,
		ExpectedStateVersion: r.ExpectedStateVersion, Scope: r.Scope, PreviousGenerationID: r.PreviousGenerationID}
}

// A retained receipt is the original outcome only. Scope is still checked, but
// newer revisions, disabled paid work or a completed successor do not reexecute it.
func (s *Store) NewSkillRetryReceiptTx(ctx context.Context, tx *sql.Tx, actor string, r harness.SkillLearningNewSkillRetryRequest) (*harness.SkillLearningNewSkillRetryResult, error) {
	if !validNewSkillRetryRequest(r) {
		return nil, harness.ErrInvalidInput
	}
	if _, err := s.reviewScopeTx(ctx, tx, actor, r.PatternID, r.Scope, r.ExpectedPatternRevision); err != nil {
		return nil, err
	}
	var id string
	var data []byte
	var sha string
	err := tx.QueryRowContext(ctx, `SELECT id,payload,payload_sha FROM df_learning_new_skill_retries WHERE session_id=? AND request_key=?`, actor, r.RequestKey).Scan(&id, &data, &sha)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	snap, err := readNewSkillRetry(data, sha, id)
	if err != nil {
		return nil, err
	}
	if snap.Actor != actor || encodedDigest(snap.Request) != encodedDigest(r) {
		return nil, harness.ErrSkillConflict
	}
	if _, err = s.newSkillRetryTx(ctx, tx, r.PatternID, snap.Binding.GenerationID, false); err != nil {
		return nil, err
	}
	result := newSkillRetryResult(id, snap)
	return &result, nil
}

func (s *Store) NewSkillRetryReceipt(ctx context.Context, actor string, request harness.SkillLearningNewSkillRetryRequest) (*harness.SkillLearningNewSkillRetryResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	return s.NewSkillRetryReceiptTx(ctx, tx, actor, request)
}

func readNewSkillRetry(data []byte, sha, id string) (newSkillRetrySnapshot, error) {
	var snap newSkillRetrySnapshot
	if len(data) > 40<<10 || digest(data) != sha || json.Unmarshal(data, &snap) != nil ||
		"lretry_"+encodedDigest([]any{NewSkillRetryPolicyVersion, snap}) != id || !validText(snap.Actor, 256, true) || !validNewSkillRetryRequest(snap.Request) ||
		snap.Review.PatternID != snap.Request.PatternID || snap.Review.PatternRevision != snap.Request.ExpectedPatternRevision ||
		snap.Review.StateVersion != snap.Request.ExpectedStateVersion+1 || snap.Review.Ignored ||
		snap.Binding.Reference.PreviousGenerationID != snap.Request.PreviousGenerationID || snap.Binding.Reference.RetryNumber < 1 || snap.Binding.Reference.RetryNumber > MaxNewSkillRetries ||
		snap.Binding.Reference.RetryNumber != snap.Request.ExpectedRetryNumber || snap.Binding.Reference.EvidenceReservationID != snap.Request.EvidenceReservationID ||
		!strings.HasPrefix(snap.Binding.Reference.EvidenceReservationID, "sd_") || !validText(snap.Binding.Reference.EvidenceReservationID, 128, true) ||
		!strings.HasPrefix(snap.Binding.AutomaticGenerationID, "smj_") || !validText(snap.Binding.AutomaticGenerationID, 128, true) ||
		!strings.HasPrefix(snap.Binding.GenerationID, "smj_") || !validText(snap.Binding.GenerationID, 128, true) ||
		!strings.HasPrefix(snap.Binding.DiscoveryID, "sd_") || !validText(snap.Binding.DiscoveryID, 128, true) ||
		snap.Binding.DiscoveryID == snap.Binding.Reference.EvidenceReservationID || snap.Binding.GenerationID == snap.Request.PreviousGenerationID ||
		len(snap.Binding.GenerationDigest) != 64 || len(snap.Binding.DiscoveryDigest) != 64 || snap.Binding.CreatedAt.IsZero() {
		return snap, invalid("inconsistent creation retry authorization")
	}
	if snap.Binding.AuthorizationDigest != NewSkillRetryAuthorization(snap.Actor, snap.Request, snap.Review, snap.Binding) {
		return snap, invalid("creation retry authorization digest changed")
	}
	var generation struct {
		Target          harness.SkillTarget            `json:"target"`
		Request         harness.SkillGenerationRequest `json:"request"`
		DiscoveryID     string                         `json:"discovery_id"`
		DiscoveryDigest string                         `json:"discovery_digest"`
		Authorization   string                         `json:"creation_retry_authorization"`
	}
	if len(snap.Binding.GenerationSnapshot) > 32<<10 || digest(snap.Binding.GenerationSnapshot) != snap.Binding.GenerationDigest ||
		json.Unmarshal(snap.Binding.GenerationSnapshot, &generation) != nil || generation.Authorization != snap.Binding.AuthorizationDigest ||
		generation.Target != snap.Request.Target || generation.Request.Base != (harness.SkillHead{}) ||
		generation.Request.Name != snap.Request.Target.Name || generation.Request.SourceID != snap.Request.Target.SourceID ||
		generation.Request.RequestKey != "creation-retry:"+snap.Binding.DiscoveryID ||
		generation.Request.Author != "host_creation_retry" || generation.DiscoveryID != snap.Binding.DiscoveryID || generation.DiscoveryDigest != snap.Binding.DiscoveryDigest {
		return snap, invalid("creation retry differs from immutable generation authorization")
	}
	return snap, nil
}

func newSkillRetryResult(id string, snap newSkillRetrySnapshot) harness.SkillLearningNewSkillRetryResult {
	return harness.SkillLearningNewSkillRetryResult{ID: id, Review: snap.Review, GenerationID: snap.Binding.GenerationID, DiscoveryID: snap.Binding.DiscoveryID, Retry: snap.Binding.Reference}
}

func (s *Store) NewSkillRetryOrdinalTx(ctx context.Context, tx *sql.Tx, pattern, previous string) (int, error) {
	var count int
	var used bool
	err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM df_learning_new_skill_retry_spent WHERE digest=?),
 EXISTS(SELECT 1 FROM df_learning_new_skill_retry_spent WHERE previous_digest=?)`,
		encodedDigest([]string{"procedural-new-skill-reservation-v1", pattern}), encodedDigest([]string{NewSkillRetryPolicyVersion, previous})).Scan(&count, &used)
	if err == nil && (used || count >= MaxNewSkillRetries) {
		err = harness.ErrSkillConflict
	}
	return count + 1, err
}

func (s *Store) ReserveNewSkillRetryTx(ctx context.Context, tx *sql.Tx, actor string, r harness.SkillLearningNewSkillRetryRequest, binding NewSkillRetryBinding) (harness.SkillLearningNewSkillRetryResult, error) {
	var result harness.SkillLearningNewSkillRetryResult
	if !validNewSkillRetryRequest(r) {
		return result, harness.ErrInvalidInput
	}
	review, err := s.NewSkillRetryScopeTx(ctx, tx, actor, retryPreparationRequest(r))
	if err != nil {
		return result, err
	}
	ordinal, err := s.NewSkillRetryOrdinalTx(ctx, tx, r.PatternID, r.PreviousGenerationID)
	if err != nil || binding.Reference.RetryNumber != ordinal {
		return result, errors.Join(err, harness.ErrSkillConflict)
	}
	var automatic string
	if err = tx.QueryRowContext(ctx, `SELECT job_id FROM df_learning_new_skill_consumed WHERE digest=?`, encodedDigest([]string{"procedural-new-skill-reservation-v1", r.PatternID})).Scan(&automatic); err != nil {
		return result, err
	}
	if automatic != binding.AutomaticGenerationID {
		return result, harness.ErrPermissionDenied
	}
	review.StateVersion++
	snap := newSkillRetrySnapshot{Actor: actor, Request: r, Review: review, Binding: binding}
	data, err := json.Marshal(snap)
	if err != nil {
		return result, err
	}
	id := "lretry_" + encodedDigest([]any{NewSkillRetryPolicyVersion, snap})
	if _, err = readNewSkillRetry(data, digest(data), id); err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO df_learning_new_skill_retry_spent VALUES(?,?,?,?)`, encodedDigest([]string{"procedural-new-skill-reservation-v1", r.PatternID}), ordinal, binding.GenerationID, encodedDigest([]string{NewSkillRetryPolicyVersion, r.PreviousGenerationID})); err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO df_learning_new_skill_retries VALUES(?,?,?,?,?,?,?)`, id, actor, r.PatternID, binding.GenerationID, r.RequestKey, data, digest(data)); err != nil {
		return result, err
	}
	if err = writePatternReviewTx(ctx, tx, review); err != nil {
		return result, err
	}
	return newSkillRetryResult(id, snap), nil
}

func (s *Store) newSkillRetryTx(ctx context.Context, tx *sql.Tx, pattern, job string, live bool) (newSkillRetrySnapshot, error) {
	var id, actor, storedPattern, key, sha string
	var data []byte
	err := tx.QueryRowContext(ctx, `SELECT id,session_id,pattern_id,request_key,payload,payload_sha FROM df_learning_new_skill_retries WHERE job_id=?`, job).Scan(&id, &actor, &storedPattern, &key, &data, &sha)
	if errors.Is(err, sql.ErrNoRows) {
		err = harness.ErrPermissionDenied
	}
	if err != nil {
		return newSkillRetrySnapshot{}, err
	}
	snap, err := readNewSkillRetry(data, sha, id)
	if err != nil {
		return snap, err
	}
	if snap.Actor != actor || snap.Request.PatternID != pattern || storedPattern != pattern || snap.Request.RequestKey != key || snap.Binding.GenerationID != job {
		return snap, invalid("creation retry binding changed")
	}
	var valid bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_learning_new_skill_consumed WHERE digest=? AND job_id=?)
 AND EXISTS(SELECT 1 FROM df_learning_new_skill_retry_spent WHERE digest=? AND ordinal=? AND job_id=? AND previous_digest=?)`,
		encodedDigest([]string{"procedural-new-skill-reservation-v1", pattern}), snap.Binding.AutomaticGenerationID,
		encodedDigest([]string{"procedural-new-skill-reservation-v1", pattern}), snap.Binding.Reference.RetryNumber, job,
		encodedDigest([]string{NewSkillRetryPolicyVersion, snap.Request.PreviousGenerationID})).Scan(&valid)
	if err != nil {
		return snap, err
	}
	if !valid {
		return snap, harness.ErrPermissionDenied
	}
	j, err := skillmaintenance.ReadJob(ctx, tx, job)
	if err != nil {
		return snap, err
	}
	if j.Request.InputDigest != snap.Binding.GenerationDigest || j.Request.SourceIdentity != snap.Request.Target.SourceIdentity || j.Request.Name != snap.Request.Target.Name ||
		j.Request.OwnerKind != skillmaintenance.SkillOwner || j.Request.Kind != skillmaintenance.SkillGeneration && j.Request.Kind != skillmaintenance.LegacyJob ||
		j.Request.Key != "creation-retry:"+snap.Binding.DiscoveryID {
		return snap, invalid("creation retry generation changed")
	}
	if live {
		review, err := s.NewSkillRetryScopeTx(ctx, tx, actor, harness.SkillLearningNewSkillRetryPreparationRequest{
			PatternID: pattern, ExpectedPatternRevision: snap.Review.PatternRevision, ExpectedStateVersion: snap.Review.StateVersion,
			Scope: snap.Request.Scope, PreviousGenerationID: snap.Request.PreviousGenerationID})
		if err != nil {
			return snap, err
		}
		if review != snap.Review {
			return snap, harness.ErrSkillConflict
		}
	}
	return snap, nil
}

func (s *Store) NewSkillRetryBindingTx(ctx context.Context, tx *sql.Tx, pattern, job string) (NewSkillRetryBinding, error) {
	snap, err := s.newSkillRetryTx(ctx, tx, pattern, job, true)
	return snap.Binding, err
}

func (s *Store) NewSkillRetryRecordTx(ctx context.Context, tx *sql.Tx, pattern, job string) (NewSkillRetryBinding, error) {
	snap, err := s.newSkillRetryTx(ctx, tx, pattern, job, false)
	return snap.Binding, err
}
