package evolution

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/omengye/deerflow-acp/go-harness/harness"
	"github.com/omengye/deerflow-acp/go-harness/internal/skills"
)

func validateReview(review Review) (Review, error) {
	review.Actor = strings.TrimSpace(review.Actor)
	review.Note = strings.TrimSpace(review.Note)
	if review.ExpectedStateVersion < 1 || review.Actor == "" || !bounded(review.Actor, 128) || !bounded(review.Note, 4000) {
		return review, invalid("review requires a state version, actor and bounded note")
	}
	return review, nil
}

func (s *Service) Approve(ctx context.Context, id string, review Review) (Proposal, error) {
	review, err := validateReview(review)
	if err != nil {
		return Proposal{}, err
	}
	if err = s.recordApproval(ctx, id, review); err != nil {
		return Proposal{}, err
	}
	var result Proposal
	var outcome error
	err = s.withProposal(ctx, id, func(tx *skills.PublicationTx, p *Proposal) error {
		if p.Status == "published" || p.Status == "publishing" {
			_, err := publicationByKey(ctx, tx, "approve/"+id, encodedHash(review))
			result = *p
			if err == nil && p.Status == "publishing" {
				err = tx.ResumeFilePublication()
			}
			return err
		}
		if p.StateVersion != review.ExpectedStateVersion || p.Status != "pending_review" {
			return ErrConflict
		}
		if err := s.guardDiscoveryApproval(ctx, tx, *p); err != nil {
			if errors.Is(err, errExperienceCredential) {
				p.StateVersion++
				p.Status = "failed"
				p.UpdatedAt = time.Now().UTC()
				p.Error = "The generated skill contains a credential-like literal."
				if updateErr := updateProposal(ctx, tx.SQL(), *p, review.ExpectedStateVersion); updateErr != nil {
					return updateErr
				}
				result = *p
				outcome = ErrValidation
				return nil
			}
			if errors.Is(err, errDiscoveryEvidence) {
				p.StateVersion++
				p.Status = "stale"
				p.UpdatedAt = time.Now().UTC()
				p.Error = "The discovery evidence changed or was withdrawn."
				if updateErr := updateProposal(ctx, tx.SQL(), *p, review.ExpectedStateVersion); updateErr != nil {
					return updateErr
				}
				result = *p
				outcome = ErrConflict
				return nil
			}
			return err
		}
		input := ValidationInput{Proposal: *p}
		for version, destination := range map[string]*map[string][]byte{p.Base.Version: &input.BaseFiles, p.CandidateVersion: &input.CandidateFiles} {
			if version == "" {
				continue
			}
			candidate, err := tx.ReadVersion(version)
			if err != nil {
				return err
			}
			*destination = candidate.Files()
		}
		if !validReportFor(p.Evaluation, input) {
			return ErrValidation
		}
		if err := s.guardBehaviorReport(ctx, tx.SQL(), *p, p.Evaluation); err != nil {
			return err
		}
		changed, diff, err := packageDiff(input.BaseFiles, input.CandidateFiles)
		if err != nil {
			return err
		}
		if diff != p.Diff || !slices.Equal(changed, p.ChangedFiles) {
			return errors.New("skill review diff is inconsistent with candidate")
		}
		if err := s.guardAutoPatch(ctx, tx, *p, input); err != nil {
			return err
		}
		head, err := tx.Current()
		if err != nil {
			return err
		}
		overlay, err := tx.OverlayBase()
		if err != nil {
			return err
		}
		if head != p.Base || !skills.SameOverlayBase(overlay, p.OverlayBase) {
			p.StateVersion++
			p.Status = "stale"
			p.UpdatedAt = time.Now().UTC()
			p.Error = "The reviewed base is no longer current."
			p.PublicationError = "base_changed"
			if err = updateProposal(ctx, tx.SQL(), *p, review.ExpectedStateVersion); err != nil {
				return err
			}
			result = *p
			outcome = ErrConflict
			return nil
		}
		enabled := p.Base.Enabled || p.Action == "create"
		if p.Action == "delete" {
			enabled = false
		}
		if err = ensureBaselinePublication(ctx, tx, head); err != nil {
			return err
		}
		next, err := tx.Publish(p.CandidateVersion, p.Base, enabled)
		if err != nil {
			if errors.Is(err, skills.ErrPublicationConflict) {
				p.StateVersion++
				p.Status = "stale"
				p.UpdatedAt = time.Now().UTC()
				p.Error = "Source files or an admitted publication changed the reviewed baseline."
				p.PublicationError = "publication_conflict"
				var conflict *skills.PublicationConflict
				if errors.As(err, &conflict) {
					p.PublicationError = conflict.Code
					p.Error = conflict.Error()
				}
				if e := updateProposal(ctx, tx.SQL(), *p, review.ExpectedStateVersion); e != nil {
					return e
				}
				result = *p
				outcome = ErrConflict
				return nil
			}
			return err
		}
		publication := Publication{Action: p.Action, Target: p.Target, Name: p.SkillName, ContentVersion: p.CandidateVersion, Enabled: enabled, Deleted: p.Action == "delete", Previous: head, Head: next, ProposalID: p.ID, Actor: review.Actor, Note: review.Note, CreatedAt: time.Now().UTC()}
		publication.OverlayBase = p.OverlayBase
		publication, err = insertPublication(ctx, tx, publication, "approve/"+id, encodedHash(review))
		if err != nil {
			return err
		}
		if err = tx.BindPublicationProposal(p.ID); err != nil {
			return err
		}
		if err = s.attachAutoPatchObservation(ctx, tx, publication, *p); err != nil {
			return err
		}
		p.StateVersion++
		p.Status = "publishing"
		p.FilePublicationPending = true
		p.PublishedRevision = publication.Revision
		p.UpdatedAt = publication.CreatedAt
		p.ReviewedAt = &publication.CreatedAt
		p.ReviewedBy = review.Actor
		p.ReviewNote = review.Note
		if p.Approval != nil {
			p.ReviewedAt = &p.Approval.CreatedAt
		}
		if err = updateProposal(ctx, tx.SQL(), *p, review.ExpectedStateVersion); err != nil {
			return err
		}
		result = *p
		return nil
	})
	if err == nil {
		err = outcome
	}
	if result.ID != "" && (result.Status == "publishing" || result.Status == "published") {
		fresh, e := s.Get(ctx, result.ID)
		if e == nil {
			result = fresh
		} else if err == nil {
			err = e
		}
	}
	if err != nil {
		// Read durable state, not a value from a transaction that may have rolled back.
		check, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if fresh, e := s.Get(check, id); e == nil {
			result = fresh
		}
	}
	return result, err
}

func (s *Service) Reject(ctx context.Context, id string, review Review) (Proposal, error) {
	review, err := validateReview(review)
	if err != nil {
		return Proposal{}, err
	}
	var result Proposal
	err = s.withProposal(ctx, id, func(tx *skills.PublicationTx, p *Proposal) error {
		if p.Status == "rejected" && p.StateVersion == review.ExpectedStateVersion+1 && p.ReviewedBy == review.Actor && p.ReviewNote == review.Note {
			result = *p
			return nil
		}
		if p.StateVersion != review.ExpectedStateVersion || p.Status != "pending_review" && p.Status != "validating" {
			return ErrConflict
		}
		if p.Approval != nil {
			return ErrConflict
		}
		if err := s.cancelBehaviorManual(ctx, tx.SQL(), *p); err != nil {
			return err
		}
		now := time.Now().UTC()
		p.StateVersion++
		p.Status = "rejected"
		p.ReviewedAt = &now
		p.UpdatedAt = now
		p.ReviewedBy = review.Actor
		p.ReviewNote = review.Note
		if err := updateProposal(ctx, tx.SQL(), *p, review.ExpectedStateVersion); err != nil {
			return err
		}
		result = *p
		return nil
	})
	return result, err
}

func decodePublication(data []byte, target skills.PublicationTarget, revision int64) (Publication, error) {
	var p Publication
	if json.Unmarshal(data, &p) != nil || p.Target != target || p.Name != target.Name || p.Revision != revision || p.Head.Version != p.ContentVersion || p.Head.Enabled != p.Enabled || p.Head.Exists == p.Deleted || p.Head.Exists != (p.ContentVersion != "") || p.Head.StateVersion < 1 {
		return p, errors.New("skill publication record is inconsistent")
	}
	if !validOverlayBase(p.OverlayBase, target) {
		return p, errors.New("skill publication overlay identity is inconsistent")
	}
	if p.Action == "baseline" {
		if revision != 1 || p.ProposalID != "" || p.RollbackOf != 0 || p.Previous != (skills.SkillHead{}) {
			return p, errors.New("skill baseline record is inconsistent")
		}
	} else if !p.Head.Managed || p.Head.StateVersion != p.Previous.StateVersion+1 {
		return p, errors.New("skill publication transition is inconsistent")
	}
	return p, nil
}

// Capture an imported head before the first evolution publication so even the
// first update can be rolled back through an ordinary integer revision.
func ensureBaselinePublication(ctx context.Context, tx *skills.PublicationTx, head skills.SkillHead) error {
	if head.StateVersion == 0 {
		return nil
	}
	var exists bool
	if err := tx.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM df_skill_publications WHERE source_key=? AND name=?)`, tx.Target().SourceIdentity, tx.Target().Name).Scan(&exists); err != nil || exists {
		return err
	}
	_, err := insertPublication(ctx, tx, Publication{Action: "baseline", Target: tx.Target(), Name: tx.Target().Name, ContentVersion: head.Version, Enabled: head.Enabled, Deleted: !head.Exists, Head: head, Actor: "captured_base", CreatedAt: time.Now().UTC()}, "baseline", encodedHash(head))
	return err
}

func readPublication(ctx context.Context, tx *skills.PublicationTx, revision int64) (Publication, error) {
	var data []byte
	err := tx.SQL().QueryRowContext(ctx, `SELECT payload FROM df_skill_publications WHERE source_key=? AND name=? AND revision=?`, tx.Target().SourceIdentity, tx.Target().Name, revision).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return Publication{}, harness.ErrNotFound
	}
	if err != nil {
		return Publication{}, err
	}
	return decodePublication(data, tx.Target(), revision)
}

func publicationByKey(ctx context.Context, tx *skills.PublicationTx, key, expected string) (Publication, error) {
	var data []byte
	var sha string
	var revision int64
	err := tx.SQL().QueryRowContext(ctx, `SELECT request_sha,revision,payload FROM df_skill_publications WHERE source_key=? AND name=? AND request_key=?`, tx.Target().SourceIdentity, tx.Target().Name, key).Scan(&sha, &revision, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return Publication{}, harness.ErrNotFound
	}
	if err != nil {
		return Publication{}, err
	}
	if sha != expected {
		return Publication{}, ErrConflict
	}
	return decodePublication(data, tx.Target(), revision)
}

func insertPublication(ctx context.Context, tx *skills.PublicationTx, p Publication, key, sha string) (Publication, error) {
	if err := tx.SQL().QueryRowContext(ctx, `SELECT COALESCE(max(revision),0)+1 FROM df_skill_publications WHERE source_key=? AND name=?`, p.Target.SourceIdentity, p.Name).Scan(&p.Revision); err != nil {
		return p, err
	}
	data, err := json.Marshal(p)
	if err != nil {
		return p, err
	}
	_, err = tx.SQL().ExecContext(ctx, `INSERT INTO df_skill_publications VALUES(?,?,?,?,?,?)`, p.Target.SourceIdentity, p.Name, p.Revision, key, sha, data)
	return p, err
}

func (s *Service) Rollback(ctx context.Context, request Rollback) (Publication, error) {
	if request.Revision < 1 || request.RequestKey == "" || !bounded(request.RequestKey, 256) {
		return Publication{}, invalid("rollback requires a revision and request key")
	}
	review, err := validateReview(Review{ExpectedStateVersion: request.Expected.StateVersion, Actor: request.Actor, Note: request.Note})
	if err != nil {
		return Publication{}, err
	}
	request.Actor, request.Note = review.Actor, review.Note
	var result Publication
	err = s.registry.WithPublication(ctx, request.SourceID, request.Name, func(tx *skills.PublicationTx) error {
		prior, err := publicationByKey(ctx, tx, "rollback/"+request.RequestKey, encodedHash(request))
		if err == nil {
			result = prior
			status, e := skills.FilePublicationStatus(ctx, tx.SQL(), prior.Target.SourceIdentity, prior.Name, prior.Head.StateVersion)
			if e != nil {
				return e
			}
			if status == "aborted" {
				return ErrConflict
			}
			if status != "pending" {
				return nil
			}
			return tx.ResumeFilePublication()
		}
		if !errors.Is(err, harness.ErrNotFound) {
			return err
		}
		target, err := readPublication(ctx, tx, request.Revision)
		if err != nil {
			return err
		}
		status, err := skills.FilePublicationStatus(ctx, tx.SQL(), target.Target.SourceIdentity, target.Name, target.Head.StateVersion)
		if err != nil {
			return err
		}
		if status == "pending" || status == "aborted" {
			return ErrConflict
		}
		current, err := tx.Current()
		if err != nil {
			return err
		}
		if current != request.Expected {
			return ErrConflict
		}
		next, err := tx.Publish(target.ContentVersion, current, target.Enabled)
		if err != nil {
			return err
		}
		result = Publication{Action: "rollback", Target: tx.Target(), Name: request.Name, ContentVersion: target.ContentVersion, Enabled: target.Enabled, Deleted: target.Deleted, Previous: current, Head: next, Actor: request.Actor, Note: request.Note, RollbackOf: target.Revision, CreatedAt: time.Now().UTC()}
		result.OverlayBase, err = tx.OverlayBase()
		if err != nil {
			return err
		}
		result, err = insertPublication(ctx, tx, result, "rollback/"+request.RequestKey, encodedHash(request))
		return err
	})
	if result.Revision > 0 {
		status, e := skills.FilePublicationStatus(ctx, s.db, result.Target.SourceIdentity, result.Name, result.Head.StateVersion)
		if e == nil {
			result.FileStatus = status
		} else if err == nil {
			err = e
		}
	}
	return result, err
}

func (s *Service) History(ctx context.Context, sourceID, name string, after int64, limit int) ([]Publication, error) {
	if after < 0 || limit < 1 || limit > 100 {
		return nil, invalid("publication page is invalid")
	}
	var result []Publication
	err := s.registry.WithPublication(ctx, sourceID, name, func(tx *skills.PublicationTx) error {
		rows, err := tx.SQL().QueryContext(ctx, `SELECT revision,payload FROM df_skill_publications WHERE source_key=? AND name=? AND revision>? ORDER BY revision LIMIT ?`, tx.Target().SourceIdentity, name, after, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		result = make([]Publication, 0)
		for rows.Next() {
			var revision int64
			var data []byte
			if err = rows.Scan(&revision, &data); err != nil {
				return err
			}
			p, err := decodePublication(data, tx.Target(), revision)
			if err != nil {
				return err
			}
			result = append(result, p)
		}
		if err = rows.Err(); err != nil {
			return err
		}
		if err = rows.Close(); err != nil {
			return err
		}
		for i := range result {
			result[i].FileStatus, err = skills.FilePublicationStatus(ctx, tx.SQL(), result[i].Target.SourceIdentity, result[i].Name, result[i].Head.StateVersion)
			if err != nil {
				return err
			}
		}
		return nil
	})
	return result, err
}
