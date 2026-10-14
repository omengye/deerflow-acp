package evolution

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/learning"
	"github.com/omengye/deerflow-acp/go-harness/internal/skillmaintenance"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

type Service struct {
	registry             *skills.Registry
	db                   *sql.DB
	validator            Validator
	collector            collectorLifecycle
	generation           *generationHost
	discovery            *discoveryHost
	behavior             *behaviorHost
	autoPatch            *autoPatchHost
	observationMonitor   collectorLifecycle
	experienceSource     *learning.Store
	maintenanceProducers map[skillmaintenance.JobKind]maintenanceProducer
}

func New(ctx context.Context, registry *skills.Registry, validator Validator) (*Service, error) {
	if registry == nil {
		return nil, invalid("registry is required")
	}
	s := &Service{registry: registry, db: registry.PublicationStore(), validator: validator}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS df_skill_proposals (
 id TEXT PRIMARY KEY,source_key TEXT NOT NULL,name TEXT NOT NULL,
 request_key TEXT NOT NULL,request_sha TEXT NOT NULL,
 state_version INTEGER NOT NULL,status TEXT NOT NULL,payload BLOB NOT NULL,
 UNIQUE(source_key,name,request_key));
CREATE INDEX IF NOT EXISTS df_skill_proposals_status ON df_skill_proposals(status,id);
CREATE TABLE IF NOT EXISTS df_skill_publications (
 source_key TEXT NOT NULL,name TEXT NOT NULL,revision INTEGER NOT NULL,
 request_key TEXT NOT NULL,request_sha TEXT NOT NULL,payload BLOB NOT NULL,
 PRIMARY KEY(source_key,name,revision),UNIQUE(source_key,name,request_key));`)
	if err != nil {
		return nil, err
	}
	if err = migrateCollector(ctx, tx); err != nil {
		return nil, err
	}
	if err = migrateFeedback(ctx, tx); err != nil {
		return nil, err
	}
	if err = migrateMissingReports(ctx, tx); err != nil {
		return nil, err
	}
	if err = migrateGeneration(ctx, tx); err != nil {
		return nil, err
	}
	if err = migrateDiscovery(ctx, tx); err != nil {
		return nil, err
	}
	if err = migrateBehavior(ctx, tx); err != nil {
		return nil, err
	}
	if err = migrateAutoPatch(ctx, tx); err != nil {
		return nil, err
	}
	if err = bindLegacyMaintenance(ctx, tx); err != nil {
		return nil, err
	}
	return s, tx.Commit()
}

func invalid(message string) error {
	return fmt.Errorf("%w: skill evolution %s", harness.ErrInvalidInput, message)
}
func hash(data []byte) string      { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }
func encodedHash(value any) string { data, _ := json.Marshal(value); return hash(data) }
func newID() string                { return "p_" + rand.Text() }
func bounded(value string, maximum int) bool {
	return len(value) <= maximum && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

// Bind the stored review target to the original idempotent request without
// persisting a second copy of candidate files. The content version already
// commits their exact names, lengths and hashes.
func requestHash(r Request, version string) string {
	return encodedHash(struct {
		Schema                                                     int
		SourceID, Name, RequestKey, Action, Reason, Author, Origin string
		Base                                                       harness.SkillHead
		Trigger                                                    Trigger
		Version                                                    string
	}{1, r.SourceID, r.Name, r.RequestKey, r.Action, r.Reason, r.Author, r.Origin, r.Base, r.Trigger, version})
}

type rowReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readProposal(ctx context.Context, db rowReader, id string) (Proposal, error) {
	var p Proposal
	var source, name, status, requestKey, requestSHA string
	var version int64
	var data []byte
	err := db.QueryRowContext(ctx, `SELECT source_key,name,state_version,status,payload,request_key,request_sha FROM df_skill_proposals WHERE id=?`, id).Scan(&source, &name, &version, &status, &data, &requestKey, &requestSHA)
	if errors.Is(err, sql.ErrNoRows) {
		return p, harness.ErrNotFound
	}
	if err != nil {
		return p, err
	}
	if json.Unmarshal(data, &p) != nil || p.ID != id || p.Target.SourceIdentity != source || p.Target.Name != name || p.SkillName != name || p.Status != status || p.StateVersion != version || version < 1 {
		return p, errors.New("skill proposal record is inconsistent")
	}
	if !validOverlayBase(p.OverlayBase, p.Target) {
		return Proposal{}, errors.New("skill proposal overlay identity is inconsistent")
	}
	intent := Request{SourceID: p.Target.SourceID, Name: p.SkillName, RequestKey: requestKey, Action: p.Action, Reason: p.Reason, Author: p.Author, Origin: p.Origin, Base: p.Base, Trigger: p.Trigger}
	if requestHash(intent, p.CandidateVersion) != requestSHA || !validStatus(p.Status) || p.Status == "" || !validAction(p.Action) || (p.Action == "delete") != (p.CandidateVersion == "") {
		return Proposal{}, errors.New("skill proposal intent is inconsistent")
	}
	if p.EvaluationDigest != "" {
		if p.EvaluationDigest != encodedHash(p.Evaluation) {
			return Proposal{}, errors.New("skill evaluation digest is inconsistent")
		}
	} else if encodedHash(p.Evaluation) != encodedHash(Report{}) || (p.Status != "validating" && p.Status != "rejected") {
		return Proposal{}, errors.New("skill evaluation is missing its digest")
	}
	return p, nil
}

func validOverlayBase(ref *harness.SkillRef, target harness.SkillTarget) bool {
	if ref == nil {
		return true
	}
	decoded, err := hex.DecodeString(ref.Version)
	return err == nil && len(decoded) == sha256.Size && ref.Hash == ref.Version && ref.SourceID == target.SourceID && ref.SourceIdentity == target.SourceIdentity && ref.Name == target.Name
}

func updateProposal(ctx context.Context, tx *sql.Tx, p Proposal, previous int64) error {
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if len(data) > 3<<20 {
		return invalid("encoded proposal exceeds management bounds")
	}
	result, err := tx.ExecContext(ctx, `UPDATE df_skill_proposals SET state_version=?,status=?,payload=? WHERE id=? AND state_version=? AND source_key=? AND name=?`, p.StateVersion, p.Status, data, p.ID, previous, p.Target.SourceIdentity, p.SkillName)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return ErrConflict
	}
	return err
}

func (s *Service) withProposal(ctx context.Context, id string, fn func(*skills.PublicationTx, *Proposal) error) error {
	if !strings.HasPrefix(id, "p_") || !bounded(id, 131) {
		return invalid("proposal id is invalid")
	}
	p, err := readProposal(ctx, s.db, id)
	if err != nil {
		return err
	}
	return s.registry.WithPublication(ctx, p.Target.SourceID, p.SkillName, func(tx *skills.PublicationTx) error {
		fresh, err := readProposal(ctx, tx.SQL(), id)
		if err != nil {
			return err
		}
		if tx.Target() != fresh.Target || fresh.Target != p.Target {
			return harness.ErrPermissionDenied
		}
		return fn(tx, &fresh)
	})
}

// Get is an operator-level API. Model tools must additionally bind the target
// to their host-supplied session scope and proposal authorship.
func (s *Service) Get(ctx context.Context, id string) (Proposal, error) {
	var p Proposal
	err := s.withProposal(ctx, id, func(_ *skills.PublicationTx, saved *Proposal) error { p = *saved; return nil })
	return p, err
}

func (s *Service) List(ctx context.Context, sourceID, name, after, status string, limit int) ([]Proposal, error) {
	if limit < 1 || limit > 100 || !bounded(after, 131) || !validStatus(status) {
		return nil, invalid("proposal page is invalid")
	}
	var result []Proposal
	err := s.registry.WithPublication(ctx, sourceID, name, func(tx *skills.PublicationTx) error {
		rows, err := tx.SQL().QueryContext(ctx, `SELECT id FROM df_skill_proposals WHERE source_key=? AND name=? AND id>? AND (?='' OR status=?) ORDER BY id LIMIT ?`, tx.Target().SourceIdentity, name, after, status, status, limit)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				break
			}
			ids = append(ids, id)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		result = make([]Proposal, 0, len(ids))
		for _, id := range ids {
			p, err := readProposal(ctx, tx.SQL(), id)
			if err != nil {
				return err
			}
			result = append(result, p)
		}
		return nil
	})
	return result, err
}
